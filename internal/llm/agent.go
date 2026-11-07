package llm

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"

	"charm.land/fantasy"
	jsonschema "github.com/invopop/jsonschema"
)

const nudgeToolName = "__roast_continue"

type agentRunState struct {
	opts        ConverseOpts
	model       string
	wd          *workDeadlineState
	turn        int
	finalTurn   bool
	cutoff      error
	noticeSent  bool
	inputWarn   bool
	finalDone   bool
	finalErr    error
	structured  bool
	out         any
	active      []string
	toolNames   map[string]bool
	toolSchemas map[string]map[string]any

	// Count all final-phase responses, even ones Fantasy rejects before dispatch.
	submissionAttempts  int
	validationExhausted bool
	budgetOnce          sync.Once

	// Signed reasoning (Claude 5.1 and later) is bound to the system prompt,
	// the tool set, and every earlier message, and the provider rejects a
	// replay whose prefix changed. Fantasy rebuilds each step from its own
	// history, which never includes messages Roast adds, so each added message
	// is pinned after the Fantasy messages that preceded it when first sent.
	base     fantasy.Prompt
	injected []injectedMessage

	// Dispatch policy for the response being processed, decided once it is
	// complete. Corrections travel as tool results so the replayed assistant
	// turn stays exactly what the model produced.
	blockExploration string
	rejectFinal      string
	acceptFinal      *fantasy.ToolCallContent
	// stopCause says why the latest response produced no submission.
	stopCause string

	// recordTools only save part of the result; they run even on the wrap-up
	// turn. recordRounds counts wrap-up responses that only recorded: they
	// never use submission attempts, and past their own allowance the run
	// stops after recording.
	recordTools  map[string]bool
	recordRounds int
	// halt ends the loop once the current response's tools have run.
	halt bool
}

type injectedMessage struct {
	at      int
	message fantasy.Message
}

func (s *agentRunState) inject(message fantasy.Message) {
	s.injected = append(s.injected, injectedMessage{at: len(s.base), message: message})
}

// messages returns this step's conversation: Fantasy's history with every
// message Roast added restored at its original position.
func (s *agentRunState) messages() fantasy.Prompt {
	out := make(fantasy.Prompt, 0, len(s.base)+len(s.injected))
	next := 0
	for i, message := range s.base {
		for ; next < len(s.injected) && s.injected[next].at <= i; next++ {
			out = append(out, s.injected[next].message)
		}
		out = append(out, message)
	}
	for ; next < len(s.injected); next++ {
		out = append(out, s.injected[next].message)
	}
	return out
}

func (s *agentRunState) notifyBudget() {
	s.budgetOnce.Do(func() {
		if s.opts.OnBudgetExhausted != nil {
			s.opts.OnBudgetExhausted()
		}
	})
}

func (s *agentRunState) forceFinal(cause error) {
	s.finalTurn = true
	if cause != nil && s.cutoff == nil {
		s.cutoff = cause
	}
}

// deadlinePassedDuringFinalization ends a final phase that is already under
// way once the work deadline has passed, including while the last response's
// tools ran: the wrap-up window is spent, so no further call opens.
func (s *agentRunState) deadlinePassedDuringFinalization() bool {
	if !s.finalTurn || !s.wd.check() {
		return false
	}
	s.forceFinal(ErrWorkDeadline)
	if s.stopCause == "" {
		s.stopCause = "the work deadline passed before `" + s.opts.FinalTool + "` was called"
	}
	return true
}

func (s *agentRunState) notice() string {
	switch {
	case errors.Is(s.cutoff, ErrBudgetExhausted):
		return "TOKEN BUDGET EXHAUSTED — call `" + s.opts.FinalTool + "` NOW with everything you have; partial results beat none."
	case errors.Is(s.cutoff, ErrWorkDeadline):
		return s.wd.wrapUpNotice(s.opts.FinalTool)
	default:
		return "Exploration budget exhausted — call `" + s.opts.FinalTool + "` NOW with everything you have; partial results beat none."
	}
}

func (s *agentRunState) cutoffError(what string, cause error) error {
	sentinel := s.cutoff
	if sentinel == nil {
		sentinel = ErrNoFinalCall
	}
	if cause != nil {
		return fmt.Errorf("%w; %s: %w: %v", sentinel, what, ErrNoFinalCall, cause)
	}
	return fmt.Errorf("%w; %s: %w", sentinel, what, ErrNoFinalCall)
}

