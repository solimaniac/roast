// Package review owns the finding model and the finder stage.
//
// The forcing-functions are the heart of roast: every finding must carry a
// verbatim CodeQuote (anti-confabulation — if the code isn't really there,
// the finding dies) and a concrete FixedCode (anti-vagueness — "consider
// error handling" is not a finding). Both are enforced twice: in the prompt
// and again server-side in Validate.
package review

import "strings"

// Severity levels, ordered. String values match the wire schema and the
// v2 JSON output consumed by the pre-commit hook.
type Severity string

const (
	SeverityInfo     Severity = "INFO"
	SeverityLow      Severity = "LOW"
	SeverityMedium   Severity = "MEDIUM"
	SeverityHigh     Severity = "HIGH"
	SeverityCritical Severity = "CRITICAL"
)

// Rank returns the ordering of a severity (INFO=0 .. CRITICAL=4).
// Unknown severities rank as INFO.
func (s Severity) Rank() int {
	switch s {
	case SeverityLow:
		return 1
	case SeverityMedium:
		return 2
	case SeverityHigh:
		return 3
	case SeverityCritical:
		return 4
	default:
		return 0
	}
}

// ParseSeverity validates a severity name, ignoring case and surrounding
// space, and returns its canonical form. Anything else, including "", is
// rejected.
func ParseSeverity(s string) (Severity, bool) {
	switch sev := Severity(strings.ToUpper(strings.TrimSpace(s))); sev {
	case SeverityInfo, SeverityLow, SeverityMedium, SeverityHigh, SeverityCritical:
		return sev, true
	}
	return "", false
}

// Finding is one claim in a report. Its schema tags describe report values,
// including empty strings for unset fields. Provider tools use WireFinding
// instead, which excludes post-hoc fields and has its own model-facing schema.
type Finding struct {
	Reasoning        string   `json:"reasoning"`
	Severity         Severity `json:"severity" jsonschema:"enum=INFO,enum=LOW,enum=MEDIUM,enum=HIGH,enum=CRITICAL"`
	Confidence       float64  `json:"confidence" jsonschema:"minimum=0,maximum=1"`
	CWE              string   `json:"cwe" jsonschema_description:"CWE id when applicable, else empty string"`
	Description      string   `json:"description"`
	File             string   `json:"file"`
	Lines            []int    `json:"lines"`
	CodeQuote        string   `json:"code_quote" jsonschema_description:"Verbatim copy of the cited lines, exactly as they appear in <files>"`
	FixedCode        string   `json:"fixed_code" jsonschema_description:"Concrete replacement for code_quote that resolves the issue"`
	FrontendCategory string   `json:"frontend_category" jsonschema:"enum=,enum=hooks,enum=security,enum=a11y,enum=perf,enum=ts" jsonschema_description:"Which frontend-addendum dimension produced this finding; empty for non-frontend findings"`

	// Attached by roast after the model responds; never part of the
	// model-facing schema (populated post-hoc, json tags for output only).
	Verdict    *Verdict `json:"verdict,omitempty"`
	DropReason string   `json:"_drop_reason,omitempty"`

	// ID is the immutable identity of this finding. Assigned once in Validate,
	// random, never derived from content: the fingerprint is
	// similarity-only and deliberately tolerates collisions, so it must
	// never double as identity.
	ID string `json:"id,omitempty"`
	// Alias is the run-local display handle ("R-8d8d19"). It is a prefix of ID,
	// lengthened on within-run collision, and resolves to ID; it is never a
	// corpus-wide key.
	Alias string `json:"alias,omitempty"`
}

// VerdictStatus is the verifier's adjudication of one finding.
type VerdictStatus string

const (
	VerdictConfirmed VerdictStatus = "confirmed"
	VerdictRefuted   VerdictStatus = "refuted"
	// VerdictInsufficientContext is the escape hatch: the verifier could
	// not decide with the context available. Maps to fail-open — the
	// finding keeps gating, verdict attached.
	VerdictInsufficientContext VerdictStatus = "insufficient_context"
)

// Verdict is the grounded cross-family adjudication of one finding.
// The verifier sees the claim + the code ONLY — never the finder's
// reasoning (anti-anchoring). A refuted verdict counts only when
// Grounding cites the specific code/guard/semantics that kills the claim.
// Its schema tags describe report values; the verifier's model-facing schema
// is defined separately.
type Verdict struct {
	Reasoning            string        `json:"reasoning"`
	Status               VerdictStatus `json:"status" jsonschema:"enum=confirmed,enum=refuted,enum=insufficient_context"`
	Confidence           float64       `json:"confidence" jsonschema:"minimum=0,maximum=1"`
	Grounding            string        `json:"grounding" jsonschema_description:"The exact lines / contradicting code / language-semantics fact the verdict rests on"`
	RecalibratedSeverity Severity      `json:"recalibrated_severity,omitempty" jsonschema:"enum=,enum=INFO,enum=LOW,enum=MEDIUM,enum=HIGH,enum=CRITICAL" jsonschema_description:"If the finding is real but its severity is overstated, the corrected LOWER severity, justified in grounding. Leave empty to keep the finder's severity. Never raise severity."`

	// Set by roast, not the model.
	Verifier  string `json:"verifier,omitempty"`
	Undecided bool   `json:"undecided,omitempty"` // tool loop ended without a verdict; failed open
}

