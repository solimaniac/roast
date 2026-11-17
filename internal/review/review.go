package review

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bluesky-social/roast/internal/llm"
	"github.com/invopop/jsonschema"
)

// proseExtensions is the set of file extensions treated as prose.
// Unknown extensions count as code — the stricter default.
var proseExtensions = map[string]struct{}{
	".md": {}, ".mdx": {}, ".markdown": {}, ".rst": {},
	".txt": {}, ".adoc": {}, ".asciidoc": {}, ".org": {}, ".tex": {},
}

// langExtensions maps file extensions to the language-family tag that
// selects finder addenda. Extensions with no entry contribute nothing.
var langExtensions = map[string]string{
	".tsx": "react", ".jsx": "react",
	".vue": "frontend", ".svelte": "frontend", ".astro": "frontend",
	".ts": "web", ".js": "web", ".mjs": "web", ".cjs": "web", ".css": "web", ".scss": "web",
	".go": "go",
}

// ClassifyMode returns "prose" if every file has a prose extension,
// "mixed" if some do, and "code" otherwise (including empty input).
func ClassifyMode(files []string) string {
	if len(files) == 0 {
		return "code"
	}
	prose := 0
	for _, f := range files {
		ext := strings.ToLower(filepath.Ext(f))
		if _, ok := proseExtensions[ext]; ok {
			prose++
		}
	}
	if prose == 0 {
		return "code"
	}
	if prose == len(files) {
		return "prose"
	}
	return "mixed"
}

// ClassifyLangs returns a sorted, deduped set of language-family tags
// derived from file extensions, used to select finder addenda. Extensions
// with no mapping in langExtensions contribute nothing; empty input returns
// an empty (non-nil) slice.
func ClassifyLangs(files []string) []string {
	seen := make(map[string]struct{})
	for _, f := range files {
		ext := strings.ToLower(filepath.Ext(f))
		if tag, ok := langExtensions[ext]; ok {
			seen[tag] = struct{}{}
		}
	}
	langs := make([]string, 0, len(seen))
	for tag := range seen {
		langs = append(langs, tag)
	}
	sort.Strings(langs)
	return langs
}

// WireFinding is the model-facing finding schema — schema-bearing fields only.
// Verdict and DropReason are post-hoc fields never exposed to the model;
// separating them prevents the model from fabricating verdict/drop data and
// keeps invopop from including them in the JSON Schema required array.
// Field order matters: reasoning first lets the model commit to its
// justification before choosing a severity.
type WireFinding struct {
	Reasoning        string   `json:"reasoning"`
	Severity         Severity `json:"severity" jsonschema:"enum=INFO,enum=LOW,enum=MEDIUM,enum=HIGH,enum=CRITICAL"`
	Confidence       float64  `json:"confidence" jsonschema:"minimum=0,maximum=1"`
	CWE              string   `json:"cwe" jsonschema_description:"CWE id when applicable, else empty string"`
	Description      string   `json:"description"`
	File             string   `json:"file"`
	Lines            []int    `json:"lines"`
	CodeQuote        string   `json:"code_quote" jsonschema_description:"Verbatim copy of the cited lines, exactly as they appear in <files>"`
	FixedCode        string   `json:"fixed_code" jsonschema_description:"Concrete replacement for code_quote that resolves the issue"`
	FrontendCategory string   `json:"frontend_category" jsonschema:"enum=none,enum=hooks,enum=security,enum=a11y,enum=perf,enum=ts" jsonschema_description:"Which frontend-addendum dimension produced this finding; use none for other findings"`
}

// ToFinding converts a WireFinding to a Finding with empty post-hoc fields.
func (w WireFinding) ToFinding() Finding {
	category := w.FrontendCategory
	if category == "none" {
		category = ""
	}
	return Finding{
		Reasoning:        w.Reasoning,
		Severity:         w.Severity,
		Confidence:       w.Confidence,
		CWE:              w.CWE,
		Description:      w.Description,
		File:             w.File,
		Lines:            append([]int(nil), w.Lines...),
		CodeQuote:        w.CodeQuote,
		FixedCode:        w.FixedCode,
		FrontendCategory: category,
	}
}

// FindingsResponse is the model's top-level structured output for a review run.
type FindingsResponse struct {
	Assessment string        `json:"assessment" jsonschema:"enum=FINDINGS_PRESENT,enum=NO_FINDINGS"`
	Findings   []WireFinding `json:"findings"`
}

