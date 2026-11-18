package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/bluesky-social/roast/internal/testutil"
)

type fakeLanguageModel struct {
	generate func(context.Context, fantasy.Call) (*fantasy.Response, error)
	stream   func(context.Context, fantasy.Call) (fantasy.StreamResponse, error)
}

func (m *fakeLanguageModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	return m.generate(ctx, call)
}

func (m *fakeLanguageModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if m.stream != nil {
		return m.stream(ctx, call)
	}
	return func(yield func(fantasy.StreamPart) bool) {
		response, err := m.generate(ctx, call)
		if err != nil {
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: err})
			return
		}
		for _, item := range response.Content {
			switch item := item.(type) {
			case fantasy.ToolCallContent:
				if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: item.ToolCallID, ToolCallName: item.ToolName, ToolCallInput: item.Input}) {
					return
				}
			case fantasy.TextContent:
				for _, part := range []fantasy.StreamPart{
					{Type: fantasy.StreamPartTypeTextStart, ID: "text"},
					{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: item.Text},
					{Type: fantasy.StreamPartTypeTextEnd, ID: "text"},
				} {
					if !yield(part) {
						return
					}
				}
			}
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: response.FinishReason, Usage: response.Usage})
	}, nil
}

func (*fakeLanguageModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (*fakeLanguageModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

func (*fakeLanguageModel) Provider() string { return "test" }
func (*fakeLanguageModel) Model() string    { return "test-model" }

func testClient(fn func(context.Context, fantasy.Call) (*fantasy.Response, error)) *client {
	return &client{cfg: ClientConfig{Model: "test-model", MaxOutputTokens: 100, MaxAttempts: 1}, model: &fakeLanguageModel{generate: fn}}
}

func toolCallResponse(name, arguments string) *fantasy.Response {
	return &fantasy.Response{
		Content: fantasy.ResponseContent{fantasy.ToolCallContent{
			ToolCallID: "call-" + name, ToolName: name, Input: arguments,
		}},
		FinishReason: fantasy.FinishReasonToolCalls,
		Usage:        fantasy.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
	}
}

func textResponse(text string) *fantasy.Response {
	return &fantasy.Response{
		Content:      fantasy.ResponseContent{fantasy.TextContent{Text: text}},
		FinishReason: fantasy.FinishReasonStop,
	}
}

func basicTools(run ToolFunc) []Tool {
	return []Tool{
		{Name: "read", Schema: map[string]any{"type": "object"}, Run: run},
		{Name: "submit", Schema: map[string]any{"type": "object"}},
	}
}

// assertToolNames checks the request offers exactly these tools. Finalization
// never narrows the tool set: it is part of the prefix signed reasoning binds to.
func assertToolNames(t *testing.T, call fantasy.Call, names ...string) {
	t.Helper()
	got := make([]string, 0, len(call.Tools))
	for _, tool := range call.Tools {
		got = append(got, tool.GetName())
	}
	if strings.Join(got, ",") != strings.Join(names, ",") {
		t.Fatalf("tools = %v, want %v", got, names)
	}
}

func TestAgentBudgetRefusalUsesReservedFinalCall(t *testing.T) {
	var calls int
	budgetFired := false
	c := testClient(func(_ context.Context, call fantasy.Call) (*fantasy.Response, error) {
		calls++
		assertToolNames(t, call, "read", "submit")
		return toolCallResponse("submit", `{"answer":"budget"}`), nil
	})
	c.cfg.Ledger = NewLedger(1500)
	var out struct {
		Answer string `json:"answer"`
	}
	err := c.Converse(t.Context(), "system", "user", ConverseOpts{
		MaxTurns: 4, FinalTool: "submit", OnBudgetExhausted: func() { budgetFired = true },
		Tools: basicTools(func(context.Context, map[string]any) (any, error) { return "read", nil }),
	}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if !budgetFired || calls != 1 || out.Answer != "budget" {
		t.Fatalf("budgetFired=%v calls=%d answer=%q", budgetFired, calls, out.Answer)
	}
}

func TestAgentInFlightDeadlineGetsFreshFinalWindow(t *testing.T) {
	var calls int
	deadlineFired := false
	c := testClient(func(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
		calls++
		if calls == 1 {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		assertToolNames(t, call, "read", "submit")
		return toolCallResponse("submit", `{"answer":"deadline"}`), nil
	})
	var out struct {
		Answer string `json:"answer"`
	}
	err := c.Converse(t.Context(), "system", "user", ConverseOpts{
		MaxTurns: 4, FinalTool: "submit", WorkDeadline: time.Now().Add(10 * time.Millisecond), WrapUpWindow: time.Second,
		OnWorkDeadline: func() { deadlineFired = true },
		Tools:          basicTools(func(context.Context, map[string]any) (any, error) { return "read", nil }),
	}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if !deadlineFired || calls != 2 || out.Answer != "deadline" {
		t.Fatalf("deadlineFired=%v calls=%d answer=%q", deadlineFired, calls, out.Answer)
	}
}

func TestInferenceRequestTimeoutIsEndpointFailure(t *testing.T) {
	c := testClient(func(ctx context.Context, _ fantasy.Call) (*fantasy.Response, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	c.cfg.RequestTimeout = 10 * time.Millisecond
	started := time.Now()
	var out struct {
		Answer string `json:"answer"`
	}
	err := c.CompleteStructured(t.Context(), "system", "user", "submit", &out)
	if !errors.Is(err, ErrEndpoint) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want endpoint-wrapped request timeout", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("request timeout took %s", elapsed)
	}
}

func TestAgentNudgesFreeFormResponse(t *testing.T) {
	var calls int
	c := testClient(func(_ context.Context, call fantasy.Call) (*fantasy.Response, error) {
		calls++
		if calls == 1 {
			return textResponse("I should keep looking."), nil
		}
		data, _ := json.Marshal(call.Prompt)
		if !strings.Contains(string(data), nudgeToolName) || !strings.Contains(string(data), "submit") {
			t.Fatalf("second call did not contain nudge result: %s", data)
		}
		return toolCallResponse("submit", `{"answer":"nudged"}`), nil
	})
	var out struct {
		Answer string `json:"answer"`
	}
	err := c.Converse(t.Context(), "system", "user", ConverseOpts{
		MaxTurns: 3, FinalTool: "submit", Tools: basicTools(nil),
	}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || out.Answer != "nudged" {
		t.Fatalf("calls=%d answer=%q", calls, out.Answer)
	}
}

func TestAgentReportsMalformedFinalArguments(t *testing.T) {
	c := testClient(func(context.Context, fantasy.Call) (*fantasy.Response, error) {
		return toolCallResponse("submit", `{`), nil
	})
	var out struct {
		Answer string `json:"answer"`
	}
	err := c.Converse(t.Context(), "system", "user", ConverseOpts{
		MaxTurns: 1, FinalTool: "submit", Tools: []Tool{{Name: "submit", Schema: map[string]any{"type": "object"}}},
	}, &out)
	if err == nil || !strings.Contains(err.Error(), "submit arguments did not satisfy the schema") {
		t.Fatalf("error = %v", err)
	}
}

func TestAgentDoesNotRetryFailedDeadlineWrapUp(t *testing.T) {
	var calls int
	c := testClient(func(context.Context, fantasy.Call) (*fantasy.Response, error) {
		calls++
		return nil, errors.New("transport unavailable")
	})
	c.cfg.MaxAttempts = 3
	var out struct {
		Answer string `json:"answer"`
	}
	err := c.Converse(t.Context(), "system", "user", ConverseOpts{
		MaxTurns: 4, FinalTool: "submit", WorkDeadline: time.Now().Add(-time.Second),
		Tools: basicTools(func(context.Context, map[string]any) (any, error) { return "read", nil }),
	}, &out)
	if !errors.Is(err, ErrWorkDeadline) || !errors.Is(err, ErrNoFinalCall) {
		t.Fatalf("error = %v, want deadline and no-final sentinels", err)
	}
	if calls != 1 {
		t.Fatalf("wrap-up calls = %d, want 1", calls)
	}
}

func TestAgentUnknownToolReturnsErrorToModel(t *testing.T) {
	var calls int
	var turns []TurnInfo
	c := testClient(func(_ context.Context, call fantasy.Call) (*fantasy.Response, error) {
		calls++
		if calls == 1 {
			return toolCallResponse("missing", `{}`), nil
		}
		data, _ := json.Marshal(call.Prompt)
		if !strings.Contains(string(data), "tool not found: missing") {
			t.Fatalf("unknown-tool result missing: %s", data)
		}
		return toolCallResponse("submit", `{"answer":"recovered"}`), nil
	})
	var out struct {
		Answer string `json:"answer"`
	}
	err := c.Converse(t.Context(), "system", "user", ConverseOpts{
		MaxTurns: 3, FinalTool: "submit", OnTurn: func(info TurnInfo) { turns = append(turns, info) },
		Tools: []Tool{{Name: "submit", Schema: map[string]any{"type": "object"}}},
	}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || out.Answer != "recovered" || len(turns) != 1 || turns[0].Tool != "missing" {
		t.Fatalf("calls=%d answer=%q turns=%#v", calls, out.Answer, turns)
	}
}

func TestAgentExecutesMultipleToolsSequentially(t *testing.T) {
	var calls int
	var order []string
	c := testClient(func(_ context.Context, _ fantasy.Call) (*fantasy.Response, error) {
		calls++
		if calls == 1 {
			return &fantasy.Response{
				Content: fantasy.ResponseContent{
					fantasy.ToolCallContent{ToolCallID: "call-first", ToolName: "first", Input: `{}`},
					fantasy.ToolCallContent{ToolCallID: "call-second", ToolName: "second", Input: `{}`},
				},
				FinishReason: fantasy.FinishReasonToolCalls,
			}, nil
		}
		return toolCallResponse("submit", `{"answer":"done"}`), nil
	})
	tool := func(name string) Tool {
		return Tool{Name: name, Schema: map[string]any{"type": "object"}, Run: func(context.Context, map[string]any) (any, error) {
			order = append(order, name)
			return name, nil
		}}
	}
	var out struct {
		Answer string `json:"answer"`
	}
	err := c.Converse(t.Context(), "system", "user", ConverseOpts{
		MaxTurns: 3, FinalTool: "submit", Tools: []Tool{tool("first"), tool("second"), {Name: "submit", Schema: map[string]any{"type": "object"}}},
	}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "first,second" {
		t.Fatalf("tool order = %v", order)
	}
}

func TestFantasyRetriesTransientModelFailure(t *testing.T) {
	var calls atomic.Int32
	c := testClient(func(context.Context, fantasy.Call) (*fantasy.Response, error) {
		if calls.Add(1) == 1 {
			return nil, &fantasy.ProviderError{Message: "temporary transport failure", TransientError: true, ResponseHeaders: map[string]string{"retry-after-ms": "1"}}
		}
		return toolCallResponse("submit", `{"answer":"retried"}`), nil
	})
	c.cfg.MaxAttempts = 2
	var out struct {
		Answer string `json:"answer"`
	}
	if err := c.CompleteStructured(t.Context(), "system", "user", "submit", &out); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || out.Answer != "retried" {
		t.Fatalf("calls=%d answer=%q", calls.Load(), out.Answer)
	}
}

func TestStructuredOutputRetryUsesFantasyLoop(t *testing.T) {
	var calls int
	c := testClient(func(_ context.Context, call fantasy.Call) (*fantasy.Response, error) {
		calls++
		if calls == 1 {
			return toolCallResponse("verdict", `{"count":"wrong"}`), nil
		}
		data, _ := json.Marshal(call.Prompt)
		if !strings.Contains(string(data), "not valid JSON matching the schema") {
			t.Fatalf("retry input lacks validation feedback: %s", data)
		}
		return toolCallResponse("verdict", `{"count":2}`), nil
	})
	c.cfg.MaxAttempts = 2
	var out struct {
		Count int `json:"count"`
	}
	if err := c.CompleteStructured(t.Context(), "system", "user", "verdict", &out); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || out.Count != 2 {
		t.Fatalf("calls=%d output=%#v", calls, out)
	}
}

func TestStructuredOutputRetryDoesNotMaskLaterProviderFailure(t *testing.T) {
	var calls int
	c := testClient(func(_ context.Context, _ fantasy.Call) (*fantasy.Response, error) {
		calls++
		if calls == 1 {
			return toolCallResponse("verdict", `{"count":"wrong"}`), nil
		}
		return nil, errors.New("provider unavailable")
	})
	c.cfg.MaxAttempts = 2
	var out struct {
		Count int `json:"count"`
	}
	err := c.CompleteStructured(t.Context(), "system", "user", "verdict", &out)
	if !errors.Is(err, ErrEndpoint) || !strings.Contains(err.Error(), "provider unavailable") {
		t.Fatalf("error = %v, want endpoint failure", err)
	}
	if strings.Contains(err.Error(), "did not satisfy the schema") {
		t.Fatalf("stale validation error masked endpoint failure: %v", err)
	}
}

func TestGenericUsageAccounting(t *testing.T) {
	c := testClient(func(context.Context, fantasy.Call) (*fantasy.Response, error) {
		resp := toolCallResponse("submit", `{"answer":"done"}`)
		resp.Usage = fantasy.Usage{InputTokens: 8, CacheReadTokens: 4, CacheCreationTokens: 1, OutputTokens: 3, TotalTokens: 11}
		return resp, nil
	})
	var out struct {
		Answer string `json:"answer"`
	}
	if err := c.CompleteStructured(t.Context(), "system", "user", "submit", &out); err != nil {
		t.Fatal(err)
	}
	if got := c.Usage(); got.Calls != 1 || got.UncachedInputTokens != 3 || got.CachedInputTokens != 4 || got.CacheWriteTokens != 1 || got.OutputTokens != 3 {
		t.Fatalf("usage = %#v", got)
	}
}

func TestAgentRestoresCompleteToolSchemaAtModelBoundary(t *testing.T) {
	c := testClient(func(_ context.Context, call fantasy.Call) (*fantasy.Response, error) {
		if len(call.Tools) != 1 {
			t.Fatalf("tools = %#v", call.Tools)
		}
		tool, ok := call.Tools[0].(fantasy.FunctionTool)
		if !ok {
			t.Fatalf("tool type = %T", call.Tools[0])
		}
		if additional, ok := tool.InputSchema["additionalProperties"].(bool); !ok || additional {
			t.Fatalf("input schema = %#v", tool.InputSchema)
		}
		return toolCallResponse("submit", `{"answer":"done"}`), nil
	})
	var out struct {
		Answer string `json:"answer"`
	}
	err := c.Converse(t.Context(), "system", "user", ConverseOpts{
		MaxTurns: 1, FinalTool: "submit", Tools: []Tool{{Name: "submit", Schema: map[string]any{
			"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "string"}},
			"required": []string{"answer"}, "additionalProperties": false,
		}}},
	}, &out)
	if err != nil {
		t.Fatal(err)
	}
}

func TestFinalToolPreservesLargeIntegers(t *testing.T) {
	c := testClient(func(context.Context, fantasy.Call) (*fantasy.Response, error) {
		return toolCallResponse("submit", `{"id":9007199254740993}`), nil
	})
	var out struct {
		ID int64 `json:"id"`
	}
	if err := c.Converse(t.Context(), "system", "user", ConverseOpts{
		MaxTurns: 1, FinalTool: "submit", Tools: []Tool{{Name: "submit", Schema: map[string]any{"type": "object"}}},
	}, &out); err != nil {
		t.Fatal(err)
	}
	if out.ID != 9007199254740993 {
		t.Fatalf("id = %d", out.ID)
	}
}

// assertAutomaticFinalCall checks a final-phase request: tool choice stays
// automatic and the submission tool is offered. The tool set is not narrowed;
// exploration calls are answered without being run.
func assertAutomaticFinalCall(t *testing.T, call fantasy.Call) {
	t.Helper()
	if call.ToolChoice == nil || *call.ToolChoice != fantasy.ToolChoiceAuto {
		t.Fatalf("tool choice = %v, want auto", call.ToolChoice)
	}
	for _, tool := range call.Tools {
		if tool.GetName() == "submit" {
			return
		}
	}
	t.Fatalf("tools = %#v, want submit offered", call.Tools)
}

func TestFinalizationUsesAutomaticToolChoice(t *testing.T) {
	for _, mode := range []string{"structured", "turn-limit", "budget", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			c := testClient(func(_ context.Context, call fantasy.Call) (*fantasy.Response, error) {
				calls++
				if calls > 5 {
					t.Fatal("valid submission did not terminate finalization")
				}
				assertAutomaticFinalCall(t, call)
				return toolCallResponse("submit", `{"answer":"done"}`), nil
			})
			var out struct {
				Answer string `json:"answer"`
			}
			opts := ConverseOpts{MaxTurns: 1, FinalTool: "submit", Tools: basicTools(nil)}
			if mode == "budget" {
				opts.MaxTurns = 4
				c.cfg.Ledger = NewLedger(1500)
			}
			if mode == "deadline" {
				opts.MaxTurns = 4
				opts.WorkDeadline = time.Now().Add(-time.Second)
			}
			var err error
			if mode == "structured" {
				err = c.CompleteStructured(t.Context(), "system", "user", "submit", &out)
			} else {
				err = c.Converse(t.Context(), "system", "user", opts, &out)
			}
			if err != nil || calls != 1 || out.Answer != "done" {
				t.Fatalf("error=%v calls=%d answer=%q", err, calls, out.Answer)
			}
		})
	}
}

func TestFinalizationRemindsWithoutReopeningExploration(t *testing.T) {
	for _, response := range []string{"prose", "exploration-tool", "unknown-tool"} {
		t.Run(response, func(t *testing.T) {
			calls := 0
			c := testClient(func(_ context.Context, call fantasy.Call) (*fantasy.Response, error) {
				calls++
				assertAutomaticFinalCall(t, call)
				if calls == 1 {
					switch response {
					case "exploration-tool":
						return toolCallResponse("read", `{}`), nil
					case "unknown-tool":
						return toolCallResponse("invented", `{}`), nil
					default:
						return textResponse(`{"answer":"not a submission"}`), nil
					}
				}
				prompt, _ := json.Marshal(call.Prompt)
				if !strings.Contains(string(prompt), "submit") || !strings.Contains(string(prompt), "tool call") {
					t.Fatalf("retry lacks submission reminder: %s", prompt)
				}
				return toolCallResponse("submit", `{"answer":"done"}`), nil
			})
			c.cfg.MaxAttempts = 2
			var out struct {
				Answer string `json:"answer"`
			}
			err := c.Converse(t.Context(), "system", "user", ConverseOpts{
				MaxTurns: 1, FinalTool: "submit", Tools: basicTools(func(context.Context, map[string]any) (any, error) {
					t.Error("exploration ran after finalization")
					return nil, nil
				}),
			}, &out)
			if err != nil || calls != 2 || out.Answer != "done" {
				t.Fatalf("error=%v calls=%d answer=%q", err, calls, out.Answer)
			}
		})
	}
}

func TestFinalizationMissingSubmissionIsBounded(t *testing.T) {
	for _, structured := range []bool{false, true} {
		t.Run(map[bool]string{false: "agent", true: "structured"}[structured], func(t *testing.T) {
			calls := 0
			c := testClient(func(context.Context, fantasy.Call) (*fantasy.Response, error) {
				calls++
				if calls > 3 {
					t.Fatal("unbounded finalization")
				}
				return textResponse(`{"answer":"not a tool call"}`), nil
			})
			c.cfg.MaxAttempts = 3
			var out struct {
				Answer string `json:"answer"`
			}
			var err error
			if structured {
				err = c.CompleteStructured(t.Context(), "system", "user", "submit", &out)
			} else {
				err = c.Converse(t.Context(), "system", "user", ConverseOpts{MaxTurns: 1, FinalTool: "submit", Tools: basicTools(nil)}, &out)
			}
			if !errors.Is(err, ErrNoFinalCall) || calls != 3 || out.Answer != "" {
				t.Fatalf("error=%v calls=%d answer=%q", err, calls, out.Answer)
			}
		})
	}
}

func TestFinalizationDoesNotRetryAfterCutoff(t *testing.T) {
	for _, cutoff := range []string{"budget", "deadline"} {
		t.Run(cutoff, func(t *testing.T) {
			calls := 0
			c := testClient(func(context.Context, fantasy.Call) (*fantasy.Response, error) {
				calls++
				return textResponse("no submission"), nil
			})
			c.cfg.MaxAttempts = 3
			opts := ConverseOpts{MaxTurns: 4, FinalTool: "submit", Tools: basicTools(nil)}
			sentinel := ErrBudgetExhausted
			if cutoff == "budget" {
				c.cfg.Ledger = NewLedger(1500)
			} else {
				opts.WorkDeadline = time.Now().Add(-time.Second)
				sentinel = ErrWorkDeadline
			}
			var out struct {
				Answer string `json:"answer"`
			}
			err := c.Converse(t.Context(), "system", "user", opts, &out)
			if !errors.Is(err, sentinel) || !errors.Is(err, ErrNoFinalCall) || calls != 1 || out.Answer != "" {
				t.Fatalf("error=%v calls=%d answer=%q", err, calls, out.Answer)
			}
		})
	}
}

func TestFinalizationSharesStructuredRetryAllowance(t *testing.T) {
	for _, proseFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "invalid-then-prose", true: "prose-then-invalid"}[proseFirst], func(t *testing.T) {
			calls := 0
			c := testClient(func(context.Context, fantasy.Call) (*fantasy.Response, error) {
				calls++
				if calls > 2 {
					t.Fatal("submission retries exceeded the shared allowance")
				}
				if (calls == 1) == proseFirst {
					return textResponse(`{"answer":"not a tool call"}`), nil
				}
				return toolCallResponse("submit", `{"answer":42}`), nil
			})
			c.cfg.MaxAttempts = 2
			var out struct {
				Answer string `json:"answer"`
			}
			err := c.CompleteStructured(t.Context(), "system", "user", "submit", &out)
			if err == nil || calls != 2 || out.Answer != "" {
				t.Fatalf("error=%v calls=%d answer=%q", err, calls, out.Answer)
			}
			if !proseFirst && !errors.Is(err, ErrNoFinalCall) {
				t.Fatalf("stale schema error masked the missing submission: %v", err)
			}
		})
	}
}

func TestFinalizationReminderRequiresTokenAdmission(t *testing.T) {
	calls := 0
	c := testClient(func(context.Context, fantasy.Call) (*fantasy.Response, error) {
		calls++
		response := textResponse("no submission")
		response.Usage = fantasy.Usage{InputTokens: 1000}
		return response, nil
	})
	c.cfg.MaxAttempts = 3
	c.cfg.Ledger = NewLedger(1500)
	var out struct {
		Answer string `json:"answer"`
	}
	err := c.CompleteStructured(t.Context(), "system", "user", "submit", &out)
	if !errors.Is(err, ErrBudgetExhausted) || !errors.Is(err, ErrNoFinalCall) || calls != 1 || out.Answer != "" {
		t.Fatalf("error=%v calls=%d answer=%q", err, calls, out.Answer)
	}
}

func TestFinalizationReminderPreservesCallerDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	calls := 0
	c := testClient(func(ctx context.Context, _ fantasy.Call) (*fantasy.Response, error) {
		calls++
		if calls == 1 {
			return textResponse("no submission"), nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	c.cfg.MaxAttempts = 3
	var out struct {
		Answer string `json:"answer"`
	}
	err := c.CompleteStructured(ctx, "system", "user", "submit", &out)
	if !errors.Is(err, context.DeadlineExceeded) || calls != 2 || out.Answer != "" {
		t.Fatalf("error=%v calls=%d answer=%q", err, calls, out.Answer)
	}
}

func TestFinalizationFrameworkValidationFailureIsBounded(t *testing.T) {
	calls := 0
	c := testClient(func(context.Context, fantasy.Call) (*fantasy.Response, error) {
		calls++
		if calls > 2 {
			t.Fatal("framework validation failure caused unbounded finalization")
		}
		// This unmarshals into Go's zero value but lacks the schema's required
		// answer property. Fantasy must reject it without executing the tool.
		return toolCallResponse("submit", `{}`), nil
	})
	c.cfg.MaxAttempts = 2
	var out struct {
		Answer string `json:"answer"`
	}
	err := c.CompleteStructured(t.Context(), "system", "user", "submit", &out)
	if !errors.Is(err, ErrNoFinalCall) || calls != 2 || out.Answer != "" {
		t.Fatalf("error=%v calls=%d answer=%q", err, calls, out.Answer)
	}
}

func TestExplorationSchemaRejectionPreservesFinalizationAllowance(t *testing.T) {
	calls := 0
	c := testClient(func(_ context.Context, call fantasy.Call) (*fantasy.Response, error) {
		calls++
		switch calls {
		case 1:
			// Go can unmarshal this, but Fantasy rejects the missing answer.
			return toolCallResponse("submit", `{}`), nil
		case 2:
			assertAutomaticFinalCall(t, call)
			return textResponse("not submitted yet"), nil
		default:
			assertAutomaticFinalCall(t, call)
			return toolCallResponse("submit", `{"answer":"done"}`), nil
		}
	})
	c.cfg.MaxAttempts = 2
	var out struct {
		Answer string `json:"answer"`
	}
	err := c.Converse(t.Context(), "system", "user", ConverseOpts{
		MaxTurns: 2, FinalTool: "submit", Tools: []Tool{
			{Name: "read", Schema: map[string]any{"type": "object"}, Run: func(context.Context, map[string]any) (any, error) {
				t.Error("exploration tool unexpectedly dispatched")
				return nil, nil
			}},
			{Name: "submit", Schema: reflectSchema(&out)},
		},
	}, &out)
	if err != nil || calls != 3 || out.Answer != "done" {
		t.Fatalf("error=%v calls=%d answer=%q", err, calls, out.Answer)
	}
}

func TestAgentFinalizationRetriesInvalidSubmission(t *testing.T) {
	calls := 0
	c := testClient(func(_ context.Context, call fantasy.Call) (*fantasy.Response, error) {
		calls++
		assertAutomaticFinalCall(t, call)
		if calls == 1 {
			return toolCallResponse("submit", `{"answer":42}`), nil
		}
		return toolCallResponse("submit", `{"answer":"corrected"}`), nil
	})
	c.cfg.MaxAttempts = 2
	var out struct {
		Answer string `json:"answer"`
	}
	err := c.Converse(t.Context(), "system", "user", ConverseOpts{
		MaxTurns: 1, FinalTool: "submit", Tools: basicTools(nil),
	}, &out)
	if err != nil || calls != 2 || out.Answer != "corrected" {
		t.Fatalf("error=%v calls=%d answer=%q", err, calls, out.Answer)
	}
}

func recordingTools(records *int, explore func()) []Tool {
	return []Tool{
		{Name: "read", Schema: map[string]any{"type": "object"}, Run: func(context.Context, map[string]any) (any, error) {
			explore()
			return nil, nil
		}},
		{Name: "record", Schema: map[string]any{"type": "object"}, RecordsResult: true, Run: func(context.Context, map[string]any) (any, error) {
			*records++
			return map[string]any{"recorded": true}, nil
		}},
		{Name: "submit", Schema: map[string]any{"type": "object"}},
	}
}

// Recording on the wrap-up turn does not use up submission attempts, but it
// has its own allowance: a model that only ever records still stops.
func TestWrapUpRecordRoundsAreBounded(t *testing.T) {
	calls, records := 0, 0
	c := testClient(func(context.Context, fantasy.Call) (*fantasy.Response, error) {
		calls++
		if calls > 4 {
			t.Fatal("recording reopened finalization without bound")
		}
		return toolCallResponse("record", `{}`), nil
	})
	var out struct{}
	err := c.Converse(t.Context(), "system", "user", ConverseOpts{
		MaxTurns: 1, FinalTool: "submit", Tools: recordingTools(&records, func() { t.Error("exploration ran on the wrap-up turn") }),
	}, &out)
	// One recording round within the allowance, then one that exceeds it:
	// it still records, and the run stops without spending a submission.
	if !errors.Is(err, ErrNoFinalCall) || calls != 2 || records != 2 || !strings.Contains(err.Error(), "2 recording rounds") {
		t.Fatalf("error=%v calls=%d records=%d", err, calls, records)
	}
}

// After the work deadline, recording still runs so the reported work is
// kept, but no further wrap-up call is made.
// A deadline that passes while a wrap-up response's tools run ends the loop:
// no new wrap-up call opens after the cutoff.
func TestDeadlineDuringWrapUpDispatchStopsFinalization(t *testing.T) {
	calls := 0
	c := testClient(func(context.Context, fantasy.Call) (*fantasy.Response, error) {
		calls++
		return toolCallResponse("record", `{}`), nil
	})
	c.cfg.MaxAttempts = 3
	deadline := time.Now().Add(100 * time.Millisecond)
	deadlineFired := false
	var recorded atomic.Bool
	var out struct{}
	err := c.Converse(t.Context(), "system", "user", ConverseOpts{
		MaxTurns: 1, FinalTool: "submit", WorkDeadline: deadline, OnWorkDeadline: func() { deadlineFired = true },
		Tools: []Tool{
			{Name: "record", Schema: map[string]any{"type": "object"}, RecordsResult: true, Run: func(ctx context.Context, _ map[string]any) (any, error) {
				// Straddle the deadline; recording still gets to finish.
				select {
				case <-time.After(time.Until(deadline) + 20*time.Millisecond):
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				recorded.Store(true)
				return map[string]any{"recorded": true}, nil
			}},
			{Name: "submit", Schema: map[string]any{"type": "object"}},
		},
	}, &out)
	if !errors.Is(err, ErrWorkDeadline) || !errors.Is(err, ErrNoFinalCall) || calls != 1 || !deadlineFired || !recorded.Load() {
		t.Fatalf("error=%v calls=%d deadline fired=%v recorded=%v", err, calls, deadlineFired, recorded.Load())
	}
}

func TestRecordToolRunsAfterDeadlineCutoff(t *testing.T) {
	for _, withExploration := range []bool{false, true} {
		t.Run(map[bool]string{false: "record-only", true: "with-exploration"}[withExploration], func(t *testing.T) {
			calls, records := 0, 0
			c := testClient(func(context.Context, fantasy.Call) (*fantasy.Response, error) {
				calls++
				content := fantasy.ResponseContent{fantasy.ToolCallContent{ToolCallID: "record", ToolName: "record", Input: `{}`}}
				if withExploration {
					content = append(fantasy.ResponseContent{fantasy.ToolCallContent{ToolCallID: "read", ToolName: "read", Input: `{}`}}, content...)
				}
				return &fantasy.Response{Content: content, FinishReason: fantasy.FinishReasonToolCalls}, nil
			})
			c.cfg.MaxAttempts = 3
			var out struct{}
			err := c.Converse(t.Context(), "system", "user", ConverseOpts{
				MaxTurns: 4, FinalTool: "submit", WorkDeadline: time.Now().Add(-time.Second),
				Tools: recordingTools(&records, func() { t.Error("exploration ran after the deadline") }),
			}, &out)
			if !errors.Is(err, ErrWorkDeadline) || !errors.Is(err, ErrNoFinalCall) || calls != 1 || records != 1 {
				t.Fatalf("error=%v calls=%d records=%d", err, calls, records)
			}
		})
	}
}

func TestStreamAbnormalFinishNeverDispatchesTools(t *testing.T) {
	for _, reason := range []fantasy.FinishReason{fantasy.FinishReasonLength, fantasy.FinishReasonContentFilter, fantasy.FinishReasonError, fantasy.FinishReasonUnknown} {
		t.Run(string(reason), func(t *testing.T) {
			c := testClient(func(context.Context, fantasy.Call) (*fantasy.Response, error) {
				return &fantasy.Response{
					Content: fantasy.ResponseContent{
						fantasy.ToolCallContent{ToolCallID: "read", ToolName: "read", Input: `{}`},
						fantasy.ToolCallContent{ToolCallID: "submit", ToolName: "submit", Input: `{"answer":"must not succeed"}`},
					},
					FinishReason: reason,
				}, nil
			})
			var tools int
			var out struct{ Answer string }
			err := c.Converse(t.Context(), "system", "user", ConverseOpts{
				MaxTurns: 2, FinalTool: "submit", Tools: basicTools(func(context.Context, map[string]any) (any, error) {
					tools++
					return "read", nil
				}),
			}, &out)
			if !errors.Is(err, ErrNoFinalCall) || tools != 0 || out.Answer != "" {
				t.Fatalf("error=%v tools=%d answer=%q", err, tools, out.Answer)
			}
		})
	}
}

func TestStreamFinalSubmissionSuppressesOtherToolCalls(t *testing.T) {
	c := testClient(func(context.Context, fantasy.Call) (*fantasy.Response, error) {
		response := toolCallResponse("submit", `{"answer":"done"}`)
		response.Content = append(fantasy.ResponseContent{fantasy.ToolCallContent{ToolCallID: "read", ToolName: "read", Input: `{}`}}, response.Content...)
		return response, nil
	})
	var out struct{ Answer string }
	err := c.Converse(t.Context(), "system", "user", ConverseOpts{
		MaxTurns: 2, FinalTool: "submit", Tools: basicTools(func(context.Context, map[string]any) (any, error) {
			t.Error("exploration tool ran alongside final submission")
			return nil, nil
		}),
	}, &out)
	if err != nil || out.Answer != "done" {
		t.Fatalf("error=%v answer=%q", err, out.Answer)
	}
}

func TestStreamCancellationDoesNotDispatchBufferedTool(t *testing.T) {
	for _, workDeadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "request-timeout", true: "work-deadline"}[workDeadline], func(t *testing.T) {
			c := testClient(nil)
			calls := 0
			c.model.(*fakeLanguageModel).stream = func(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
				calls++
				return func(yield func(fantasy.StreamPart) bool) {
					if calls == 1 {
						if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "read", ToolCallName: "read", ToolCallInput: `{}`}) {
							return
						}
						<-ctx.Done()
						yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: ctx.Err()})
						return
					}
					if len(call.Tools) != 2 || call.Tools[1].GetName() != "submit" || ctx.Err() != nil {
						t.Error("wrap-up did not keep the tool set and get a fresh context")
					}
					if yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "submit", ToolCallName: "submit", ToolCallInput: `{"answer":"partial"}`}) {
						yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
					}
				}, nil
			}
			opts := ConverseOpts{
				MaxTurns: 4, FinalTool: "submit", WrapUpWindow: time.Second,
				Tools: basicTools(func(context.Context, map[string]any) (any, error) {
					t.Error("buffered exploration tool ran after cancellation")
					return nil, nil
				}),
			}
			if workDeadline {
				opts.WorkDeadline = time.Now().Add(100 * time.Millisecond)
			} else {
				c.cfg.RequestTimeout = 20 * time.Millisecond
			}
			var out struct{ Answer string }
			err := c.Converse(t.Context(), "system", "user", opts, &out)
			if workDeadline {
				if err != nil || calls != 2 || out.Answer != "partial" {
					t.Fatalf("error=%v calls=%d answer=%q", err, calls, out.Answer)
				}
			} else if !errors.Is(err, ErrEndpoint) || calls != 1 || out.Answer != "" {
				t.Fatalf("error=%v calls=%d answer=%q", err, calls, out.Answer)
			}
		})
	}
}