func (s *agentRunState) prepareStep(ctx context.Context, opts fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
	s.turn++
	s.base = opts.Messages
	ceiling := s.opts.InputByteCeiling
	if ceiling <= 0 {
		ceiling = defaultInputByteCeiling
	}
	if s.turn > 1 && !s.inputWarn && fantasyConversationBytes(s.messages()) > ceiling {
		s.inputWarn = true
		s.inject(fantasy.NewUserMessage(
			"INPUT BUDGET EXHAUSTED. Do not request more files. Call `" + s.opts.FinalTool + "` now with what you have — partial results beat none.",
		))
	}
	if s.wd.check() {
		s.forceFinal(ErrWorkDeadline)
	}
	if s.turn >= normalizedMaxTurns(s.opts.MaxTurns) {
		s.forceFinal(nil)
	}
	if s.finalTurn && !s.noticeSent {
		s.noticeSent = true
		notice := s.notice()
		if s.structured {
			notice = "Return the requested structured result via the `" + s.opts.FinalTool + "` tool call. Do not answer in prose."
		}
		s.inject(fantasy.NewUserMessage(notice))
	}
	// The tool set is part of the signed-reasoning prefix too, so it never
	// narrows for finalization; tool results enforce the final phase instead.
	return ctx, fantasy.PrepareStepResult{Messages: s.messages(), ActiveTools: s.active}, nil
}

type managedModel struct {
	client *client
	state  *agentRunState
}

func (m *managedModel) Provider() string { return m.client.model.Provider() }
func (m *managedModel) Model() string    { return m.client.model.Model() }

func (m *managedModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("roast agent runtime requires streaming")
}

func (m *managedModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	// Fantasy's AgentTool API keeps only properties and required. Restore the
	// complete caller-supplied JSON Schema at the model boundary so providers
	// that accept it retain top-level constraints such as additionalProperties.
	// Fantasy's Anthropic adapter currently keeps type, properties, and required
	// but discards other top-level keywords, so this restoration is best-effort
	// for providers that accept the complete schema.
	for i, tool := range call.Tools {
		functionTool, ok := tool.(fantasy.FunctionTool)
		if !ok {
			continue
		}
		if schema := m.state.toolSchemas[functionTool.Name]; schema != nil {
			functionTool.InputSchema = cloneSchema(schema)
			call.Tools[i] = functionTool
		}
	}
	final := m.state.finalTurn
	// Never force a tool choice: thinking-mode providers (DeepSeek, Claude
	// with thinking) reject it. Finalization relies on the notice, schema
	// validation, and bounded reminders instead.
	choice := fantasy.ToolChoiceAuto
	call.ToolChoice = &choice
	return func(yield func(fantasy.StreamPart) bool) {
		toolJSON, _ := json.Marshal(call.Tools)
		release, err := m.client.cfg.Ledger.Reserve(fantasyConversationBytes(call.Prompt)+len(toolJSON)+2048, m.client.cfg.MaxOutputTokens, final)
		if err != nil {
			if final {
				if m.state.cutoff == nil {
					m.state.forceFinal(ErrBudgetExhausted)
					m.state.notifyBudget()
				}
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: m.state.cutoffError("no headroom for the wrap-up call", err)})
				return
			}
			m.state.forceFinal(ErrBudgetExhausted)
			m.state.notifyBudget()
			m.streamFinal(ctx, call, yield)
			return
		}

		err = m.streamCall(ctx, call, release, yield)
		deadlineReached := m.state.wd.check()
		if err == nil || errors.Is(err, errStreamStopped) {
			return
		}
		var captureErr *captureError
		if errors.As(err, &captureErr) {
			err = permanentModelError{err: err}
		} else if !final && deadlineReached && ctx.Err() == nil {
			m.state.forceFinal(ErrWorkDeadline)
			m.streamFinal(ctx, call, yield)
			return
		} else if final && m.state.cutoff != nil {
			err = m.state.cutoffError("wrap-up call failed", normalizeProviderError(ctx, m.client.cfg.Model, err))
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: err})
	}, nil
}

var errStreamStopped = errors.New("stream consumer stopped")

func (m *managedModel) streamFinal(ctx context.Context, call fantasy.Call, yield func(fantasy.StreamPart) bool) {
	stream, err := m.Stream(ctx, m.finalCall(call, m.state.notice()))
	if err != nil {
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: err})
		return
	}
	for part := range stream {
		if !yield(part) {
			return
		}
	}
}