// Validate enforces the forcing-functions server-side. A dropped finding means
// the model ignored its instructions — dropped findings are kept (tagged) for
// --show-dropped rather than discarded silently.
//
// ground, when non-nil, is the quote-grounding check (context.NewQuoteGrounder):
// a code_quote that does not appear (whitespace-normalized) in content the
// finder could have seen is dropped as ungrounded. nil disables the check —
// only for callers with no content to ground against.
func Validate(resp FindingsResponse, confidenceFloor float64, ground func(file, quote string) bool) *Result {
	result := &Result{
		Assessment: resp.Assessment,
		Findings:   []Finding{},
		Dropped:    []Finding{},
		Notes:      []string{},
	}
	for _, wf := range resp.Findings {
		f := wf.ToFinding()
		// Providers do not all enforce the schema's severity enum. Rank()
		// treats anything unrecognized as INFO, so an unnormalized "high"
		// would pass a --max-severity HIGH gate; canonicalize it, and drop
		// what cannot be canonicalized rather than let it rank as INFO.
		sev, ok := ParseSeverity(string(f.Severity))
		if !ok {
			f.DropReason = DropInvalidSeverity
			result.Dropped = append(result.Dropped, f)
			continue
		}
		f.Severity = sev
		if strings.TrimSpace(f.CodeQuote) == "" {
			f.DropReason = DropMissingCodeQuote
			result.Dropped = append(result.Dropped, f)
			continue
		}
		if strings.TrimSpace(f.FixedCode) == "" {
			f.DropReason = DropMissingFixedCode
			result.Dropped = append(result.Dropped, f)
			continue
		}
		if ground != nil && !ground(f.File, f.CodeQuote) {
			f.DropReason = DropUngroundedCodeQuote
			result.Dropped = append(result.Dropped, f)
			continue
		}
		if f.Confidence < confidenceFloor {
			f.DropReason = DropBelowConfidenceFloor
			result.Dropped = append(result.Dropped, f)
			continue
		}
		result.Findings = append(result.Findings, f)
	}
	if len(result.Findings) > 0 {
		result.Assessment = AssessmentFindings
	} else if result.Assessment == AssessmentFindings {
		result.Assessment = AssessmentNoFindings
	}
	assignIdentity(result)
	return result
}

// assignIdentity gives every finding — kept and dropped alike, since dropped
// findings are adjudicable too — an immutable UUID and a run-local display
// alias (see Finding.ID / Finding.Alias). The alias is the shortest ID prefix
// of at least 6 hex characters unique among ALL of the run's IDs (not merely
// among assigned aliases — a short alias must not remain a prefix of some
// other finding's full ID, or it would be ambiguous as a citation).
func assignIdentity(result *Result) {
	all := make([]*Finding, 0, len(result.Findings)+len(result.Dropped))
	for i := range result.Findings {
		all = append(all, &result.Findings[i])
	}
	for i := range result.Dropped {
		all = append(all, &result.Dropped[i])
	}

	hexIDs := make([]string, len(all))
	prefixCounts := make(map[string]int)
	for i, f := range all {
		if f.ID == "" {
			f.ID = newUUID()
		}
		hexIDs[i] = strings.ReplaceAll(f.ID, "-", "")
		for n := 6; n <= len(hexIDs[i]); n++ {
			prefixCounts[hexIDs[i][:n]]++
		}
	}

	for i, f := range all {
		n := 6
		for n < len(hexIDs[i]) && prefixCounts[hexIDs[i][:n]] > 1 {
			n++
		}
		f.Alias = "R-" + hexIDs[i][:n]
	}
}

