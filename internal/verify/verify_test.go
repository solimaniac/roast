package verify

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bluesky-social/roast/internal/llm"
	"github.com/bluesky-social/roast/internal/review"
)

// ---------------------------------------------------------------------------
// Stub llm.Client

type stubClient struct {
	name    string
	results []stubResult // one per Converse call, in order
	mu      sync.Mutex
	calls   int
}

type stubResult struct {
	out any
	err error
	// delay simulates out-of-order completion for concurrency tests.
	delay time.Duration
}

func (s *stubClient) Name() string     { return s.name }
func (s *stubClient) Usage() llm.Usage { return llm.Usage{} }

func (s *stubClient) CompleteStructured(_ context.Context, _, _ string, _ string, _ any) error {
	return nil
}

func (s *stubClient) Converse(_ context.Context, _, _ string, _ llm.ConverseOpts, out any) error {
	s.mu.Lock()
	i := s.calls
	s.calls++
	s.mu.Unlock()

	if i >= len(s.results) {
		return nil
	}
	r := s.results[i]
	if r.delay > 0 {
		time.Sleep(r.delay)
	}
	if r.err != nil {
		return r.err
	}
	return roundTripJSON(r.out, out)
}

func roundTripJSON(src, dst any) error {
	b, err := json.Marshal(src)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}

// ---------------------------------------------------------------------------
// Helpers

var testFinding = review.Finding{
	Reasoning:   "r",
	Severity:    review.SeverityHigh,
	Confidence:  0.9,
	Description: "d",
	File:        "f.go",
	Lines:       []int{1, 2},
	CodeQuote:   "x := 1",
	FixedCode:   "x := 2",
}

func baseResult(findings ...review.Finding) *review.Result {
	return &review.Result{
		Findings:   findings,
		Dropped:    []review.Finding{},
		Notes:      []string{},
		Assessment: review.AssessmentFindings,
	}
}

func confirmedWire() wireVerdict {
	return wireVerdict{
		Reasoning:  "confirmed",
		Status:     review.VerdictConfirmed,
		Confidence: 0.9,
		Grounding:  "line 1",
	}
}

func TestVerdictSeverityWireSchemaAllowsOmissionWithoutEmptyEnum(t *testing.T) {
	schema := reflectWireVerdictSchema()
	for _, name := range schema["required"].([]any) {
		if name == "recalibrated_severity" {
			t.Fatal("recalibrated severity must be optional")
		}
	}
	properties := schema["properties"].(map[string]any)
	severity := properties["recalibrated_severity"].(map[string]any)
	for _, value := range severity["enum"].([]any) {
		if value == "" {
			t.Fatal("recalibrated severity enum contains an empty value")
		}
	}
}

func recalibratedWire(sev review.Severity) wireVerdict {
	return wireVerdict{
		Reasoning:            "confirmed but overstated",
		Status:               review.VerdictConfirmed,
		Confidence:           0.9,
		Grounding:            "guard limits blast radius",
		RecalibratedSeverity: sev,
	}
}

func refutedWire() wireVerdict {
	return wireVerdict{
		Reasoning:  "refuted",
		Status:     review.VerdictRefuted,
		Confidence: 0.8,
		Grounding:  "guard at line 5",
	}
}

func makeDeps(_ llm.Client) Deps {
	return Deps{
		SystemPrompt: "sys",
		UserPrompt:   func(_ review.Finding) string { return "usr" },
		Concurrency:  1,
	}
}

// ---------------------------------------------------------------------------
// Tests

func TestVerify_Confirmed_Kept(t *testing.T) {
	c := &stubClient{
		name:    "verifier",
		results: []stubResult{{out: confirmedWire()}},
	}
	out := Verify(context.Background(), c, baseResult(testFinding), makeDeps(c))
	if len(out.Findings) != 1 {
		t.Fatalf("want 1 finding, got %d", len(out.Findings))
	}
	if len(out.Dropped) != 0 {
		t.Errorf("want 0 dropped, got %d", len(out.Dropped))
	}
	v := out.Findings[0].Verdict
	if v == nil || v.Status != review.VerdictConfirmed {
		t.Errorf("want confirmed verdict, got %v", v)
	}
	if v.Verifier != "verifier" {
		t.Errorf("want verifier name set, got %q", v.Verifier)
	}
}