func (m *managedModel) streamCall(ctx context.Context, call fantasy.Call, release func(Usage), yield func(fantasy.StreamPart) bool) (callErr error) {
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	callCtx, workCancel := m.state.wd.callCtx(callCtx)
	defer workCancel()
	if m.client.cfg.RequestTimeout > 0 {
		var deadlineCancel context.CancelFunc
		callCtx, deadlineCancel = context.WithTimeout(callCtx, m.client.cfg.RequestTimeout)
		defer deadlineCancel()
	}
	body := &responseBody{}
	callCtx = context.WithValue(callCtx, responseBodyKey{}, body)
	var finish *fantasy.StreamPart
	defer func() {
		usage := Usage{}
		if finish != nil {
			usage = m.client.addUsage(finish.Usage)
		}
		release(usage)
	}()
	defer func() {
		cancel()
		_ = body.close()
		if err := m.client.transport.failure(); err != nil {
			callErr = err
		}
		if body.failure != nil {
			callErr = body.failure
		}
	}()
	m.state.blockExploration, m.state.rejectFinal, m.state.acceptFinal = "", "", nil
	stream, err := m.client.model.Stream(callCtx, call)
	if err != nil {
		return err
	}
	// From the first tool call on, hold every part until the response is
	// complete. Releasing them in arrival order keeps the replayed turn
	// identical to what the provider sent (a thinking block after a tool call
	// is signed over that call), and nothing is dispatched before capture.
	var held []fantasy.StreamPart
	var calls []fantasy.ToolCallContent
	// Fantasy maps provider finish reasons to a few categories. Some adapters
	// (chat, as of v0.46) also keep the provider's own string, but only on the
	// warning they attach to an abnormal finish.
	var rawFinish string
	for part := range stream {
		if part.Type == fantasy.StreamPartTypeWarnings {
			for _, warning := range part.Warnings {
				if warning.RawFinishReason != "" {
					rawFinish = warning.RawFinishReason
				}
			}
		}
		switch part.Type {
		case fantasy.StreamPartTypeError:
			return part.Error
		case fantasy.StreamPartTypeFinish:
			finish = &part
		case fantasy.StreamPartTypeToolCall:
			calls = append(calls, fantasy.ToolCallContent{ToolCallID: part.ID, ToolName: part.ToolCallName, Input: part.ToolCallInput})
			held = append(held, part)
		default:
			if len(held) > 0 {
				held = append(held, part)
				continue
			}
			if !yield(part) {
				return errStreamStopped
			}
		}
	}
	// Capture must succeed before Fantasy is allowed to dispatch any tool, even
	// if the SDK stopped at a terminal event without reading through EOF.
	_ = body.close()
	if err := m.client.transport.failure(); err != nil {
		return err
	}
	// Some SDKs stop at a terminal SSE event without requesting another body
	// read. A wire guard can already have rejected that event; never postpone
	// its failure until deferred cleanup after buffered tools are released.
	if body.failure != nil {
		return body.failure
	}
	if err := callCtx.Err(); err != nil {
		return err
	}
	if finish == nil {
		return fantasy.NewIncompleteStreamError()
	}
	reason := finish.FinishReason
	var appended []fantasy.ToolCallContent
	// Let Fantasy reject incomplete/truncated/filtered turns. Rewriting their
	// finish reason to tool_calls could execute a partial tool invocation.
	if reason == fantasy.FinishReasonToolCalls || reason == fantasy.FinishReasonStop {
		appended, reason = m.reviewResponse(calls)
	} else {
		m.state.stopCause = fmt.Sprintf("the response ended with finish reason %q", reason)
		if rawFinish != "" && rawFinish != string(reason) {
			m.state.stopCause += fmt.Sprintf(" (the provider sent %q)", rawFinish)
		}
		if reason == fantasy.FinishReasonLength {
			m.state.stopCause += "; raise max_output_tokens if the output was cut off"
		}
	}
	for _, part := range held {
		if !yield(part) {
			return errStreamStopped
		}
	}
	for _, tool := range appended {
		if !yield(fantasy.StreamPart{
			Type: fantasy.StreamPartTypeToolCall, ID: tool.ToolCallID,
			ToolCallName: tool.ToolName, ToolCallInput: tool.Input,
		}) {
			return errStreamStopped
		}
	}
	finish.FinishReason = reason
	if !yield(*finish) {
		return errStreamStopped
	}
	return nil
}

