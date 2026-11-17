// Package record persists one JSONL line per review run and scans prior runs
// for matching finding fingerprints (dedup). It exposes checked writes for
// user-facing reviews and best-effort compatibility wrappers for local tools.
// The record dir defaults to ~/.local/share/roast; override with
// ROAST_RECORD_DIR.
package record

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bluesky-social/roast/internal/render"
	"github.com/bluesky-social/roast/internal/review"
)

// DefaultDir is the record directory: ROAST_RECORD_DIR if set, else
// the operating system's user-data directory. Returns "" if the directory
// can't be resolved; checked review writes reject that state, while
// compatibility wrappers treat it as recording disabled. Exported so the
// `runs` command resolves the same path instead of duplicating the logic.
func DefaultDir() string {
	if d := os.Getenv("ROAST_RECORD_DIR"); d != "" {
		return d
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	// Go does not expose UserDataDir. On Unix, keep durable run history under
	// XDG_DATA_HOME (or ~/.local/share) rather than mixing it with config.
	if data := os.Getenv("XDG_DATA_HOME"); data != "" {
		return filepath.Join(data, "roast")
	}
	if home, homeErr := os.UserHomeDir(); homeErr == nil && home != "" {
		return filepath.Join(home, ".local", "share", "roast")
	}
	return filepath.Join(dir, "roast", "runs-data")
}

// runLine is the JSON shape written to the JSONL file. It extends the Python
// v2 schema with a per-finding fingerprint field (populated inside each finding
// object) and mirrors all existing fields for backward compatibility.
type runLine struct {
	TS              string        `json:"ts"`
	RunID           string        `json:"run_id,omitempty"`
	Repo            string        `json:"repo"`
	Argv            []string      `json:"argv"`
	Mode            string        `json:"mode"`
	Quick           bool          `json:"quick"`
	Verified        bool          `json:"verified"`
	Assessment      string        `json:"assessment"`
	NFindings       int           `json:"n_findings"`
	NDropped        int           `json:"n_dropped"`
	NRefuted        int           `json:"n_refuted"`
	FilesReviewed   []string      `json:"files_reviewed"`
	Findings        []findingLine `json:"findings"`
	DroppedFindings []findingLine `json:"dropped_findings"`
	Usage           any           `json:"usage"`
	PromptPacks     []string      `json:"prompt_packs"`
	Budgets         any           `json:"budgets,omitempty"`
}

// findingLine is a review.Finding with a fingerprint field appended.
type findingLine struct {
	review.Finding
	Fingerprint string `json:"fingerprint"`
}

// NewRunID mints a run id in the `<RFC3339-basic-UTC>-<8 hex>` shape:
// lexically sortable, collision-free across concurrent runs, no clock-skew
// dependency beyond ordering. The snapshot ref and all local artifacts reuse
// the same identity.
func NewRunID() string {
	now := time.Now().UTC()
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Best-effort without collapsing every failed read to the same
		// constant suffix: a zero suffix would collide same-second runs and
		// WriteReport would truncate the earlier run's artifact. Mix process
		// identity with the high-resolution clock instead.
		n := uint32(now.UnixNano()) ^ uint32(os.Getpid())
		b = [4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}
	}
	return now.Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
}

// WriteReportRequired writes the rendered markdown report into the run's
// artifact directory and reports the real filesystem error. User-facing
// reviews use this to enforce the durable-report contract.
func WriteReportRequired(runID, markdown string) (string, error) {
	dir := DefaultDir()
	if dir == "" {
		return "", fmt.Errorf("resolve record directory (set ROAST_RECORD_DIR to a writable directory)")
	}
	if runID == "" {
		return "", fmt.Errorf("write report: empty run id")
	}
	runDir, err := makeRunDir(dir, runID)
	if err != nil {
		return "", err
	}
	path := filepath.Join(runDir, "report.md")
	if err := writePrivateFile(path, []byte(markdown)); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}

// WriteReport is the best-effort compatibility wrapper for local tools.
// User-facing review paths must call WriteReportRequired instead.
func WriteReport(runID, markdown string) string {
	path, _ := WriteReportRequired(runID, markdown)
	return path
}

// OpenFindingSpill returns a sink that appends one JSON line per
// incrementally-reported finding to runs/<runID>/findings.jsonl:
// findings live on disk the moment they are found, so even a SIGKILL of
// roast leaves them recoverable; a blown deadline or exhausted ledger loses
// only the un-reported tail. Best-effort like all record ops: returns nil
// when recording is unavailable and swallows write failures. Lines carry an
// arrival timestamp; a re-report of the same finding
// appends again — consumers dedup by fingerprint, last line wins.
func OpenFindingSpill(runID string) func(review.WireFinding) {
	dir := DefaultDir()
	if dir == "" || runID == "" {
		return nil
	}
	runDir, err := makeRunDir(dir, runID)
	if err != nil {
		return nil
	}
	path := filepath.Join(runDir, "findings.jsonl")
	return func(wf review.WireFinding) {
		finding := wf.ToFinding()
		// The spill is a report artifact, so keep its category in report form
		// instead of exposing the model-facing sentinel.
		wf.FrontendCategory = finding.FrontendCategory
		line := struct {
			TS          string             `json:"ts"`
			Fingerprint string             `json:"fingerprint"`
			Finding     review.WireFinding `json:"finding"`
		}{
			TS:          time.Now().UTC().Format(time.RFC3339Nano),
			Fingerprint: review.Fingerprint(finding),
			Finding:     wf,
		}
		b, err := json.Marshal(line)
		if err != nil {
			return
		}
		fh, err := openPrivateFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY)
		if err != nil {
			return
		}
		defer func() { _ = fh.Close() }()
		_, _ = fh.Write(append(b, '\n'))
	}
}