func TestVerify_Confirmed_RecalibratedSeverityFlowsThrough(t *testing.T) {
	c := &stubClient{
		name:    "verifier",
		results: []stubResult{{out: recalibratedWire(review.SeverityLow)}},
	}
	out := Verify(context.Background(), c, baseResult(testFinding), makeDeps(c))
	if len(out.Findings) != 1 {
		t.Fatalf("want 1 finding kept, got %d", len(out.Findings))
	}
	v := out.Findings[0].Verdict
	if v == nil || v.Status != review.VerdictConfirmed {
		t.Fatalf("want confirmed verdict, got %v", v)
	}
	if v.RecalibratedSeverity != review.SeverityLow {
		t.Errorf("want recalibrated_severity LOW to flow through the wire struct, got %q", v.RecalibratedSeverity)
	}
	if got := out.Findings[0].EffectiveSeverity(); got != review.SeverityLow {
		t.Errorf("want EffectiveSeverity() LOW after recalibration, got %q", got)
	}
}

func TestVerify_Confirmed_UngroundedRecalibration_Cleared(t *testing.T) {
	// A confirmed verdict with a recalibrated_severity but no grounding is not
	// a valid recalibration — the downgrade must be dropped and the finder's
	// original severity must stand.
	ungrounded := wireVerdict{
		Reasoning:            "confirmed but overstated, trust me",
		Status:               review.VerdictConfirmed,
		Confidence:           0.9,
		Grounding:            "", // no grounding — recalibration must not apply
		RecalibratedSeverity: review.SeverityLow,
	}
	c := &stubClient{
		name:    "verifier",
		results: []stubResult{{out: ungrounded}},
	}
	out := Verify(context.Background(), c, baseResult(testFinding), makeDeps(c))
	if len(out.Findings) != 1 {
		t.Fatalf("want finding kept, got %d", len(out.Findings))
	}
	v := out.Findings[0].Verdict
	if v == nil {
		t.Fatal("want verdict attached")
	}
	if v.RecalibratedSeverity != "" {
		t.Errorf("want RecalibratedSeverity cleared when ungrounded, got %q", v.RecalibratedSeverity)
	}
	if got := out.Findings[0].EffectiveSeverity(); got != testFinding.Severity {
		t.Errorf("want EffectiveSeverity() to fall back to finder severity %q, got %q", testFinding.Severity, got)
	}
}

func TestVerify_Refuted_Dropped(t *testing.T) {
	c := &stubClient{
		name:    "verifier",
		results: []stubResult{{out: refutedWire()}},
	}
	var logged []string
	d := makeDeps(c)
	d.Log = func(s string) { logged = append(logged, s) }

	out := Verify(context.Background(), c, baseResult(testFinding), d)
	if len(out.Findings) != 0 {
		t.Fatalf("want 0 findings after refutation, got %d", len(out.Findings))
	}
	if len(out.Dropped) != 1 {
		t.Fatalf("want 1 dropped, got %d", len(out.Dropped))
	}
	if out.Dropped[0].DropReason != review.DropRefutedByVerifier {
		t.Errorf("want drop reason %q, got %q", review.DropRefutedByVerifier, out.Dropped[0].DropReason)
	}
	if out.Assessment != review.AssessmentNoFindings {
		t.Errorf("want NO_FINDINGS after all refuted, got %q", out.Assessment)
	}
	if len(logged) == 0 {
		t.Error("want at least one log line for refuted finding")
	}
}

func TestVerify_Error_FailOpen(t *testing.T) {
	c := &stubClient{
		name:    "verifier",
		results: []stubResult{{err: llm.ErrNoFinalCall}},
	}
	out := Verify(context.Background(), c, baseResult(testFinding), makeDeps(c))
	if len(out.Findings) != 1 {
		t.Fatalf("want finding kept on error (fail-open), got %d", len(out.Findings))
	}
	v := out.Findings[0].Verdict
	if v == nil || !v.Undecided {
		t.Errorf("want Undecided verdict on error, got %v", v)
	}
	if v.Status != review.VerdictInsufficientContext {
		t.Errorf("want insufficient_context status, got %q", v.Status)
	}
}