type permanentModelError struct{ err error }

func (e permanentModelError) Error() string { return e.err.Error() }

func (m *managedModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("roast agent runtime does not generate objects directly")
}

func (m *managedModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("roast agent runtime does not stream objects")
}

// finalCall turns this step's call into the wrap-up call. The notice joins
// the conversation for good (a Fantasy retry of this step resends it, and
// later steps keep it), and the tool set is left as sent.
func (m *managedModel) finalCall(call fantasy.Call, notice string) fantasy.Call {
	m.state.finalTurn = true
	if !m.state.noticeSent {
		m.state.noticeSent = true
		m.state.inject(fantasy.NewUserMessage(notice))
	}
	call.Prompt = m.state.messages()
	return call
}

// mayContinueFinalization reports whether finalization may make another call.
// Never reopen exploration or start another wrap-up window after a cutoff.
// Every reminder still goes through the normal token admission and deadlines.
func (m *managedModel) mayContinueFinalization() bool {
	if m.state.wd.check() {
		m.state.forceFinal(ErrWorkDeadline)
	}
	return m.state.cutoff == nil
}

func (m *managedModel) canRetrySubmission() bool {
	return m.mayContinueFinalization() && m.state.submissionAttempts < maxAttempts(m.client.cfg)
}

// reviewResponse decides how a complete response is dispatched. It never
// edits, drops, or reorders what the model produced: a later signed thinking
// block may be bound to any of it. It returns synthetic nudge calls to append
// after the model's content, and the finish reason to report.
func (m *managedModel) reviewResponse(calls []fantasy.ToolCallContent) ([]fantasy.ToolCallContent, fantasy.FinishReason) {
	s := m.state
	s.stopCause = ""
	var final *fantasy.ToolCallContent
	records := 0
	for i, call := range calls {
		switch {
		case call.ToolName == s.opts.FinalTool:
			final = &calls[i]
		case s.recordTools[call.ToolName]:
			records++
		case !s.toolNames[call.ToolName] && s.opts.OnTurn != nil:
			// Fantasy answers unknown tools with an error result; report them
			// as turns because nothing else will.
			s.opts.OnTurn(TurnInfo{Turn: s.turn, Tool: call.ToolName, Args: decodeArgs(call.Input)})
		}
	}

	if s.finalTurn && final == nil && records > 0 && records == len(calls) {
		// Recording results on the wrap-up turn is never a failed submission.
		// Run the calls; hand the turn back only within the recording
		// allowance and never past a cutoff, otherwise stop after them.
		s.recordRounds++
		switch {
		case !m.mayContinueFinalization():
			s.halt = true
			s.stopCause = "the model recorded results but did not call `" + s.opts.FinalTool + "` before the cutoff"
		case s.recordRounds > maxAttempts(m.client.cfg):
			s.halt = true
			s.stopCause = fmt.Sprintf("the model kept recording results without calling `%s` (%d recording rounds)", s.opts.FinalTool, s.recordRounds)
		}
		return nil, fantasy.FinishReasonToolCalls
	}
	if s.finalTurn {
		s.submissionAttempts++
	}
	if final != nil {
		s.blockExploration = "Not executed: this response also called `" + s.opts.FinalTool + "`, which ends the run."
		validationErr := validateFinalArguments(final.Input, s.out)
		if validationErr != nil && s.finalTurn && m.canRetrySubmission() {
			// This failure is provisional. Do not let it mask a provider error
			// from the follow-up call.
			s.finalErr = nil
			s.rejectFinal = fmt.Sprintf("your previous response was not valid JSON matching the schema: %v. Call `%s` again with valid arguments.", validationErr, s.opts.FinalTool)
			s.stopCause = "the last `" + s.opts.FinalTool + "` call did not match the schema"
			return nil, fantasy.FinishReasonToolCalls
		}
		s.finalErr = validationErr
		if validationErr != nil {
			s.validationExhausted = true
		}
		accepted := *final
		s.acceptFinal = &accepted
		// Fantasy still validates required fields before dispatch; if it
		// rejects the call, the run can end here without a submission.
		s.stopCause = "the last `" + s.opts.FinalTool + "` call did not pass tool-call validation"
		return nil, fantasy.FinishReasonToolCalls
	}

	if s.finalTurn {
		// Auto tool selection can produce prose or other tool calls. Only a
		// validated final submission may finish successfully; never dispatch
		// exploration tools, even ones Fantasy still knows how to execute.
		// Recording tools still run.
		s.blockExploration = "Not executed: exploration is finished."
		if m.canRetrySubmission() {
			return []fantasy.ToolCallContent{nudgeCall(fmt.Sprintf("roast-final-%d", s.submissionAttempts),
				"Exploration is finished. Return the result via the `"+s.opts.FinalTool+"` tool call, not prose or another tool.")}, fantasy.FinishReasonToolCalls
		}
		s.stopCause = "the model answered without calling `" + s.opts.FinalTool + "`"
		if len(calls) > 0 {
			s.stopCause = "the model called other tools instead of `" + s.opts.FinalTool + "`"
		}
		attempts := "attempts"
		if s.submissionAttempts == 1 {
			attempts = "attempt"
		}
		s.stopCause += fmt.Sprintf(" (%d submission %s)", s.submissionAttempts, attempts)
		if records > 0 {
			// Keep what the model recorded: dispatch, then stop.
			s.halt = true
			return nil, fantasy.FinishReasonToolCalls
		}
		return nil, fantasy.FinishReasonStop
	}

	if len(calls) > 0 {
		return nil, fantasy.FinishReasonToolCalls
	}
	return []fantasy.ToolCallContent{nudgeCall(fmt.Sprintf("roast-nudge-%d", s.turn),
		"Results must be delivered via the `"+s.opts.FinalTool+"` tool call. If you have enough context call it now; otherwise call an exploration tool.")}, fantasy.FinishReasonToolCalls
}

