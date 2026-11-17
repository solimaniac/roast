// Package verify runs the cross-family grounded verification stage.
//
// Every surviving finding from the finder is adjudicated by a separate model
// (the verifier). The verifier receives the claim and the code only — never the
// finder's reasoning (anti-anchoring). A refutation counts only when the verdict
// is grounded in specific code, guards, or language semantics. Findings that
// cannot be refuted with grounding, or whose context is insufficient, keep
// gating — uncertainty must not silently kill a CRITICAL.
package verify

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/bluesky-social/roast/internal/llm"
	"github.com/bluesky-social/roast/internal/review"
	"github.com/bluesky-social/roast/internal/textsafe"
	"github.com/invopop/jsonschema"
)

// wireVerdict is the model-facing verdict schema. It mirrors review.Verdict's
// schema-bearing fields but excludes Verifier and Undecided, which are
// post-hoc fields set by roast — exposing them to the model would let it
// fabricate them and trigger confabulation.
type wireVerdict struct {
	Reasoning            string               `json:"reasoning"`
	Status               review.VerdictStatus `json:"status" jsonschema:"enum=confirmed,enum=refuted,enum=insufficient_context"`
	Confidence           float64              `json:"confidence" jsonschema:"minimum=0,maximum=1"`
	Grounding            string               `json:"grounding" jsonschema_description:"The exact lines / contradicting code / language-semantics fact the verdict rests on"`
	RecalibratedSeverity review.Severity      `json:"recalibrated_severity,omitempty" jsonschema:"enum=INFO,enum=LOW,enum=MEDIUM,enum=HIGH,enum=CRITICAL" jsonschema_description:"If the finding is real but its severity is overstated, the corrected LOWER severity, justified in grounding. Omit to keep the finder's severity. Never raise severity."`
}

// Deps carries external inputs to Verify so the package does not import
// prompts or context.
type Deps struct {
	// SystemPrompt is the verifier system prompt (pre-rendered).
	SystemPrompt string
	// UserPrompt builds the per-finding user message from a finding.
	// It receives the finding and should include the file snippet.
	UserPrompt func(f review.Finding) string
	// Tools are exploration tools made available during verification.
	Tools []llm.Tool
	// MaxTurns bounds the per-finding tool loop. 0 means the llm default.
	MaxTurns int
	// Concurrency is the number of findings verified in parallel. Defaults to 4.
	Concurrency int
	// Log receives one-line progress messages. Defaults to no-op.
	Log func(string)
	// MaxVerified caps how many findings enter verification this run (it
	// bounds the finding-count × turns × full-price product). Findings past
	// the cap keep gating with an explicit unverified verdict (fail-open).
	// 0 means no cap.
	MaxVerified int
	// Ledger, when non-nil, is the shared run budget: once exhausted, no
	// further verifications launch and remaining findings keep gating with
	// an explicit unverified verdict (fail-open).
	Ledger *llm.Ledger
}

// unverified builds the fail-open verdict for a finding that never entered
// verification: it keeps gating, with the reason on the record.
func unverified(reason string) *review.Verdict {
	return &review.Verdict{
		Status:    review.VerdictInsufficientContext,
		Undecided: true,
		Reasoning: reason,
	}
}

