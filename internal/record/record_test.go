package record

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/roast/internal/render"
	"github.com/bluesky-social/roast/internal/review"
)

// ---------------------------------------------------------------------------
// Helpers

var goodFinding = review.Finding{
	Reasoning:   "r",
	Severity:    review.SeverityHigh,
	Confidence:  0.9,
	Description: "Nil pointer dereference in handler",
	File:        "main.go",
	Lines:       []int{10, 12},
	CodeQuote:   "x := foo()\nreturn x.Val",
	FixedCode:   "x := foo()\nif x == nil { return }\nreturn x.Val",
}

func makeReport(findings []review.Finding) render.Report {
	return render.Report{
		Result: &review.Result{
			Findings:   findings,
			Dropped:    []review.Finding{},
			Assessment: review.AssessmentFindings,
			Notes:      []string{},
		},
		Mode:          "code",
		FilesReviewed: []string{"main.go"},
		Task:          "t",
		Quick:         false,
		Verified:      true,
	}
}

// setDir overrides ROAST_RECORD_DIR and returns a cleanup function.
func setDir(t *testing.T, dir string) func() {
	t.Helper()
	prev := os.Getenv("ROAST_RECORD_DIR")
	_ = os.Setenv("ROAST_RECORD_DIR", dir)
	return func() { _ = os.Setenv("ROAST_RECORD_DIR", prev) }
}

// ---------------------------------------------------------------------------
// Record

func TestRecord_WritesJSONL(t *testing.T) {
	dir := t.TempDir()
	defer setDir(t, dir)()

	Record(makeReport([]review.Finding{goodFinding}), "/repo", []string{"roast", "review"})

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 file, got %d", len(entries))
	}
	b, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 1 {
		t.Fatalf("want 1 JSONL line, got %d", len(lines))
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &m); err != nil {
		t.Fatalf("invalid JSON in record: %v", err)
	}
	if m["assessment"] != review.AssessmentFindings {
		t.Errorf("want assessment %q, got %v", review.AssessmentFindings, m["assessment"])
	}
	if m["repo"] != "/repo" {
		t.Errorf("want repo /repo, got %v", m["repo"])
	}
}

func TestFindingSpillUsesReportCategory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROAST_RECORD_DIR", dir)
	spill := OpenFindingSpill("category-test")
	if spill == nil {
		t.Fatal("no finding sink")
	}
	for _, category := range []string{"none", "security"} {
		spill(review.WireFinding{FrontendCategory: category})
	}
	data, err := os.ReadFile(filepath.Join(dir, "runs", "category-test", "findings.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d entries, want 2", len(lines))
	}
	for i, want := range []string{"", "security"} {
		var entry struct {
			Finding review.WireFinding `json:"finding"`
		}
		if err := json.Unmarshal([]byte(lines[i]), &entry); err != nil {
			t.Fatal(err)
		}
		if entry.Finding.FrontendCategory != want {
			t.Errorf("entry %d category = %q, want %q", i, entry.Finding.FrontendCategory, want)
		}
	}
}

func TestRecord_PromptPacks(t *testing.T) {
	dir := t.TempDir()
	defer setDir(t, dir)()

	report := makeReport([]review.Finding{goodFinding})
	report.PromptPacks = []string{"go", "react"}
	Record(report, "/repo", nil)

	entries, _ := os.ReadDir(dir)
	b, _ := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("invalid JSON in record: %v", err)
	}
	packs, ok := m["prompt_packs"].([]any)
	if !ok || len(packs) != 2 || packs[0] != "go" || packs[1] != "react" {
		t.Errorf("want prompt_packs [go react], got %v", m["prompt_packs"])
	}
}

func TestRecord_FingerprintInFindings(t *testing.T) {
	dir := t.TempDir()
	defer setDir(t, dir)()

	Record(makeReport([]review.Finding{goodFinding}), "/repo", nil)

	entries, _ := os.ReadDir(dir)
	b, _ := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	var m map[string]any
	_ = json.Unmarshal(b, &m)

	findings := m["findings"].([]any)
	f0 := findings[0].(map[string]any)
	fp, ok := f0["fingerprint"].(string)
	if !ok || len(fp) != 16 {
		t.Errorf("want 16-char fingerprint, got %q", fp)
	}
}