// newUUID returns a random RFC 4122 version-4 UUID string.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on supported platforms; if it somehow
		// does, silently duplicated identities are worse than a crash.
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// FindInput carries all inputs to the Find stage. Prompt strings are accepted
// directly so Find does not import the prompts package — the CLI wires them.
type FindInput struct {
	// System is the system prompt text.
	System string
	// User is the user message (diff + file context, already rendered).
	User string
	// Quick selects the quick-mode confidence floor.
	Quick bool
	// ConfidenceFloor is the minimum confidence for a finding to survive Validate.
	ConfidenceFloor float64
	// Interactive enables the multi-turn exploration tool loop.
	Interactive bool
	// Tools are exploration tools available during an interactive run.
	// The submit_findings final tool is appended automatically.
	Tools []llm.Tool
	// MaxTurns bounds the interactive tool loop; 0 uses no per-turn limit.
	MaxTurns int
	// Survey selects submit_findings' description. See submitDescription.
	Survey bool
	// Ground is the quote-grounding check passed through to Validate; nil
	// disables grounding (see Validate).
	Ground func(file, quote string) bool
	// Capture, when non-nil, receives every incrementally-reported finding
	// the moment it arrives (the CLI wires an append-only disk spill so
	// no stop path — deadline, exhausted budget, SIGKILL — discards work).
	Capture func(WireFinding)
	// Deadline, when non-zero, is the run's work deadline passed through to
	// Converse: exploration stops there and a reserved wrap-up turn flushes
	// whatever the finder has.
	Deadline time.Time
}

// findingAccumulator collects incrementally-reported findings:
// deduped by similarity fingerprint, last report of a fingerprint wins (the
// model may refine severity or wording on a re-report), ordered by first
// appearance. Every accepted report is forwarded to sink immediately.
type findingAccumulator struct {
	sink  func(WireFinding)
	order []string
	byKey map[string]WireFinding
}

func newFindingAccumulator(sink func(WireFinding)) *findingAccumulator {
	return &findingAccumulator{sink: sink, byKey: make(map[string]WireFinding)}
}

func (a *findingAccumulator) add(wf WireFinding) {
	key := Fingerprint(wf.ToFinding())
	if _, seen := a.byKey[key]; !seen {
		a.order = append(a.order, key)
	}
	a.byKey[key] = wf
	if a.sink != nil {
		a.sink(wf)
	}
}

func (a *findingAccumulator) ordered() []WireFinding {
	out := make([]WireFinding, 0, len(a.order))
	for _, k := range a.order {
		out = append(out, a.byKey[k])
	}
	return out
}

func submitDescription(survey bool) string {
	if !survey {
		return "Submit your final findings. Call exactly once when you have gathered enough context; this ends the review."
	}
	return "Submit your findings. Call exactly once; this ends the review. Submit EVERY grounded defect you found rather than a curated best-of. Use confidence to express uncertainty and let the verifier adjudicate it. Every finding still needs a verbatim code_quote and concrete fixed_code."
}