func (c *client) CompleteStructured(ctx context.Context, system, user string, schemaName string, out any) error {
	return runAgent(ctx, c, system, user, ConverseOpts{
		Tools:     []Tool{{Name: schemaName, Description: "Return the requested structured result.", Schema: reflectSchema(out)}},
		FinalTool: schemaName,
		MaxTurns:  1,
	}, out, true)
}

func (c *client) Converse(ctx context.Context, system, user string, opts ConverseOpts, out any) error {
	return runAgent(ctx, c, system, user, opts, out, false)
}

func runAgent(ctx context.Context, c *client, system, user string, opts ConverseOpts, out any, structured bool) error {
	if findTool(opts.Tools, opts.FinalTool) == nil {
		return fmt.Errorf("final tool %q is not configured", opts.FinalTool)
	}
	state := &agentRunState{
		opts: opts, model: c.cfg.Model, wd: newWorkDeadlineState(opts), structured: structured, out: out,
		toolNames: make(map[string]bool, len(opts.Tools)+1), toolSchemas: make(map[string]map[string]any, len(opts.Tools)),
		recordTools: make(map[string]bool),
	}
	if structured {
		state.finalTurn = true
	}

	tools := make([]fantasy.AgentTool, 0, len(opts.Tools)+1)
	for i := range opts.Tools {
		info, schema, err := fantasyToolInfo(opts.Tools[i])
		if err != nil {
			return fmt.Errorf("tool %s schema: %w", opts.Tools[i].Name, err)
		}
		state.toolSchemas[opts.Tools[i].Name] = schema
		state.toolNames[opts.Tools[i].Name] = true
		state.active = append(state.active, opts.Tools[i].Name)
		if opts.Tools[i].Name == opts.FinalTool {
			tools = append(tools, finalFantasyTool(info, state))
			continue
		}
		if opts.Tools[i].RecordsResult {
			state.recordTools[opts.Tools[i].Name] = true
		}
		tools = append(tools, explorationFantasyTool(info, opts.Tools[i], state))
	}
	state.toolNames[nudgeToolName] = true
	tools = append(tools, nudgeFantasyTool())

	managed := &managedModel{client: c, state: state}
	agent := fantasy.NewAgent(managed,
		fantasy.WithSystemPrompt(system),
		fantasy.WithMaxOutputTokens(int64(c.cfg.MaxOutputTokens)),
		fantasy.WithTools(tools...),
		fantasy.WithMaxRetries(maxAttempts(c.cfg)-1),
		fantasy.WithPrepareStep(state.prepareStep),
		fantasy.WithRepairToolCall(func(context.Context, fantasy.ToolCallRepairOptions) (*fantasy.ToolCallContent, error) {
			return nil, errors.New("tool-call repair disabled")
		}),
		fantasy.WithStopConditions(func([]fantasy.StepResult) bool {
			return state.finalDone || state.validationExhausted || state.halt ||
				(state.finalTurn && state.submissionAttempts >= maxAttempts(c.cfg)) ||
				state.deadlinePassedDuringFinalization()
		}),
	)

	ctx = context.WithValue(ctx, sessionContextKey{}, "roast-"+rand.Text())
	_, runErr := agent.Stream(ctx, fantasy.AgentStreamCall{Prompt: user})
	var permanent permanentModelError
	if errors.As(runErr, &permanent) {
		// permanentModelError is intentionally opaque while Fantasy classifies
		// retries. Restore its cause once that layer has finished.
		runErr = permanent.err
	}
	if state.finalDone {
		return nil
	}
	if state.finalErr != nil {
		err := fmt.Errorf("%s: %s arguments did not satisfy the schema (%v); if truncated, raise max_output_tokens", state.model, opts.FinalTool, state.finalErr)
		if state.cutoff != nil {
			return state.cutoffError("wrap-up submission was invalid", err)
		}
		return err
	}
	if runErr != nil {
		if errors.Is(runErr, ErrNoFinalCall) {
			// The wrap-up call already explained itself, provider error included.
			return runErr
		}
		if state.cutoff != nil {
			return state.cutoffError("wrap-up turn failed", runErr)
		}
		return normalizeProviderError(ctx, c.cfg.Model, runErr)
	}
	var cause error
	if state.stopCause != "" {
		cause = fmt.Errorf("%s: %s", state.model, state.stopCause)
	}
	if state.cutoff != nil {
		return state.cutoffError("wrap-up turn produced no submission", cause)
	}
	if cause != nil {
		return fmt.Errorf("%w: %v", ErrNoFinalCall, cause)
	}
	return ErrNoFinalCall
}