func TestStreamConsumerStopCancelsAndReleasesReservation(t *testing.T) {
	c := testClient(nil)
	c.cfg.Ledger = NewLedger(100000)
	var providerCtx context.Context
	c.model.(*fakeLanguageModel).stream = func(ctx context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
		providerCtx = ctx
		return func(yield func(fantasy.StreamPart) bool) {
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "partial"})
		}, nil
	}
	managed := &managedModel{client: c, state: &agentRunState{wd: newWorkDeadlineState(ConverseOpts{})}}
	stream, err := managed.Stream(t.Context(), fantasy.Call{})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
		break
	}
	if providerCtx == nil || providerCtx.Err() != context.Canceled || c.cfg.Ledger.outstanding != 0 {
		t.Fatalf("provider context=%v outstanding=%v", providerCtx, c.cfg.Ledger.outstanding)
	}
}

// requestToolSchemas returns each tool's JSON Schema as the provider received it.
func requestToolSchemas(t *testing.T, protocol string, request map[string]any) map[string]map[string]any {
	t.Helper()
	tools, _ := request["tools"].([]any)
	schemas := make(map[string]map[string]any, len(tools))
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		var name any
		var schema map[string]any
		switch protocol {
		case "anthropic":
			name, schema = tool["name"], asObject(tool["input_schema"])
		case "responses":
			name, schema = tool["name"], asObject(tool["parameters"])
		default:
			function := asObject(tool["function"])
			name, schema = function["name"], asObject(function["parameters"])
		}
		if name, ok := name.(string); ok {
			schemas[name] = schema
		}
	}
	return schemas
}