func TestRecord_MonthlyFilename(t *testing.T) {
	dir := t.TempDir()
	defer setDir(t, dir)()

	Record(makeReport([]review.Finding{}), "/r", nil)

	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("want 1 file, got %d", len(entries))
	}
	name := entries[0].Name()
	month := time.Now().Format("2006-01")
	if !strings.HasPrefix(name, "runs-"+month) {
		t.Errorf("want filename starting with runs-%s, got %q", month, name)
	}
}

func TestRecord_Appends(t *testing.T) {
	dir := t.TempDir()
	defer setDir(t, dir)()

	Record(makeReport([]review.Finding{}), "/r", nil)
	Record(makeReport([]review.Finding{}), "/r", nil)

	entries, _ := os.ReadDir(dir)
	b, _ := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Errorf("want 2 appended lines, got %d", len(lines))
	}
}

func TestRecord_SwallowsError_NoDir(t *testing.T) {
	// Set an unwritable dir path — Record should silently do nothing.
	defer setDir(t, "/nonexistent/roast-test-record-dir")()
	// Should not panic or return an error.
	Record(makeReport([]review.Finding{}), "/r", nil)
}

// ---------------------------------------------------------------------------
// SeenBefore

// writeFixture writes a fake JSONL line to dir/runs-YYYY-MM.jsonl.
// repo is stored in the "repo" field; use "" to leave it blank (simulates
// old records without a repo field — treated as any-repo by scanFile).
func writeFixture(t *testing.T, dir string, month string, ts string, repo string, findings []review.Finding, dropped []review.Finding) {
	t.Helper()
	type sf struct {
		Fingerprint string `json:"fingerprint"`
		DropReason  string `json:"_drop_reason"`
	}
	toSF := func(fs []review.Finding) []sf {
		out := make([]sf, len(fs))
		for i, f := range fs {
			out[i] = sf{Fingerprint: review.Fingerprint(f), DropReason: f.DropReason}
		}
		return out
	}
	line := map[string]any{
		"ts":               ts,
		"repo":             repo,
		"findings":         toSF(findings),
		"dropped_findings": toSF(dropped),
	}
	b, _ := json.Marshal(line)
	path := filepath.Join(dir, "runs-"+month+".jsonl")
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	defer func() { _ = f.Close() }()
	_, _ = f.Write(append(b, '\n'))
}

func TestSeenBefore_Match(t *testing.T) {
	dir := t.TempDir()
	defer setDir(t, dir)()

	repo := t.TempDir() // absolute path that matches what SeenBefore normalizes
	writeFixture(t, dir, "2026-06", "2026-06-30T10:00:00Z", repo, []review.Finding{goodFinding}, nil)

	sightings := SeenBefore(repo, []review.Finding{goodFinding}, 3)
	fp := review.Fingerprint(goodFinding)
	s, ok := sightings[fp]
	if !ok {
		t.Fatalf("want sighting for %q, got nothing. sightings=%v", fp, sightings)
	}
	if s.RunTS != "2026-06-30T10:00:00Z" {
		t.Errorf("want RunTS 2026-06-30T10:00:00Z, got %q", s.RunTS)
	}
}

func TestSeenBefore_NoMatch(t *testing.T) {
	dir := t.TempDir()
	defer setDir(t, dir)()

	repo := t.TempDir()
	other := goodFinding
	other.File = "other.go"
	writeFixture(t, dir, "2026-06", "2026-06-30T10:00:00Z", repo, []review.Finding{other}, nil)

	sightings := SeenBefore(repo, []review.Finding{goodFinding}, 3)
	if len(sightings) != 0 {
		t.Errorf("want empty sightings, got %v", sightings)
	}
}

func TestSeenBefore_CrossRepoFiltered(t *testing.T) {
	// A finding from a different repo with the same fingerprint must not
	// appear as a prior sighting.
	dir := t.TempDir()
	defer setDir(t, dir)()

	repo1 := t.TempDir()
	repo2 := t.TempDir()
	writeFixture(t, dir, "2026-06", "2026-06-30T10:00:00Z", repo1, []review.Finding{goodFinding}, nil)

	sightings := SeenBefore(repo2, []review.Finding{goodFinding}, 3)
	if len(sightings) != 0 {
		t.Errorf("want empty sightings for different repo, got %v", sightings)
	}
}