type roastFantasyTool struct {
	info fantasy.ToolInfo
	run  func(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error)
	opts fantasy.ProviderOptions
}

func (t *roastFantasyTool) Info() fantasy.ToolInfo { return t.info }
func (t *roastFantasyTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return t.run(ctx, call)
}
func (t *roastFantasyTool) ProviderOptions() fantasy.ProviderOptions        { return t.opts }
func (t *roastFantasyTool) SetProviderOptions(opts fantasy.ProviderOptions) { t.opts = opts }

func explorationFantasyTool(info fantasy.ToolInfo, definition Tool, state *agentRunState) fantasy.AgentTool {
	return &roastFantasyTool{info: info, run: func(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
		if state.blockExploration != "" && !definition.RecordsResult {
			return fantasy.NewTextErrorResponse(state.blockExploration), nil
		}
		args := decodeArgs(call.Input)
		var result any
		var err error
		if definition.Run == nil {
			err = errors.New("tool has no implementation")
		} else {
			result, err = state.wd.runTool(ctx, &definition, args)
		}
		if err != nil {
			result = map[string]any{"error": err.Error()}
		}
		if state.opts.OnTurn != nil {
			state.opts.OnTurn(TurnInfo{Turn: state.turn, Tool: definition.Name, Args: args})
		}
		data, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			data, _ = json.Marshal(map[string]any{"error": marshalErr.Error()})
		}
		return fantasy.NewTextResponse(string(data)), nil
	}}
}

func finalFantasyTool(info fantasy.ToolInfo, state *agentRunState) fantasy.AgentTool {
	return &roastFantasyTool{info: info, run: func(_ context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
		// Fantasy may repair malformed JSON locally. A final submission that
		// Roast already rejected must not become successful after that repair.
		if state.finalErr != nil {
			resp := fantasy.NewTextErrorResponse(state.finalErr.Error())
			resp.StopTurn = true
			return resp, nil
		}
		if state.rejectFinal != "" {
			return fantasy.NewTextErrorResponse(state.rejectFinal), nil
		}
		if state.finalDone {
			resp := fantasy.NewTextResponse("Already submitted.")
			resp.StopTurn = true
			return resp, nil
		}
		// A response may carry several submissions; only the one that was
		// validated is accepted.
		if accepted := state.acceptFinal; accepted == nil || accepted.ToolCallID != call.ID || accepted.Input != call.Input {
			return fantasy.NewTextErrorResponse("Not accepted: call `" + info.Name + "` once per response."), nil
		}
		resp := fantasy.NewTextResponse(call.Input)
		resp.StopTurn = true
		if err := json.Unmarshal([]byte(call.Input), state.out); err != nil {
			state.finalErr = err
			resp = fantasy.NewTextErrorResponse(err.Error())
			resp.StopTurn = true
			return resp, nil
		}
		state.finalErr = nil
		state.finalDone = true
		return resp, nil
	}}
}

