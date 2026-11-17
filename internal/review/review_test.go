package review

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/bluesky-social/roast/internal/llm"
)

// TestSubmitDescriptionSurveyAsksForVolume pins the output-side counterpart to
// the survey prompt's breadth requirement. Survey mode must ask for everything
// grounded and invite low-confidence submissions; diff mode must not, since a
// small changed-line set legitimately yields few findings.
func TestSubmitDescriptionSurveyAsksForVolume(t *testing.T) {
	survey := submitDescription(true)
	for _, want := range []string{"EVERY", "confidence", "verifier"} {
		if !strings.Contains(survey, want) {
			t.Errorf("survey submit description must mention %q, got: %s", want, survey)
		}
	}
	if !strings.Contains(survey, "code_quote") {
		t.Errorf("survey submit description must keep the grounding requirement, got: %s", survey)
	}

	diff := submitDescription(false)
	if strings.Contains(diff, "EVERY") {
		t.Errorf("diff-mode submit description must not push for volume, got: %s", diff)
	}
	if len(diff) >= len(survey) {
		t.Error("diff-mode submit description should be the short one")
	}
}

// ---------------------------------------------------------------------------
// ClassifyMode

func TestClassifyMode(t *testing.T) {
	cases := []struct {
		name  string
		files []string
		want  string
	}{
		{"empty nil", nil, "code"},
		{"empty slice", []string{}, "code"},
		{"all code", []string{"main.go", "util.go"}, "code"},
		{"all prose", []string{"README.md", "docs/plan.md", "notes.txt"}, "prose"},
		{"mixed", []string{"main.go", "README.md"}, "mixed"},
		{"unknown extension counts as code", []string{"Makefile", "README.md"}, "mixed"},
		{"all prose extensions", []string{
			"a.md", "b.mdx", "c.markdown", "d.rst",
			"e.txt", "f.adoc", "g.asciidoc", "h.org", "i.tex",
		}, "prose"},
		{"case insensitive", []string{"README.MD", "notes.TXT"}, "prose"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyMode(tc.files)
			if got != tc.want {
				t.Errorf("ClassifyMode(%v) = %q, want %q", tc.files, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ClassifyLangs

func TestClassifyLangs(t *testing.T) {
	cases := []struct {
		name  string
		files []string
		want  []string
	}{
		{"empty nil", nil, []string{}},
		{"empty slice", []string{}, []string{}},
		{"all go", []string{"main.go", "util.go"}, []string{"go"}},
		{"react tsx", []string{"App.tsx"}, []string{"react"}},
		{"react jsx", []string{"App.jsx"}, []string{"react"}},
		{"frontend vue", []string{"App.vue"}, []string{"frontend"}},
		{"frontend svelte", []string{"App.svelte"}, []string{"frontend"}},
		{"frontend astro", []string{"App.astro"}, []string{"frontend"}},
		{"web ts and css", []string{"index.ts", "styles.css"}, []string{"web"}},
		{"mixed families sorted", []string{"main.go", "App.tsx"}, []string{"go", "react"}},
		{"unknown extension contributes nothing", []string{"Makefile"}, []string{}},
		{"case insensitive", []string{"App.TSX"}, []string{"react"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyLangs(tc.files)
			if len(got) != len(tc.want) {
				t.Fatalf("ClassifyLangs(%v) = %v, want %v", tc.files, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("ClassifyLangs(%v) = %v, want %v", tc.files, got, tc.want)
					break
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Validate

// goodWireFinding is a finding that survives all gates.
var goodWireFinding = WireFinding{
	Reasoning:   "r",
	Severity:    SeverityHigh,
	Confidence:  0.9,
	Description: "d",
	File:        "f.go",
	Lines:       []int{1},
	CodeQuote:   "x := 1",
	FixedCode:   "x := 2",
}

func TestFrontendCategoryWireSchemaAndConversion(t *testing.T) {
	for _, category := range []string{"none", "", "security"} {
		got := (WireFinding{FrontendCategory: category}).ToFinding().FrontendCategory
		want := category
		if category == "none" {
			want = ""
		}
		if got != want {
			t.Errorf("category %q converted to %q, want %q", category, got, want)
		}
	}

	schema := reflectSchema(&FindingsResponse{})
	properties := schema["properties"].(map[string]any)
	findings := properties["findings"].(map[string]any)
	item := findings["items"].(map[string]any)
	category := item["properties"].(map[string]any)["frontend_category"].(map[string]any)
	seenNone := false
	for _, value := range category["enum"].([]any) {
		if value == "" {
			t.Fatal("frontend category enum contains an empty value")
		}
		seenNone = seenNone || value == "none"
	}
	if !seenNone {
		t.Fatal("frontend category enum must allow none for non-frontend findings")
	}
}

func TestValidateDropReasons(t *testing.T) {
	t.Run("all good", func(t *testing.T) {
		resp := FindingsResponse{Assessment: AssessmentFindings, Findings: []WireFinding{goodWireFinding}}
		r := Validate(resp, 0.6, nil)
		if len(r.Findings) != 1 {
			t.Fatalf("want 1 finding, got %d", len(r.Findings))
		}
		if len(r.Dropped) != 0 {
			t.Fatalf("want 0 dropped, got %d", len(r.Dropped))
		}
		if r.Assessment != AssessmentFindings {
			t.Errorf("want FINDINGS_PRESENT, got %q", r.Assessment)
		}
	})

	t.Run("missing_code_quote empty string", func(t *testing.T) {
		f := goodWireFinding
		f.CodeQuote = ""
		resp := FindingsResponse{Assessment: AssessmentFindings, Findings: []WireFinding{f}}
		r := Validate(resp, 0.6, nil)
		if len(r.Findings) != 0 {
			t.Fatalf("want 0 findings, got %d", len(r.Findings))
		}
		if len(r.Dropped) != 1 {
			t.Fatalf("want 1 dropped, got %d", len(r.Dropped))
		}
		if r.Dropped[0].DropReason != DropMissingCodeQuote {
			t.Errorf("want drop reason %q, got %q", DropMissingCodeQuote, r.Dropped[0].DropReason)
		}
		if r.Assessment != AssessmentNoFindings {
			t.Errorf("want NO_FINDINGS after all dropped, got %q", r.Assessment)
		}
	})

	t.Run("missing_code_quote whitespace only", func(t *testing.T) {
		f := goodWireFinding
		f.CodeQuote = "   \t\n  "
		resp := FindingsResponse{Findings: []WireFinding{f}}
		r := Validate(resp, 0.6, nil)
		if len(r.Dropped) != 1 || r.Dropped[0].DropReason != DropMissingCodeQuote {
			t.Errorf("want missing_code_quote drop, got dropped=%v", r.Dropped)
		}
	})

	t.Run("missing_fixed_code", func(t *testing.T) {
		f := goodWireFinding
		f.FixedCode = ""
		resp := FindingsResponse{Assessment: AssessmentFindings, Findings: []WireFinding{f}}
		r := Validate(resp, 0.6, nil)
		if len(r.Dropped) != 1 || r.Dropped[0].DropReason != DropMissingFixedCode {
			t.Errorf("want drop reason %q, got %v", DropMissingFixedCode, r.Dropped)
		}
	})

	t.Run("below_confidence_floor", func(t *testing.T) {
		f := goodWireFinding
		f.Confidence = 0.5
		resp := FindingsResponse{Assessment: AssessmentFindings, Findings: []WireFinding{f}}
		r := Validate(resp, 0.6, nil)
		if len(r.Dropped) != 1 || r.Dropped[0].DropReason != DropBelowConfidenceFloor {
			t.Errorf("want drop reason %q, got %v", DropBelowConfidenceFloor, r.Dropped)
		}
	})

	t.Run("confidence exactly at floor survives", func(t *testing.T) {
		f := goodWireFinding
		f.Confidence = 0.6
		resp := FindingsResponse{Findings: []WireFinding{f}}
		r := Validate(resp, 0.6, nil)
		if len(r.Findings) != 1 {
			t.Fatalf("want 1 finding at exact floor, got %d", len(r.Findings))
		}
	})

	t.Run("assessment recomputed to NO_FINDINGS when all drop", func(t *testing.T) {
		f := goodWireFinding
		f.CodeQuote = ""
		resp := FindingsResponse{Assessment: AssessmentFindings, Findings: []WireFinding{f}}
		r := Validate(resp, 0.6, nil)
		if r.Assessment != AssessmentNoFindings {
			t.Errorf("want NO_FINDINGS when all dropped, got %q", r.Assessment)
		}
	})

	t.Run("NO_FINDINGS assessment preserved when no findings", func(t *testing.T) {
		resp := FindingsResponse{Assessment: AssessmentNoFindings}
		r := Validate(resp, 0.6, nil)
		if r.Assessment != AssessmentNoFindings {
			t.Errorf("want NO_FINDINGS, got %q", r.Assessment)
		}
	})

	t.Run("ungrounded_code_quote", func(t *testing.T) {
		resp := FindingsResponse{Assessment: AssessmentFindings, Findings: []WireFinding{goodWireFinding}}
		r := Validate(resp, 0.6, func(file, quote string) bool { return false })
		if len(r.Dropped) != 1 || r.Dropped[0].DropReason != DropUngroundedCodeQuote {
			t.Errorf("want drop reason %q, got %v", DropUngroundedCodeQuote, r.Dropped)
		}
		if r.Assessment != AssessmentNoFindings {
			t.Errorf("want NO_FINDINGS after grounding drop, got %q", r.Assessment)
		}
	})

	t.Run("identity assigned to kept and dropped findings", func(t *testing.T) {
		bad := goodWireFinding
		bad.FixedCode = ""
		resp := FindingsResponse{Findings: []WireFinding{goodWireFinding, bad}}
		r := Validate(resp, 0.6, nil)
		if len(r.Findings) != 1 || len(r.Dropped) != 1 {
			t.Fatalf("want 1 kept + 1 dropped, got %d + %d", len(r.Findings), len(r.Dropped))
		}
		kept, dropped := r.Findings[0], r.Dropped[0]
		for _, f := range []Finding{kept, dropped} {
			if len(f.ID) != 36 {
				t.Errorf("want UUID id, got %q", f.ID)
			}
			if !strings.HasPrefix(f.Alias, "R-") || len(f.Alias) < 8 {
				t.Errorf("want R-xxxxxx alias, got %q", f.Alias)
			}
			if !strings.HasPrefix(strings.ReplaceAll(f.ID, "-", ""), f.Alias[2:]) {
				t.Errorf("alias %q must be a prefix of id %q", f.Alias, f.ID)
			}
		}
		if kept.ID == dropped.ID || kept.Alias == dropped.Alias {
			t.Error("identities must be unique within a run")
		}
	})

	t.Run("grounded finding passes with grounder set", func(t *testing.T) {
		resp := FindingsResponse{Findings: []WireFinding{goodWireFinding}}
		var gotFile, gotQuote string
		r := Validate(resp, 0.6, func(file, quote string) bool {
			gotFile, gotQuote = file, quote
			return true
		})
		if len(r.Findings) != 1 {
			t.Fatalf("want 1 finding, got %d", len(r.Findings))
		}
		if gotFile != goodWireFinding.File || gotQuote != goodWireFinding.CodeQuote {
			t.Errorf("grounder called with (%q, %q), want (%q, %q)",
				gotFile, gotQuote, goodWireFinding.File, goodWireFinding.CodeQuote)
		}
	})

	t.Run("mixed good and bad findings", func(t *testing.T) {
		bad := goodWireFinding
		bad.CodeQuote = ""
		resp := FindingsResponse{
			Assessment: AssessmentFindings,
			Findings:   []WireFinding{goodWireFinding, bad},
		}
		r := Validate(resp, 0.6, nil)
		if len(r.Findings) != 1 {
			t.Errorf("want 1 surviving finding, got %d", len(r.Findings))
		}
		if len(r.Dropped) != 1 {
			t.Errorf("want 1 dropped, got %d", len(r.Dropped))
		}
	})
}

// ---------------------------------------------------------------------------
// EffectiveSeverity / MaxSeverity recalibration

// TestValidateCanonicalizesSeverity pins the gate bypass fix: providers that do
// not enforce the schema enum can return "high", which Rank() would score as
// INFO and a --max-severity HIGH gate would pass.
func TestValidateCanonicalizesSeverity(t *testing.T) {
	var findings []WireFinding
	for _, sev := range []Severity{"high", " Critical ", "medium", "SEVERE", ""} {
		f := goodWireFinding
		f.Severity = sev
		f.CodeQuote = "quote for " + string(sev)
		findings = append(findings, f)
	}
	r := Validate(FindingsResponse{Assessment: AssessmentFindings, Findings: findings}, 0.6, nil)

	var kept []Severity
	for _, f := range r.Findings {
		kept = append(kept, f.Severity)
	}
	if got := fmt.Sprint(kept); got != "[HIGH CRITICAL MEDIUM]" {
		t.Errorf("kept severities = %s, want canonical [HIGH CRITICAL MEDIUM]", got)
	}
	if r.MaxSeverity() != SeverityCritical.Rank() {
		t.Errorf("MaxSeverity = %d, want CRITICAL's rank", r.MaxSeverity())
	}
	if len(r.Dropped) != 2 {
		t.Fatalf("want the unknown and empty severities dropped, got %v", r.Dropped)
	}
	for _, f := range r.Dropped {
		if f.DropReason != DropInvalidSeverity {
			t.Errorf("severity %q dropped as %q, want %q", f.Severity, f.DropReason, DropInvalidSeverity)
		}
	}

	only := goodWireFinding
	only.Severity = "high"
	gate := Validate(FindingsResponse{Findings: []WireFinding{only}}, 0.6, nil)
	if gate.MaxSeverity() < SeverityHigh.Rank() {
		t.Error(`a lowercase "high" finding must trip a HIGH gate`)
	}
}

func TestParseSeverityIgnoresCase(t *testing.T) {
	for in, want := range map[string]Severity{"high": SeverityHigh, "Low": SeverityLow, " INFO ": SeverityInfo} {
		if got, ok := ParseSeverity(in); !ok || got != want {
			t.Errorf("ParseSeverity(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "SEVERE", "hi gh"} {
		if _, ok := ParseSeverity(in); ok {
			t.Errorf("ParseSeverity(%q) should be rejected", in)
		}
	}
}

func TestEffectiveSeverity(t *testing.T) {
	cases := []struct {
		name    string
		finding Finding
		want    Severity
	}{
		{
			name: "recalibrated applies: confirmed + strictly lower",
			finding: Finding{
				Severity: SeverityHigh,
				Verdict: &Verdict{
					Status:               VerdictConfirmed,
					RecalibratedSeverity: SeverityLow,
				},
			},
			want: SeverityLow,
		},
		{
			name: "ignored: recalibrated higher than claimed",
			finding: Finding{
				Severity: SeverityLow,
				Verdict: &Verdict{
					Status:               VerdictConfirmed,
					RecalibratedSeverity: SeverityHigh,
				},
			},
			want: SeverityLow,
		},
		{
			name: "ignored: recalibrated equal to claimed",
			finding: Finding{
				Severity: SeverityHigh,
				Verdict: &Verdict{
					Status:               VerdictConfirmed,
					RecalibratedSeverity: SeverityHigh,
				},
			},
			want: SeverityHigh,
		},
		{
			name: "ignored: recalibrated empty",
			finding: Finding{
				Severity: SeverityHigh,
				Verdict: &Verdict{
					Status:               VerdictConfirmed,
					RecalibratedSeverity: "",
				},
			},
			want: SeverityHigh,
		},
		{
			name: "ignored: recalibrated invalid value",
			finding: Finding{
				Severity: SeverityHigh,
				Verdict: &Verdict{
					Status:               VerdictConfirmed,
					RecalibratedSeverity: Severity("NOT_A_SEVERITY"),
				},
			},
			want: SeverityHigh,
		},
		{
			name: "ignored: status refuted",
			finding: Finding{
				Severity: SeverityHigh,
				Verdict: &Verdict{
					Status:               VerdictRefuted,
					RecalibratedSeverity: SeverityLow,
				},
			},
			want: SeverityHigh,
		},
		{
			name: "ignored: status insufficient_context (fail-open, no weakening)",
			finding: Finding{
				Severity: SeverityCritical,
				Verdict: &Verdict{
					Status:               VerdictInsufficientContext,
					RecalibratedSeverity: SeverityLow,
				},
			},
			want: SeverityCritical,
		},
		{
			name:    "ignored: nil verdict",
			finding: Finding{Severity: SeverityHigh, Verdict: nil},
			want:    SeverityHigh,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.finding.EffectiveSeverity()
			if got != tc.want {
				t.Errorf("EffectiveSeverity() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMaxSeverity_UsesEffectiveSeverity(t *testing.T) {
	t.Run("recalibrated HIGH-to-LOW does not trip a HIGH gate", func(t *testing.T) {
		r := &Result{Findings: []Finding{{
			Severity: SeverityHigh,
			Verdict: &Verdict{
				Status:               VerdictConfirmed,
				RecalibratedSeverity: SeverityLow,
			},
		}}}
		if got := r.MaxSeverity(); got != SeverityLow.Rank() {
			t.Errorf("MaxSeverity() = %d, want %d (LOW)", got, SeverityLow.Rank())
		}
	})

	t.Run("un-recalibrated HIGH still trips the gate", func(t *testing.T) {
		r := &Result{Findings: []Finding{{
			Severity: SeverityHigh,
			Verdict: &Verdict{
				Status: VerdictConfirmed,
			},
		}}}
		if got := r.MaxSeverity(); got != SeverityHigh.Rank() {
			t.Errorf("MaxSeverity() = %d, want %d (HIGH)", got, SeverityHigh.Rank())
		}
	})

	t.Run("no findings", func(t *testing.T) {
		r := &Result{}
		if got := r.MaxSeverity(); got != -1 {
			t.Errorf("MaxSeverity() = %d, want -1", got)
		}
	})
}

// ---------------------------------------------------------------------------
// Find — stub llm.Client for unit tests

// stubClient implements llm.Client for testing. It returns pre-canned results
// via JSON round-trip so that the stub exercises the same unmarshal path the
// real client would.
type stubClient struct {
	name      string
	structOut any   // returned by CompleteStructured
	convOut   any   // returned by Converse when convErr is nil
	convErr   error // if non-nil, Converse returns this
	// driveTools, when non-nil, is called with the Converse tool set before
	// returning — lets a test invoke report_finding like a model would.
	driveTools func(tools []llm.Tool)
}

func (s *stubClient) Name() string     { return s.name }
func (s *stubClient) Usage() llm.Usage { return llm.Usage{} }

func (s *stubClient) CompleteStructured(_ context.Context, _, _ string, _ string, out any) error {
	return roundTripJSON(s.structOut, out)
}

func (s *stubClient) Converse(_ context.Context, _, _ string, opts llm.ConverseOpts, out any) error {
	if s.driveTools != nil {
		s.driveTools(opts.Tools)
	}
	if s.convErr != nil {
		return s.convErr
	}
	return roundTripJSON(s.convOut, out)
}

// callTool invokes the named tool from a Converse tool set with args, as the
// model would.
func callTool(t *testing.T, tools []llm.Tool, name string, args any) {
	t.Helper()
	for _, tool := range tools {
		if tool.Name != name {
			continue
		}
		var m map[string]any
		if err := roundTripJSON(args, &m); err != nil {
			t.Fatalf("marshal tool args: %v", err)
		}
		if _, err := tool.Run(context.Background(), m); err != nil {
			t.Fatalf("%s run: %v", name, err)
		}
		return
	}
	t.Fatalf("tool %q not in tool set", name)
}

// roundTripJSON marshals src and unmarshals into dst, simulating the wire path.
func roundTripJSON(src, dst any) error {
	b, err := json.Marshal(src)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}

func TestFind_SingleShot(t *testing.T) {
	resp := FindingsResponse{
		Assessment: AssessmentFindings,
		Findings:   []WireFinding{goodWireFinding},
	}
	c := &stubClient{name: "stub", structOut: resp}
	result, err := Find(context.Background(), c, FindInput{
		System:          "sys",
		User:            "usr",
		ConfidenceFloor: 0.6,
	})
	if err != nil {
		t.Fatalf("Find error: %v", err)
	}
	if len(result.Findings) != 1 {
		t.Errorf("want 1 finding, got %d", len(result.Findings))
	}
}

func TestFind_Interactive_MaxTurns(t *testing.T) {
	c := &stubClient{name: "stub", convErr: llm.ErrNoFinalCall}
	result, err := Find(context.Background(), c, FindInput{
		Interactive:     true,
		MaxTurns:        3,
		ConfidenceFloor: 0.6,
	})
	if err != nil {
		t.Fatalf("ErrNoFinalCall should not propagate as error, got: %v", err)
	}
	// ErrNoFinalCall maps to INCOMPLETE_NO_SUBMISSION, never NO_FINDINGS: an
	// unsubmitted run is visibly incomplete (coverage contract).
	if result.Assessment != AssessmentIncompleteNoSubmission {
		t.Errorf("want INCOMPLETE_NO_SUBMISSION assessment, got %q", result.Assessment)
	}
	if result.Assessment == AssessmentNoFindings {
		t.Error("must never convert an unsubmitted run to NO_FINDINGS")
	}
	if len(result.Notes) == 0 {
		t.Error("want at least one note for the stop reason")
	}
}

func TestFind_Interactive_OK(t *testing.T) {
	resp := FindingsResponse{
		Assessment: AssessmentFindings,
		Findings:   []WireFinding{goodWireFinding},
	}
	c := &stubClient{name: "stub", convOut: resp}
	result, err := Find(context.Background(), c, FindInput{
		Interactive:     true,
		MaxTurns:        6,
		ConfidenceFloor: 0.6,
	})
	if err != nil {
		t.Fatalf("Find interactive error: %v", err)
	}
	if len(result.Findings) != 1 {
		t.Errorf("want 1 finding, got %d", len(result.Findings))
	}
}

func TestFind_Interactive_IncrementalCaptureSurvivesCutoff(t *testing.T) {
	var captured []WireFinding
	c := &stubClient{
		name:    "stub",
		convErr: llm.ErrNoFinalCall,
		driveTools: func(tools []llm.Tool) {
			callTool(t, tools, "report_finding", goodWireFinding)
		},
	}
	result, err := Find(context.Background(), c, FindInput{
		Interactive:     true,
		MaxTurns:        3,
		ConfidenceFloor: 0.6,
		Capture:         func(wf WireFinding) { captured = append(captured, wf) },
	})
	if err != nil {
		t.Fatalf("Find error: %v", err)
	}
	// The reported finding survives the cutoff — and reached the spill.
	if len(result.Findings) != 1 {
		t.Fatalf("want 1 retained finding, got %d", len(result.Findings))
	}
	if len(captured) != 1 {
		t.Errorf("capture sink got %d findings, want 1", len(captured))
	}
	if result.Assessment == AssessmentNoFindings {
		t.Error("cutoff run must never read as clean NO_FINDINGS")
	}
	// Coverage contract: even with retained findings (FINDINGS_PRESENT),
	// the cutoff must stay machine-visible.
	if !result.Incomplete {
		t.Error("cutoff run must set Result.Incomplete")
	}
}

func TestFind_Interactive_CutoffWithAllReportedDroppedStaysIncomplete(t *testing.T) {
	ungrounded := goodWireFinding
	c := &stubClient{
		name:    "stub",
		convErr: llm.ErrNoFinalCall,
		driveTools: func(tools []llm.Tool) {
			callTool(t, tools, "report_finding", ungrounded)
		},
	}
	result, err := Find(context.Background(), c, FindInput{
		Interactive:     true,
		MaxTurns:        3,
		ConfidenceFloor: 0.6,
		Ground:          func(file, quote string) bool { return false },
	})
	if err != nil {
		t.Fatalf("Find error: %v", err)
	}
	if result.Assessment != AssessmentIncompleteNoSubmission {
		t.Errorf("all-dropped cutoff must stay INCOMPLETE_NO_SUBMISSION, got %q", result.Assessment)
	}
}

func TestFind_Interactive_SubmitMergesReported(t *testing.T) {
	reportedOnly := goodWireFinding
	reportedOnly.File = "only-reported.go"
	submitted := goodWireFinding
	submitted.File = "submitted.go"
	c := &stubClient{
		name:    "stub",
		convOut: FindingsResponse{Assessment: AssessmentFindings, Findings: []WireFinding{submitted}},
		driveTools: func(tools []llm.Tool) {
			callTool(t, tools, "report_finding", reportedOnly)
		},
	}
	result, err := Find(context.Background(), c, FindInput{
		Interactive:     true,
		MaxTurns:        6,
		ConfidenceFloor: 0.6,
	})
	if err != nil {
		t.Fatalf("Find error: %v", err)
	}
	// Reported-but-unsubmitted findings are kept alongside the submission.
	if len(result.Findings) != 2 {
		t.Fatalf("want reported+submitted = 2 findings, got %d", len(result.Findings))
	}
}

func TestFindingAccumulator_DedupLastWins(t *testing.T) {
	acc := newFindingAccumulator(nil)
	first := goodWireFinding
	first.Severity = SeverityLow
	acc.add(first)
	second := goodWireFinding // same file+quote → same fingerprint
	second.Severity = SeverityHigh
	acc.add(second)
	got := acc.ordered()
	if len(got) != 1 {
		t.Fatalf("want dedup to 1 finding, got %d", len(got))
	}
	if got[0].Severity != SeverityHigh {
		t.Errorf("re-report must replace: want HIGH, got %s", got[0].Severity)
	}
}

func TestFind_PropagatesClientError(t *testing.T) {
	sentinel := context.DeadlineExceeded
	c := &stubClient{name: "stub", convErr: sentinel}
	_, err := Find(context.Background(), c, FindInput{
		Interactive:     true,
		ConfidenceFloor: 0.6,
	})
	if err != sentinel {
		t.Errorf("want propagated error %v, got %v", sentinel, err)
	}
}