// Gates reports whether this verdict allows the finding to keep gating.
// Refuted findings drop; confirmed and insufficient_context (fail-open)
// keep gating. A nil verdict (verification skipped) also gates.
func (v *Verdict) Gates() bool {
	return v == nil || v.Status != VerdictRefuted
}

// EffectiveSeverity returns the severity that should gate and render for
// this finding. It is the verifier's recalibrated severity when the verifier
// confirmed the finding AND the recalibration is a valid severity strictly
// lower than the finder's claimed severity; otherwise it is the finder's
// original Severity. This is the only path a severity can move, and it can
// only move down — recalibration is never applied to refuted or
// insufficient_context verdicts, so a fail-open CRITICAL is never weakened.
func (f Finding) EffectiveSeverity() Severity {
	if f.Verdict == nil || f.Verdict.Status != VerdictConfirmed {
		return f.Severity
	}
	recal, ok := ParseSeverity(string(f.Verdict.RecalibratedSeverity))
	if !ok {
		return f.Severity
	}
	if recal.Rank() < f.Severity.Rank() {
		return recal
	}
	return f.Severity
}

// Drop reasons attached to findings that fell out of the pipeline. Dropped
// findings are kept (tagged) for --show-dropped rather than discarded —
// a dropped finding means the model ignored its instructions, which is
// signal.
const (
	DropInvalidSeverity      = "invalid_severity"
	DropMissingCodeQuote     = "missing_code_quote"
	DropMissingFixedCode     = "missing_fixed_code"
	DropUngroundedCodeQuote  = "ungrounded_code_quote"
	DropBelowConfidenceFloor = "below_confidence_floor"
	DropRefutedByVerifier    = "refuted_by_verifier"
)

// Assessment values for a whole run.
const (
	AssessmentFindings   = "FINDINGS_PRESENT"
	AssessmentNoFindings = "NO_FINDINGS"
	AssessmentNoDiff     = "NO_DIFF"
	// AssessmentIncompleteNoSubmission marks a budget-exhausted interactive
	// run that never submitted — even after the reserved final-turn flush.
	// Replaces the old "MAX_TURNS_REACHED" signal for all interactive runs.
	// It must NEVER be converted to NO_FINDINGS: an incomplete run that found
	// nothing recoverable is not the same as a clean review (survey coverage
	// contract).
	AssessmentIncompleteNoSubmission = "INCOMPLETE_NO_SUBMISSION"
	// AssessmentNoFindingsInSampledAreas is the survey-mode clean result: a
	// survey that finished (submitted) but found nothing in the areas it
	// sampled. Distinct from NO_FINDINGS so a triage is never
	// machine-indistinguishable from a complete clean review.
	AssessmentNoFindingsInSampledAreas = "NO_FINDINGS_IN_SAMPLED_AREAS"
)

// Result is what one review run produced.
type Result struct {
	Findings   []Finding `json:"findings"`
	Dropped    []Finding `json:"dropped_findings"`
	Assessment string    `json:"assessment"`
	Notes      []string  `json:"notes"`
	// Incomplete marks a run whose interactive loop ended without a final
	// submission (an early cutoff), independent of Assessment: when incrementally
	// reported findings survive the cutoff, Assessment is FINDINGS_PRESENT —
	// but the coverage contract still requires completion/stop_reason to say
	// the run was cut off, never "converged".
	Incomplete bool `json:"incomplete,omitempty"`
	// StopReason names why the run stopped early: "deadline" (work deadline
	// fired — set even when the wrap-up turn submitted successfully, since
	// exploration was still cut short) or "budget_exhausted" (turn budget
	// elapsed without submission). Empty for a normally-converged run.
	StopReason string `json:"stop_reason,omitempty"`
}

// MaxSeverity returns the highest-ranked effective severity among live
// (gating) findings, or -1 when there are none. Uses EffectiveSeverity so a
// finding the verifier grounds down below the gate threshold does not trip
// it.
func (r *Result) MaxSeverity() int {
	max := -1
	for _, f := range r.Findings {
		if rank := f.EffectiveSeverity().Rank(); rank > max {
			max = rank
		}
	}
	return max
}