func TestSeenBefore_RespectsLookbackFiles(t *testing.T) {
	dir := t.TempDir()
	defer setDir(t, dir)()

	repo := t.TempDir()
	// Write two monthly files; finding is only in the older one.
	writeFixture(t, dir, "2026-05", "2026-05-01T00:00:00Z", repo, []review.Finding{goodFinding}, nil)
	writeFixture(t, dir, "2026-06", "2026-06-01T00:00:00Z", repo, []review.Finding{}, nil)

	// lookbackFiles=1 should only scan the newest file (2026-06), which lacks
	// the finding, so the result should be empty.
	sightings := SeenBefore(repo, []review.Finding{goodFinding}, 1)
	if len(sightings) != 0 {
		t.Errorf("want empty sightings with lookbackFiles=1 (older file not scanned), got %v", sightings)
	}

	// lookbackFiles=2 should find it.
	sightings = SeenBefore(repo, []review.Finding{goodFinding}, 2)
	if len(sightings) == 0 {
		t.Error("want sighting with lookbackFiles=2")
	}
}

func TestSeenBefore_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	defer setDir(t, dir)()

	sightings := SeenBefore("/repo", []review.Finding{goodFinding}, 3)
	if len(sightings) != 0 {
		t.Errorf("want empty sightings for empty dir, got %v", sightings)
	}
}

func TestSeenBefore_DroppedFindingsScanned(t *testing.T) {
	dir := t.TempDir()
	defer setDir(t, dir)()

	repo := t.TempDir()
	dropped := goodFinding
	dropped.DropReason = review.DropRefutedByVerifier
	writeFixture(t, dir, "2026-06", "2026-06-30T00:00:00Z", repo, nil, []review.Finding{dropped})

	sightings := SeenBefore(repo, []review.Finding{goodFinding}, 3)
	fp := review.Fingerprint(goodFinding)
	s, ok := sightings[fp]
	if !ok {
		t.Fatal("want sighting in dropped findings")
	}
	if s.DropReason != review.DropRefutedByVerifier {
		t.Errorf("want drop reason preserved, got %q", s.DropReason)
	}
}

func TestSeenBefore_NewestSightingWins(t *testing.T) {
	// Same fingerprint appears in two monthly files; the newer one should win.
	dir := t.TempDir()
	defer setDir(t, dir)()

	repo := t.TempDir()
	writeFixture(t, dir, "2026-05", "2026-05-01T00:00:00Z", repo, []review.Finding{goodFinding}, nil)
	writeFixture(t, dir, "2026-06", "2026-06-15T00:00:00Z", repo, []review.Finding{goodFinding}, nil)

	sightings := SeenBefore(repo, []review.Finding{goodFinding}, 3)
	fp := review.Fingerprint(goodFinding)
	s, ok := sightings[fp]
	if !ok {
		t.Fatal("want sighting found")
	}
	if s.RunTS != "2026-06-15T00:00:00Z" {
		t.Errorf("want newest RunTS 2026-06-15, got %q", s.RunTS)
	}
}

// ---------------------------------------------------------------------------
// Run artifacts (durable report plus run id)

func TestNewRunID_Shape(t *testing.T) {
	a, b := NewRunID(), NewRunID()
	for _, id := range []string{a, b} {
		// <RFC3339-basic-UTC>-<8 hex>, e.g. 20260726T193000Z-a3f1c92b
		if len(id) != 25 || id[16] != '-' {
			t.Errorf("run id %q not in <basic-utc>-<8hex> shape", id)
		}
	}
	if a == b {
		t.Error("consecutive run ids must differ")
	}
}

func TestWriteReport_ShowRoundTrip(t *testing.T) {
	dir := t.TempDir()
	defer setDir(t, dir)()

	id := NewRunID()
	path := WriteReport(id, "## report body\n")
	if path == "" {
		t.Fatal("WriteReport returned no path")
	}
	got, err := ReportPath(id)
	if err != nil || got != path {
		t.Fatalf("ReportPath(full id) = %q, %v; want %q", got, err, path)
	}
	// Unique prefix resolves; missing id errors.
	if got, err := ReportPath(id[:10]); err != nil || got != path {
		t.Errorf("ReportPath(prefix) = %q, %v; want %q", got, err, path)
	}
	if _, err := ReportPath("nope"); err == nil {
		t.Error("ReportPath must error for an unknown id")
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "## report body\n" {
		t.Errorf("artifact content = %q, %v", b, err)
	}
}