// Verify attaches a cross-family verdict to every finding in result. The input
// result is not modified in place; a new Result is returned. Refuted findings
// move to Dropped with reason DropRefutedByVerifier. Errors per-finding fail
// open: the finding is kept with an Undecided verdict and no error is
// propagated. Assessment is recomputed from surviving findings.
//
// Output order is deterministic (same as input order) regardless of
// goroutine completion order.
func Verify(ctx context.Context, verifier llm.Client, result *review.Result, deps Deps) *review.Result {
	if deps.Concurrency <= 0 {
		deps.Concurrency = 4
	}
	if deps.Log == nil {
		deps.Log = func(string) {}
	}
	if deps.UserPrompt == nil {
		deps.UserPrompt = func(f review.Finding) string { return "" }
	}

	n := len(result.Findings)
	verdicts := make([]*review.Verdict, n)

	// Worker pool bounded by deps.Concurrency.
	type job struct {
		idx     int
		finding review.Finding
	}
	jobs := make(chan job, n)
	for i, f := range result.Findings {
		jobs <- job{i, f}
	}
	close(jobs)

	var wg sync.WaitGroup
	sem := make(chan struct{}, deps.Concurrency)
	for range deps.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				sem <- struct{}{}
				var v *review.Verdict
				switch {
				case deps.MaxVerified > 0 && j.idx >= deps.MaxVerified:
					// Per-run verifier cap: findings past the cap keep
					// gating unverified, and the report says so.
					v = unverified(fmt.Sprintf("unverified: over the per-run verifier cap (%d)", deps.MaxVerified))
				case deps.Ledger.Exhausted():
					// Fail open: uncertainty must not silently kill a
					// CRITICAL — the finding gates with the reason attached.
					v = unverified("unverified: run token budget exhausted before this finding's verification")
				default:
					v = verifyOne(ctx, verifier, j.finding, deps)
				}
				v.Verifier = verifier.Name()
				verdicts[j.idx] = v
				<-sem
			}
		}()
	}
	wg.Wait()

	// Assemble output preserving input order.
	out := &review.Result{
		Dropped:    append([]review.Finding(nil), result.Dropped...),
		Notes:      append([]string(nil), result.Notes...),
		Assessment: result.Assessment,
		Findings:   []review.Finding{},
		// Cutoff state must survive verification: a cut-off run whose
		// findings were verified is still a cut-off run (coverage contract).
		Incomplete: result.Incomplete,
		StopReason: result.StopReason,
	}
	for i, f := range result.Findings {
		v := verdicts[i]
		// A refutation without grounding is not a valid refutation — the
		// verifier must cite the specific code, guard, or semantics that kills
		// the claim. Without it, the verdict is ungrounded speculation and we
		// fail open (insufficient_context) rather than silently dropping a
		// potentially real finding.
		if v.Status == review.VerdictRefuted && strings.TrimSpace(v.Grounding) == "" {
			v.Status = review.VerdictInsufficientContext
			v.Undecided = true
			v.Reasoning = "verifier returned refuted without grounding — treating as insufficient_context (fail-open)"
		}
		// Same discipline for a severity recalibration: an ungrounded downgrade
		// is not a valid downgrade. The finder's severity stands (fail-open, no
		// weakening) rather than letting an unjustified recalibration through.
		if v.Status == review.VerdictConfirmed && v.RecalibratedSeverity != "" && strings.TrimSpace(v.Grounding) == "" {
			v.RecalibratedSeverity = ""
		}
		f.Verdict = v
		if !v.Gates() {
			f.DropReason = review.DropRefutedByVerifier
			out.Dropped = append(out.Dropped, f)
			// Finder text is model-written from untrusted repository input:
			// escape it so the log line can neither drive the terminal nor
			// break into extra lines.
			deps.Log(fmt.Sprintf(
				"roast: verifier refuted [%s] %s (conf %.2f)",
				textsafe.Line(string(f.Severity)), textsafe.Line(truncate(f.Description, 80)), v.Confidence,
			))
		} else {
			out.Findings = append(out.Findings, f)
		}
	}

	if len(out.Findings) == 0 && out.Assessment == review.AssessmentFindings {
		out.Assessment = review.AssessmentNoFindings
	}
	return out
}

// verifyOne runs the verifier for a single finding. Errors and ErrNoFinalCall
// both fail open: a verdict with Undecided=true is returned and the finding
// keeps gating.
func verifyOne(ctx context.Context, verifier llm.Client, f review.Finding, deps Deps) *review.Verdict {
	submitTool := llm.Tool{
		Name:        "submit_verdict",
		Description: "Submit your verdict on the finding. Call exactly once.",
		Schema:      reflectWireVerdictSchema(),
		Run:         nil,
	}
	tools := append(append([]llm.Tool{}, deps.Tools...), submitTool)

	var wv wireVerdict
	err := verifier.Converse(ctx, deps.SystemPrompt, deps.UserPrompt(f), llm.ConverseOpts{
		Tools:     tools,
		FinalTool: "submit_verdict",
		MaxTurns:  deps.MaxTurns,
	}, &wv)

	if err != nil {
		return &review.Verdict{
			Status:     review.VerdictInsufficientContext,
			Confidence: 0,
			Reasoning:  fmt.Sprintf("verifier did not reach a verdict: %v", err),
			Undecided:  true,
		}
	}
	return &review.Verdict{
		Reasoning:            wv.Reasoning,
		Status:               wv.Status,
		Confidence:           wv.Confidence,
		Grounding:            wv.Grounding,
		RecalibratedSeverity: wv.RecalibratedSeverity,
	}
}

// truncate cuts s to at most n bytes without splitting a UTF-8 sequence, which
// would otherwise render as escaped invalid bytes.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// reflectWireVerdictSchema returns the JSON Schema for wireVerdict.
// Settings match the llm package's reflector.
func reflectWireVerdictSchema() map[string]any {
	r := &jsonschema.Reflector{
		AllowAdditionalProperties: false,
		DoNotReference:            true,
	}
	s := r.Reflect(&wireVerdict{})
	b, _ := json.Marshal(s)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	delete(m, "$schema")
	delete(m, "$id")
	return m
}