func nudgeFantasyTool() fantasy.AgentTool {
	return &roastFantasyTool{
		info: fantasy.ToolInfo{
			Name: nudgeToolName, Description: "Continue the Roast tool loop after a free-form response.",
			Parameters: map[string]any{"message": map[string]any{"type": "string"}}, Required: []string{"message"},
		},
		run: func(_ context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			var payload struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal([]byte(call.Input), &payload)
			return fantasy.NewTextResponse(payload.Message), nil
		},
	}
}

func nudgeCall(id, message string) fantasy.ToolCallContent {
	data, _ := json.Marshal(map[string]string{"message": message})
	return fantasy.ToolCallContent{ToolCallID: id, ToolName: nudgeToolName, Input: string(data)}
}

func decodeArgs(arguments string) map[string]any {
	var args map[string]any
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return map[string]any{"error": fmt.Sprintf("args parse error: %v", err)}
	}
	return args
}

func fantasyToolInfo(t Tool) (fantasy.ToolInfo, map[string]any, error) {
	data, err := json.Marshal(t.Schema)
	if err != nil {
		return fantasy.ToolInfo{}, nil, err
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		return fantasy.ToolInfo{}, nil, err
	}
	parameters, _ := schema["properties"].(map[string]any)
	if parameters == nil {
		parameters = map[string]any{}
	}
	var required []string
	if raw, ok := schema["required"].([]any); ok {
		for _, item := range raw {
			if name, ok := item.(string); ok {
				required = append(required, name)
			}
		}
	}
	return fantasy.ToolInfo{Name: t.Name, Description: t.Description, Parameters: parameters, Required: required}, schema, nil
}

// cloneSchema returns a deep copy of schema for one request. Fantasy's
// Anthropic adapter reads a required list only as []string and sends an empty
// one for any other shape, so every required list the JSON round trip decoded
// as []any is restored to []string.
func cloneSchema(schema map[string]any) map[string]any {
	data, _ := json.Marshal(schema)
	var clone map[string]any
	_ = json.Unmarshal(data, &clone)
	restoreRequiredLists(clone)
	return clone
}

func restoreRequiredLists(node any) {
	switch node := node.(type) {
	case map[string]any:
		for key, child := range node {
			if list, ok := child.([]any); ok && key == "required" {
				if names, ok := stringList(list); ok {
					node[key] = names
					continue
				}
			}
			restoreRequiredLists(child)
		}
	case []any:
		for _, item := range node {
			restoreRequiredLists(item)
		}
	}
}

func stringList(items []any) ([]string, bool) {
	names := make([]string, 0, len(items))
	for _, item := range items {
		name, ok := item.(string)
		if !ok {
			return nil, false
		}
		names = append(names, name)
	}
	return names, true
}

func reflectSchema(v any) map[string]any {
	r := &jsonschema.Reflector{Anonymous: true, AllowAdditionalProperties: false, DoNotReference: true}
	s := r.Reflect(v)
	s.Version = ""
	data, _ := json.Marshal(s)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return out
}

func validateFinalArguments(arguments string, out any) error {
	data := json.RawMessage(arguments)
	if !json.Valid(data) {
		return errors.New("invalid JSON")
	}
	t := reflect.TypeOf(out)
	if t == nil || t.Kind() != reflect.Pointer {
		return errors.New("output must be a pointer")
	}
	scratch := reflect.New(t.Elem()).Interface()
	return json.Unmarshal(data, scratch)
}

func normalizedMaxTurns(turns int) int {
	if turns <= 0 {
		return 8
	}
	return turns
}

func findTool(tools []Tool, name string) *Tool {
	for i := range tools {
		if tools[i].Name == name {
			return &tools[i]
		}
	}
	return nil
}

func fantasyConversationBytes(messages fantasy.Prompt) int {
	data, _ := json.Marshal(messages)
	return len(data)
}