// ReportPath returns the report.md path for a run id, resolving a unique
// id prefix to the full id. Returns an error naming the ambiguity or absence
// otherwise.
func ReportPath(id string) (string, error) {
	runDir, err := RunDir(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(runDir, "report.md"), nil
}

// Record appends one JSON line for the run to the monthly JSONL file. It is
// strictly best-effort: all errors are swallowed.
func Record(report render.Report, repo string, argv []string) {
	dir := DefaultDir()
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	now := time.Now()

	marshalFindings := func(fs []review.Finding) []findingLine {
		out := make([]findingLine, len(fs))
		for i, f := range fs {
			out[i] = findingLine{Finding: f, Fingerprint: review.Fingerprint(f)}
		}
		return out
	}

	nRefuted := 0
	for _, f := range report.Result.Dropped {
		if f.DropReason == review.DropRefutedByVerifier {
			nRefuted++
		}
	}

	line := runLine{
		TS:              now.Format(time.RFC3339),
		RunID:           report.RunID,
		Repo:            repo,
		Argv:            argv,
		Mode:            report.Mode,
		Quick:           report.Quick,
		Verified:        report.Verified,
		Assessment:      report.Result.Assessment,
		NFindings:       len(report.Result.Findings),
		NDropped:        len(report.Result.Dropped),
		NRefuted:        nRefuted,
		FilesReviewed:   report.FilesReviewed,
		Findings:        marshalFindings(report.Result.Findings),
		DroppedFindings: marshalFindings(report.Result.Dropped),
		Usage:           report.Usage,
		PromptPacks:     report.PromptPacks,
		Budgets:         report.Budgets,
	}

	b, err := json.Marshal(line)
	if err != nil {
		return
	}

	outPath := filepath.Join(dir, "runs-"+now.Format("2006-01")+".jsonl")
	fh, err := openPrivateFile(outPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY)
	if err != nil {
		return
	}
	defer func() { _ = fh.Close() }()
	_, _ = fh.Write(append(b, '\n'))
}

// PriorSighting records where a matching fingerprint was last seen.
type PriorSighting struct {
	// RunTS is the RFC3339 timestamp of the run where the fingerprint was seen.
	RunTS string
	// DropReason is the _drop_reason field of the matching finding, if any.
	DropReason string
}

// SeenBefore scans the newest lookbackFiles monthly JSONL files for finding
// fingerprints that match any of findings in the given repoDir. Only records
// from the same repository are considered — cross-repo fingerprint collisions
// (two repos with identical relative paths and code) must not produce spurious
// prior sightings. Returns a map of fingerprint → PriorSighting for each match.
//
// lookbackFiles <= 0 defaults to 3. Errors scanning files return an empty map.
func SeenBefore(repoDir string, findings []review.Finding, lookbackFiles int) map[string]PriorSighting {
	if lookbackFiles <= 0 {
		lookbackFiles = 3
	}
	// Normalize repoDir so stored absolute paths compare correctly.
	if abs, err := filepath.Abs(repoDir); err == nil {
		repoDir = abs
	}

	dir := DefaultDir()
	if dir == "" {
		return nil
	}

	// Build the set of fingerprints we're looking for.
	targets := make(map[string]struct{}, len(findings))
	for _, f := range findings {
		targets[review.Fingerprint(f)] = struct{}{}
	}
	if len(targets) == 0 {
		return nil
	}

	// Find monthly files, sort newest-first, take up to lookbackFiles.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "runs-") && strings.HasSuffix(e.Name(), ".jsonl") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(files)))
	if len(files) > lookbackFiles {
		files = files[:lookbackFiles]
	}

	out := make(map[string]PriorSighting)
	for _, path := range files {
		scanFile(path, repoDir, targets, out)
		if len(out) == len(targets) {
			break // found everything; no need to scan older files
		}
	}
	return out
}

// scanFile reads one JSONL file and populates results with any fingerprint
// matches found in runs from repoDir. Only the first (newest within the file)
// match for each fingerprint is recorded — once a result is written, subsequent
// (older) lines cannot overwrite it, preserving newest-wins across files.
func scanFile(path string, repoDir string, targets map[string]struct{}, results map[string]PriorSighting) {
	fh, err := os.Open(path)
	if err != nil {
		return
	}
	defer func() { _ = fh.Close() }()

	// Minimal struct for scanning: ts, repo, and per-finding fingerprints.
	type scanFinding struct {
		Fingerprint string `json:"fingerprint"`
		DropReason  string `json:"_drop_reason"`
	}
	type scanLine struct {
		TS       string        `json:"ts"`
		Repo     string        `json:"repo"`
		Findings []scanFinding `json:"findings"`
		Dropped  []scanFinding `json:"dropped_findings"`
	}

	scanner := bufio.NewScanner(fh)
	scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)
	for scanner.Scan() {
		var line scanLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			continue
		}
		// Skip runs from other repositories.
		if repoDir != "" && line.Repo != repoDir {
			continue
		}
		for _, sf := range append(line.Findings, line.Dropped...) {
			if sf.Fingerprint == "" {
				continue
			}
			// Skip-if-already-present: files are scanned newest-first, so the
			// first write is always the newest sighting; older lines must not
			// overwrite it.
			if _, already := results[sf.Fingerprint]; already {
				continue
			}
			if _, wanted := targets[sf.Fingerprint]; wanted {
				results[sf.Fingerprint] = PriorSighting{
					RunTS:      line.TS,
					DropReason: sf.DropReason,
				}
			}
		}
	}
}