func TestVerify_DeterministicOrder(t *testing.T) {
	// Five findings; completions arrive in reverse order via staggered delays.
	// Output order must match input order regardless of goroutine scheduling.
	n := 5
	findings := make([]review.Finding, n)
	for i := range findings {
		f := testFinding
		f.Description = string(rune('A' + i))
		findings[i] = f
	}

	results := make([]stubResult, n)
	for i := range results {
		// Findings with higher index complete first (shorter delay).
		delay := time.Duration(n-i) * 5 * time.Millisecond
		results[i] = stubResult{out: confirmedWire(), delay: delay}
	}
	c := &stubClient{name: "v", results: results}
	d := Deps{
		SystemPrompt: "sys",
		UserPrompt:   func(f review.Finding) string { return f.Description },
		Concurrency:  n,
	}
	out := Verify(context.Background(), c, baseResult(findings...), d)
	if len(out.Findings) != n {
		t.Fatalf("want %d findings, got %d", n, len(out.Findings))
	}
	for i, f := range out.Findings {
		want := string(rune('A' + i))
		if f.Description != want {
			t.Errorf("position %d: want description %q, got %q", i, want, f.Description)
		}
	}
}

func TestVerify_PreservesExistingDropped(t *testing.T) {
	preDropped := review.Finding{DropReason: review.DropMissingCodeQuote, Description: "pre-dropped"}
	input := &review.Result{
		Findings:   []review.Finding{testFinding},
		Dropped:    []review.Finding{preDropped},
		Notes:      []string{},
		Assessment: review.AssessmentFindings,
	}
	c := &stubClient{
		name:    "v",
		results: []stubResult{{out: confirmedWire()}},
	}
	out := Verify(context.Background(), c, input, makeDeps(c))
	found := false
	for _, dropped := range out.Dropped {
		if dropped.Description == "pre-dropped" {
			found = true
		}
	}
	if !found {
		t.Error("want pre-existing dropped finding preserved")
	}
}

func TestVerify_AssessmentKeptWhenFindingsRemain(t *testing.T) {
	c := &stubClient{
		name:    "v",
		results: []stubResult{{out: confirmedWire()}},
	}
	out := Verify(context.Background(), c, baseResult(testFinding), makeDeps(c))
	if out.Assessment != review.AssessmentFindings {
		t.Errorf("want FINDINGS_PRESENT, got %q", out.Assessment)
	}
}

func TestVerify_UngroundedRefutation_FailOpen(t *testing.T) {
	// A refuted verdict with empty Grounding must NOT drop the finding.
	// The grounding requirement is part of the verifier contract; ungrounded
	// refutations are demoted to insufficient_context and fail open.
	ungrounded := wireVerdict{
		Reasoning:  "looks fine to me",
		Status:     review.VerdictRefuted,
		Confidence: 0.9,
		Grounding:  "", // no grounding — must not drop
	}
	c := &stubClient{
		name:    "verifier",
		results: []stubResult{{out: ungrounded}},
	}
	out := Verify(context.Background(), c, baseResult(testFinding), makeDeps(c))
	if len(out.Findings) != 1 {
		t.Fatalf("want finding kept (ungrounded refutation = fail-open), got %d findings", len(out.Findings))
	}
	v := out.Findings[0].Verdict
	if v == nil {
		t.Fatal("want verdict attached")
	}
	if v.Status != review.VerdictInsufficientContext {
		t.Errorf("want status insufficient_context, got %q", v.Status)
	}
	if !v.Undecided {
		t.Error("want Undecided=true for ungrounded refutation")
	}
}

// TestVerify_RefutedLogEscapesFinderText pins the refuted-log line: the
// description is model-written from untrusted repository input, so it must
// reach stderr with no raw control bytes and no line break of its own.
func TestVerify_RefutedLogEscapesFinderText(t *testing.T) {
	c := &stubClient{name: "verifier", results: []stubResult{{out: refutedWire()}}}
	var logged []string
	d := makeDeps(c)
	d.Log = func(s string) { logged = append(logged, s) }

	f := testFinding
	f.Severity = "HIGH\x1b[0m"
	f.Description = "oops\x1b]0;pwned\x07\nroast: verifier confirmed everything ✓✓✓✓✓✓✓✓✓✓✓✓✓✓✓✓✓✓✓✓✓✓✓✓✓✓✓✓✓✓"
	Verify(context.Background(), c, baseResult(f), d)
	if len(logged) != 1 {
		t.Fatalf("want one log line, got %q", logged)
	}
	line := logged[0]
	for _, r := range line {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) || r == utf8.RuneError {
			t.Fatalf("log line holds %U: %q", r, line)
		}
	}
	if !strings.Contains(line, `\x1b]0;pwned\x07\nroast: verifier`) {
		t.Errorf("escapes must stay visible: %q", line)
	}
}