func asObject(value any) map[string]any {
	object, _ := value.(map[string]any)
	return object
}

func assertRequired(t *testing.T, what string, schema map[string]any, want ...string) {
	t.Helper()
	raw, _ := schema["required"].([]any)
	got := make([]string, 0, len(raw))
	for _, item := range raw {
		name, _ := item.(string)
		got = append(got, name)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("%s required = %v, want %v (schema %v)", what, got, want, schema)
	}
}

// Fantasy's Anthropic adapter reads a tool's required list only as []string
// and sends an empty list for any other shape, so a schema that went through
// JSON must not reach any provider with its required fields dropped.
func TestToolSchemasCarryRequiredFields(t *testing.T) {
	type detail struct {
		Line int    `json:"line"`
		Note string `json:"note,omitempty"`
	}
	type submission struct {
		Answer string `json:"answer"`
		Detail detail `json:"detail"`
		Extra  string `json:"extra,omitempty"`
	}
	for _, protocol := range []string{"chat", "responses", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			var mu sync.Mutex
			var requests []map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer r.Body.Close()
				var request map[string]any
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				mu.Lock()
				requests = append(requests, request)
				mu.Unlock()
				_ = testutil.WriteInferenceStream(w, protocol, structuredToolResponse(protocol, "submit", `{"answer":"done","detail":{"line":1}}`))
			}))
			defer server.Close()
			c, err := New(t.Context(), ClientConfig{
				BaseURL: server.URL + "/v1", APIKey: "key", Model: "test-model",
				Protocol: protocol, MaxOutputTokens: 100, MaxAttempts: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			var out submission
			err = c.Converse(t.Context(), "system", "user", ConverseOpts{MaxTurns: 2, FinalTool: "submit", Tools: []Tool{
				{Name: "read", Description: "read", Schema: map[string]any{
					"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}},
					"required": []string{"path"},
				}, Run: func(context.Context, map[string]any) (any, error) { return "package a", nil }},
				{Name: "submit", Description: "submit", Schema: reflectSchema(&out)},
			}}, &out)
			if err != nil || out.Answer != "done" {
				t.Fatalf("answer = %q, error = %v", out.Answer, err)
			}

			mu.Lock()
			defer mu.Unlock()
			if len(requests) != 1 {
				t.Fatalf("requests = %d, want 1", len(requests))
			}
			schemas := requestToolSchemas(t, protocol, requests[0])
			assertRequired(t, "submit", schemas["submit"], "answer", "detail")
			properties := asObject(schemas["submit"]["properties"])
			assertRequired(t, "submit.detail", asObject(properties["detail"]), "line")
			assertRequired(t, "read", schemas["read"], "path")
		})
	}
}