// Find runs the finder stage against client. Single-shot (non-interactive) uses
// CompleteStructured; interactive uses Converse with the provided exploration
// tools plus a submit_findings final tool derived from FindingsResponse's schema.
//
// On ErrNoFinalCall the loop returns a result with Assessment
// INCOMPLETE_NO_SUBMISSION and a note naming the stop reason rather than an
// error. This is NEVER silently converted to NO_FINDINGS: a budget-exhausted
// run that never submitted is visibly incomplete, not clean (the coverage
// contract). The reserved final-turn flush in Converse is the best-effort
// salvage before this path is reached.
func Find(ctx context.Context, client llm.Client, in FindInput) (*Result, error) {
	if !in.Interactive {
		var resp FindingsResponse
		if err := client.CompleteStructured(ctx, in.System, in.User, "findings", &resp); err != nil {
			return nil, err
		}
		return Validate(resp, in.ConfidenceFloor, in.Ground), nil
	}

	acc := newFindingAccumulator(in.Capture)
	reportTool := llm.Tool{
		Name:        "report_finding",
		Description: "Report ONE finding the moment you confirm it, instead of holding findings until the end — findings reported here are saved immediately and survive even if the run is cut off by a budget or deadline. Same grounding bar as submit_findings: verbatim code_quote, concrete fixed_code. Re-reporting a finding replaces the earlier version; submit_findings may repeat it safely (deduplicated).",
		Schema:      reflectSchema(&WireFinding{}),
		// Recording only touches the accumulator, so it runs on the wrap-up
		// turn and after the work deadline; that is when it matters most.
		RecordsResult: true,
		Run: func(_ context.Context, args map[string]any) (any, error) {
			raw, err := json.Marshal(args)
			if err != nil {
				return map[string]any{"error": err.Error()}, nil
			}
			var wf WireFinding
			if err := json.Unmarshal(raw, &wf); err != nil {
				return map[string]any{"error": fmt.Sprintf("malformed finding: %v", err)}, nil
			}
			acc.add(wf)
			return map[string]any{"recorded": true, "reported_so_far": len(acc.order)}, nil
		},
	}

	submitTool := llm.Tool{
		Name:        "submit_findings",
		Description: submitDescription(in.Survey),
		Schema:      reflectSchema(&FindingsResponse{}),
		Run:         nil, // final tool — args returned to caller, not dispatched
	}
	tools := append(append([]llm.Tool{}, in.Tools...), reportTool, submitTool)

	deadlineFired := false
	budgetFired := false
	var resp FindingsResponse
	err := client.Converse(ctx, in.System, in.User, llm.ConverseOpts{
		Tools:             tools,
		FinalTool:         "submit_findings",
		MaxTurns:          in.MaxTurns,
		WorkDeadline:      in.Deadline,
		OnWorkDeadline:    func() { deadlineFired = true },
		OnBudgetExhausted: func() { budgetFired = true },
	}, &resp)
	if err != nil {
		// Raw context errors keep propagating (pinned by
		// TestFind_PropagatesClientError); the disk spill preserves the
		// findings either way. The graceful cutoffs — turn budget and the
		// work deadline — come back as (wrapped) ErrNoFinalCall.
		if errors.Is(err, llm.ErrNoFinalCall) {
			stopReason := "budget_exhausted"
			if errors.Is(err, llm.ErrWorkDeadline) {
				stopReason = "deadline"
			} else if errors.Is(err, llm.ErrBudgetExhausted) {
				stopReason = "token_budget"
			}
			// Keep the cause: it carries the provider's error or the reason the
			// model stopped, which the stop reason alone does not.
			note := fmt.Sprintf("interactive review ended without submit_findings (even after the reserved wrap-up turn); stop reason: %s; cause: %v", stopReason, err)
			if n := len(acc.order); n > 0 {
				// The governing invariant: no stop path may discard
				// accumulated work. The incrementally-reported findings ARE
				// the result; only the un-reported tail is lost. Validate
				// leaves the INCOMPLETE assessment in place unless findings
				// survive — either way this is never a clean NO_FINDINGS.
				result := Validate(FindingsResponse{
					Assessment: AssessmentIncompleteNoSubmission,
					Findings:   acc.ordered(),
				}, in.ConfidenceFloor, in.Ground)
				result.Incomplete = true
				result.StopReason = stopReason
				result.Notes = append(result.Notes, note,
					fmt.Sprintf("%d incrementally-reported finding(s) retained from before the cutoff", n))
				return result, nil
			}
			return &Result{
				Assessment: AssessmentIncompleteNoSubmission,
				Findings:   []Finding{},
				Dropped:    []Finding{},
				Notes:      []string{note},
				Incomplete: true,
				StopReason: stopReason,
			}, nil
		}
		return nil, err
	}
	// Merge the final submission over anything reported incrementally: the
	// submission may repeat, refine, or extend reported findings (last
	// version of a fingerprint wins), and reported-but-unsubmitted findings
	// are kept — reporting was the durable act.
	for _, wf := range resp.Findings {
		acc.add(wf)
	}
	result := Validate(FindingsResponse{
		Assessment: resp.Assessment,
		Findings:   acc.ordered(),
	}, in.ConfidenceFloor, in.Ground)
	if deadlineFired || budgetFired {
		// The wrap-up submission succeeded, but exploration was still cut
		// short — coverage claims must say so (survey completion: partial).
		reason, what := "deadline", "work deadline reached"
		if !deadlineFired {
			reason, what = "token_budget", "run token budget exhausted"
		}
		result.Incomplete = true
		result.StopReason = reason
		result.Notes = append(result.Notes,
			what+"; the submission is the reserved wrap-up flush, not a converged review")
	}
	return result, nil
}

// reflectSchema returns a JSON Schema map for v, suitable as an llm.Tool.Schema.
// Settings match the llm package's reflector: AllowAdditionalProperties=false,
// DoNotReference=true.
func reflectSchema(v any) map[string]any {
	r := &jsonschema.Reflector{
		AllowAdditionalProperties: false,
		DoNotReference:            true,
	}
	s := r.Reflect(v)
	b, _ := json.Marshal(s)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	// Strip JSON Schema document-level fields not needed in tool schemas.
	delete(m, "$schema")
	delete(m, "$id")
	return m
}
