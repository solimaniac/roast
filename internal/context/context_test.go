package context

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/roast/internal/snapshot"
)

// initRepo creates a temp git repo with user identity configured.
func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmds := [][]string{
		{"git", "-C", dir, "init"},
		{"git", "-C", dir, "config", "user.email", "test@example.com"},
		{"git", "-C", dir, "config", "user.name", "Test"},
	}
	for _, c := range cmds {
		if out, err := exec.Command(c[0], c[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("setup %v: %v\n%s", c, err, out)
		}
	}
	return dir
}

func writeFile(t *testing.T, repo, rel, content string) {
	t.Helper()
	full := filepath.Join(repo, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitAdd(t *testing.T, repo string, paths ...string) {
	t.Helper()
	args := append([]string{"-C", repo, "add"}, paths...)
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
}

func gitCommit(t *testing.T, repo, msg string) {
	t.Helper()
	if out, err := exec.Command("git", "-C", repo, "commit", "-m", msg).CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
}

// TestResolveUnderRepo_PathEscape verifies that .., absolute paths, and
// symlinks pointing outside the repo are all rejected.
func TestResolveUnderRepo_PathEscape(t *testing.T) {
	repo := t.TempDir()
	outside := t.TempDir()

	cases := []struct {
		name   string
		path   string
		wantOK bool
	}{
		{"empty", "", false},
		{"absolute", "/etc/passwd", false},
		{"dotdot", "../escape", false},
		{"dotdot nested", "sub/../../etc/passwd", false},
		{"normal", "internal/foo.go", true},
		{"root relative dot", ".", true},
	}

	// Create a symlink inside repo pointing outside.
	symlinkPath := filepath.Join(repo, "escape_link")
	if err := os.Symlink(outside, symlinkPath); err == nil {
		cases = append(cases, struct {
			name   string
			path   string
			wantOK bool
		}{"symlink outside", "escape_link/secret.txt", false})
	}

	for _, c := range cases {
		_, ok := ResolveUnderRepo(repo, c.path)
		if ok != c.wantOK {
			t.Errorf("ResolveUnderRepo(%q): got ok=%v, want %v", c.path, ok, c.wantOK)
		}
	}
}

// TestNumberedSnippet_Truncation verifies that line numbers stay accurate
// across the middle-truncation elision and that both ends are present.
func TestNumberedSnippet_Truncation(t *testing.T) {
	repo := initRepo(t)

	// Build a file with 200 lines. maxBytes must be > file_size/8 so the
	// read_ceiling doesn't cut off the tail, but < file_size so truncation fires.
	var lines []string
	for i := 1; i <= 200; i++ {
		lines = append(lines, fmt.Sprintf("line %d content here for padding to make it long enough", i))
	}
	content := strings.Join(lines, "\n") + "\n"
	writeFile(t, repo, "big.go", content)

	// ~11 000-byte file; maxBytes=2000 forces middle truncation while keeping
	// the read_ceiling (8*2000=16000) above the file size so all lines are read.
	snippet := NumberedSnippet(repo, "big.go", 2000, Tree{})

	if !strings.Contains(snippet, "lines omitted") {
		t.Error("expected truncation marker in snippet")
	}
	// Line numbers must appear as integers in the output.
	if !strings.Contains(snippet, "    1  ") {
		t.Error("expected line 1 in head")
	}
	// The last few lines of the file should appear in the tail (line 200).
	if !strings.Contains(snippet, "200") {
		t.Error("expected line 200 in tail")
	}
}

// TestNumberedSnippet_StagedVsWorktree verifies that staged=true reads the
// index, not the working tree.
func TestNumberedSnippet_StagedVsWorktree(t *testing.T) {
	repo := initRepo(t)

	writeFile(t, repo, "f.go", "package p\n// v1\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "initial")

	writeFile(t, repo, "f.go", "package p\n// v2\n")
	gitAdd(t, repo, ".")

	// Write v3 to worktree without staging.
	writeFile(t, repo, "f.go", "package p\n// v3\n")

	tree, err := PinIndexTree(repo)
	if err != nil {
		t.Fatalf("PinIndexTree: %v", err)
	}
	staged := NumberedSnippet(repo, "f.go", 120_000, tree)
	worktree := NumberedSnippet(repo, "f.go", 120_000, Tree{})

	if !strings.Contains(staged, "v2") {
		t.Error("staged snippet should show v2")
	}
	if strings.Contains(staged, "v3") {
		t.Error("staged snippet must not show v3 (unstaged)")
	}
	if !strings.Contains(worktree, "v3") {
		t.Error("worktree snippet should show v3")
	}
}

func TestNumberedSnippetCapturedWorktreeTruncationLabel(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "large.go", "package p\n"+strings.Repeat("var captured = 1\n", 2000))
	gitAdd(t, repo, "large.go")
	rawOID, err := exec.Command("git", "-C", repo, "write-tree").Output()
	if err != nil {
		t.Fatal(err)
	}
	tree := PinnedWorktree(strings.TrimSpace(string(rawOID)))
	snippet := NumberedSnippet(repo, "large.go", 100, tree)
	if !strings.Contains(snippet, "captured worktree blob exceeds") {
		t.Fatalf("captured truncation header does not identify its corpus:\n%s", snippet)
	}
	if strings.Contains(snippet, "staged blob") {
		t.Fatalf("captured worktree was mislabeled as staged:\n%s", snippet)
	}
}

// TestBuildFileContext_BudgetFitting verifies that files exceeding the budget
// are rendered as omission notices rather than content.
func TestBuildFileContext_BudgetFitting(t *testing.T) {
	repo := initRepo(t)

	bigContent := strings.Repeat("x", 2000) + "\n"
	writeFile(t, repo, "big.go", bigContent)
	writeFile(t, repo, "small.go", "package p\n")

	// Budget big enough for small.go but not big.go (the snippet overhead
	// plus content will exceed budget).
	out := BuildFileContext(repo, []string{"big.go", "small.go"}, 100, Tree{})

	if !strings.Contains(out, "context budget exceeded") {
		t.Error("expected budget-exceeded notice for big.go")
	}
	if !strings.Contains(out, "small.go") {
		t.Error("expected small.go to appear")
	}
}

// TestBuildFileContext_Empty verifies the empty-input sentinel.
func TestBuildFileContext_Empty(t *testing.T) {
	repo := t.TempDir()
	out := BuildFileContext(repo, nil, 100_000, Tree{})
	if out != "(no files)" {
		t.Errorf("expected '(no files)', got %q", out)
	}
}

// TestReadFileTool_Windowing verifies offset/limit windowing and offset-past-EOF.
func TestReadFileTool_Windowing(t *testing.T) {
	repo := initRepo(t)

	var lines []string
	for i := 1; i <= 20; i++ {
		lines = append(lines, fmt.Sprintf("line%d", i))
	}
	writeFile(t, repo, "f.go", strings.Join(lines, "\n")+"\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "init")

	// Window: lines 5–9.
	result := readFileTool(repo, map[string]any{
		"path":   "f.go",
		"offset": 5,
		"limit":  5,
	}, Tree{})
	if _, ok := result["error"]; ok {
		t.Fatalf("unexpected error: %v", result["error"])
	}
	content, _ := result["content"].(string)
	if !strings.Contains(content, "    5  line5") {
		t.Errorf("expected line5 in window, got: %s", content)
	}
	if strings.Contains(content, "line10") {
		t.Error("line10 should not be in window [5,9]")
	}
	if !strings.Contains(content, "more lines below") {
		t.Error("expected more-below message for window not at EOF")
	}

	// Past EOF.
	result2 := readFileTool(repo, map[string]any{
		"path":   "f.go",
		"offset": 999,
		"limit":  10,
	}, Tree{})
	content2, _ := result2["content"].(string)
	if !strings.Contains(content2, "past EOF") {
		t.Errorf("expected past-EOF message, got: %s", content2)
	}
}

// TestReadFileTool_StringIntCoercion verifies that string integers for offset/
// limit are accepted (models sometimes send them as strings).
func TestReadFileTool_StringIntCoercion(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "f.go", "a\nb\nc\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "init")

	result := readFileTool(repo, map[string]any{
		"path":   "f.go",
		"offset": "2",
		"limit":  "1",
	}, Tree{})
	if _, ok := result["error"]; ok {
		t.Fatalf("unexpected error: %v", result["error"])
	}
	content, _ := result["content"].(string)
	if !strings.Contains(content, "    2  b") {
		t.Errorf("expected line 2 content, got: %s", content)
	}
}

// TestListFilesTool_HiddenDirSkip verifies that hidden directories like .git
// are excluded from results.
func TestListFilesTool_HiddenDirSkip(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "visible.go", "package p\n")
	writeFile(t, repo, ".hidden/secret.go", "package p\n")

	result := listFilesTool(repo, map[string]any{"directory": ".", "pattern": "*.go"}, Tree{})
	matches, _ := result["matches"].([]string)

	for _, m := range matches {
		if strings.Contains(m, ".hidden") {
			t.Errorf("hidden dir appeared in list_files: %s", m)
		}
	}
	found := false
	for _, m := range matches {
		if m == "visible.go" {
			found = true
		}
	}
	if !found {
		t.Error("visible.go missing from list_files results")
	}
}

// TestListFilesTool_Cap verifies that list_files caps at 500 entries.
func TestListFilesTool_Cap(t *testing.T) {
	repo := initRepo(t)
	// Create 510 files.
	for i := 0; i < 510; i++ {
		writeFile(t, repo, fmt.Sprintf("f%04d.go", i), "package p\n")
	}

	result := listFilesTool(repo, map[string]any{"pattern": "*.go"}, Tree{})
	matches, _ := result["matches"].([]string)

	// Cap is 500, plus a truncation marker.
	if len(matches) > listFilesCap+1 {
		t.Errorf("list_files returned %d matches, expected <= %d+1", len(matches), listFilesCap)
	}
	last := matches[len(matches)-1]
	if !strings.Contains(last, "truncated") {
		t.Error("expected truncation marker as last entry")
	}
}

// TestGrepRepoTool_LiteralMatch verifies basic grep functionality.
func TestGrepRepoTool_LiteralMatch(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "a.go", "package p\n// needle here\nfunc f() {}\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "init")

	result := grepRepoTool(repo, map[string]any{"pattern": "needle"}, Tree{})
	if _, ok := result["error"]; ok {
		t.Fatalf("unexpected error: %v", result["error"])
	}
	matches, _ := result["matches"].([]string)
	if len(matches) == 0 {
		t.Fatal("expected at least one match for 'needle'")
	}
	if !strings.Contains(matches[0], "needle") {
		t.Errorf("match line doesn't contain pattern: %s", matches[0])
	}
}

// TestGrepRepoTool_InvalidRegexp verifies that an invalid regexp returns an
// error map rather than panicking.
func TestGrepRepoTool_InvalidRegexp(t *testing.T) {
	repo := initRepo(t)
	result := grepRepoTool(repo, map[string]any{"pattern": "["}, Tree{})
	if _, ok := result["error"]; !ok {
		t.Error("expected error for invalid regexp pattern")
	}
}

// TestGrepRepoTool_NoMatch verifies that no matches returns empty list, not error.
func TestGrepRepoTool_NoMatch(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "a.go", "package p\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "init")

	result := grepRepoTool(repo, map[string]any{"pattern": "XYZZY_NOT_FOUND_12345"}, Tree{})
	if _, ok := result["error"]; ok {
		t.Fatalf("no-match should not be an error: %v", result["error"])
	}
	matches, _ := result["matches"].([]string)
	if len(matches) != 0 {
		t.Errorf("expected empty matches, got: %v", matches)
	}
}

// TestGrepRepoTool_SeesUntrackedFilesUnderReview is the regression test for the
// verifier-blindness bug: `git grep` without --untracked searches only TRACKED
// files, but roast's own scope (internal/scope: git ls-files --others
// --exclude-standard) puts untracked files under review. A brand-new package in
// an --uncommitted-only review is therefore invisible to grep_repo while
// read_file and list_files can see it — so the reviewer greps for a caller,
// gets nothing, and reports "this function is never called" as grounding for a
// refutation or a severity downgrade.
//
// Both halves are asserted: the symbol's own definition AND its caller in a
// sibling untracked file, because the observed failure was a verifier
// concluding "no callers" for a symbol its own diff calls.
func TestGrepRepoTool_SeesUntrackedFilesUnderReview(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "tracked.go", "package p\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "init")

	// A new package, never `git add`ed — exactly what a worker's in-progress
	// diff looks like under --uncommitted-only.
	writeFile(t, repo, "newpkg/def.go", "package newpkg\n\nfunc runGrounder() bool { return true }\n")
	writeFile(t, repo, "newpkg/caller.go", "package newpkg\n\nfunc Prepare() bool { return runGrounder() }\n")

	result := grepRepoTool(repo, map[string]any{"pattern": "runGrounder"}, Tree{})
	if _, ok := result["error"]; ok {
		t.Fatalf("unexpected error: %v", result["error"])
	}
	matches, _ := result["matches"].([]string)
	joined := strings.Join(matches, "\n")
	for _, want := range []string{"newpkg/def.go", "newpkg/caller.go"} {
		if !strings.Contains(joined, want) {
			t.Errorf("grep_repo is blind to untracked file %s — a reviewer would read this as "+
				"'runGrounder has no callers'. matches:\n%s", want, joined)
		}
	}
}

// TestGrepRepoTool_StillHonorsGitignore pins the other side of --untracked:
// including untracked files must NOT start dragging ignored build output,
// node_modules, or vendored caches into the reviewer's context. git grep
// --untracked honors .gitignore unless --no-exclude-standard is passed; this
// test fails loudly if that ever changes under us.
func TestGrepRepoTool_StillHonorsGitignore(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, ".gitignore", "node_modules/\n*.log\n")
	writeFile(t, repo, "a.go", "package p\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "init")

	writeFile(t, repo, "node_modules/big.js", "ZZ_IGNORED_TOKEN\n")
	writeFile(t, repo, "debug.log", "ZZ_IGNORED_TOKEN\n")
	writeFile(t, repo, "real.go", "package p // ZZ_IGNORED_TOKEN\n")

	result := grepRepoTool(repo, map[string]any{"pattern": "ZZ_IGNORED_TOKEN"}, Tree{})
	matches, _ := result["matches"].([]string)
	joined := strings.Join(matches, "\n")
	if !strings.Contains(joined, "real.go") {
		t.Errorf("untracked non-ignored file not searched; matches:\n%s", joined)
	}
	for _, unwanted := range []string{"node_modules/big.js", "debug.log"} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("ignored path %s leaked into grep results:\n%s", unwanted, joined)
		}
	}
}

// TestGrepRepoTool_ZeroMatchesCarriesAbsenceCaveat: an empty grep result is
// where the false "no callers" inference is born, so the result itself must say
// that empty is not proof of absence. The caveat rides on the tool result, not
// only in a prompt, because it is the tool output the model reasons over turns
// later.
func TestGrepRepoTool_ZeroMatchesCarriesAbsenceCaveat(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "a.go", "package p\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "init")

	result := grepRepoTool(repo, map[string]any{"pattern": "XYZZY_NOT_FOUND_12345"}, Tree{})
	note, _ := result["note"].(string)
	if note == "" {
		t.Fatal("zero-match grep result carries no absence caveat")
	}
	for _, want := range []string{"not proof", "no callers", "insufficient_context"} {
		if !strings.Contains(strings.ToLower(note), want) {
			t.Errorf("absence caveat is missing %q: %s", want, note)
		}
	}
	if searched, _ := result["searched"].(string); searched == "" {
		t.Error("zero-match grep result does not say what corpus was searched")
	}

	// A result WITH matches has nothing to disclaim — the caveat exists to stop
	// an absence inference, and paying for it on every hit is pure token cost.
	hit := grepRepoTool(repo, map[string]any{"pattern": "package"}, Tree{})
	if _, ok := hit["note"]; ok {
		t.Error("non-empty grep result should not carry the absence caveat")
	}
}

// stagedDivergentRepo returns a repo where f.go has three distinct versions —
// committed, staged, and unstaged — plus a staged-only and an unstaged-only
// marker line, which is what makes "which tree did you read?" answerable from
// the content alone.
func stagedDivergentRepo(t *testing.T) string {
	t.Helper()
	repo := initRepo(t)
	writeFile(t, repo, "f.go", "package p\n// committed\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "initial")

	writeFile(t, repo, "f.go", "package p\nvar stagedOnlySymbol = 1\n")
	gitAdd(t, repo, "f.go")
	writeFile(t, repo, "f.go", "package p\nvar unstagedOnlySymbol = 2\n")
	return repo
}

// TestReadFileTool_StagedServesIndexNotWorktree is the core regression for the
// split-tree defect: read_file took a `staged` flag and ignored it, so a gate
// run showed the reviewer index content in its numbered snippet and worktree
// content from every exploration tool. Adjudicating a finding against a tree
// that is under review nowhere is exactly the class that cost one wrongly
// refuted finding and three bogus downgrades (aa28178).
func TestReadFileTool_StagedServesIndexNotWorktree(t *testing.T) {
	repo := stagedDivergentRepo(t)
	tree, err := PinIndexTree(repo)
	if err != nil {
		t.Fatalf("PinIndexTree: %v", err)
	}

	res := readFileTool(repo, map[string]any{"path": "f.go"}, tree)
	if _, ok := res["error"]; ok {
		t.Fatalf("unexpected error: %v", res["error"])
	}
	content, _ := res["content"].(string)
	if !strings.Contains(content, "stagedOnlySymbol") {
		t.Errorf("staged read_file did not serve the index blob:\n%s", content)
	}
	if strings.Contains(content, "unstagedOnlySymbol") {
		t.Errorf("staged read_file leaked unstaged worktree content:\n%s", content)
	}

	// The other half of "one review sees one tree": non-staged scope is
	// unchanged and still reads the worktree.
	wt, _ := readFileTool(repo, map[string]any{"path": "f.go"}, Tree{})["content"].(string)
	if !strings.Contains(wt, "unstagedOnlySymbol") {
		t.Errorf("non-staged read_file should read the worktree:\n%s", wt)
	}
}

// TestReadFileTool_StagedWindowingAndPathologicalLines pins that serving the
// index did not cost read_file its contract: the windowing, more-below,
// past-EOF and long-line handling are the same code on both paths.
func TestReadFileTool_StagedWindowingAndPathologicalLines(t *testing.T) {
	repo := initRepo(t)
	var lines []string
	for i := 1; i <= 20; i++ {
		lines = append(lines, fmt.Sprintf("line%d", i))
	}
	writeFile(t, repo, "f.go", strings.Join(lines, "\n")+"\n")
	// Unterminated: ReadString consumes a newline-terminated line whole however
	// long it is, so the truncation branch is reachable only at an EOF with no
	// trailing newline. Same shape on both corpora — git preserves a missing
	// trailing newline in the blob.
	writeFile(t, repo, "long.go", strings.Repeat("x", maxLineBytes+10))
	gitAdd(t, repo, ".")
	// Deliberately NOT committed: the index is the whole corpus in staged scope.
	tree, err := PinIndexTree(repo)
	if err != nil {
		t.Fatalf("PinIndexTree: %v", err)
	}

	res := readFileTool(repo, map[string]any{"path": "f.go", "offset": 5, "limit": 5}, tree)
	if _, ok := res["error"]; ok {
		t.Fatalf("unexpected error: %v", res["error"])
	}
	content, _ := res["content"].(string)
	if !strings.Contains(content, "    5  line5") {
		t.Errorf("staged window lost its line numbering:\n%s", content)
	}
	if strings.Contains(content, "line10") {
		t.Errorf("staged window [5,9] leaked line10:\n%s", content)
	}
	if !strings.Contains(content, "more lines below") {
		t.Errorf("staged window not at EOF lacks the more-below hint:\n%s", content)
	}
	if more, _ := res["more_below"].(bool); !more {
		t.Error("staged more_below should be true for a window before EOF")
	}
	if got := fmt.Sprint(res["returned_range"]); got != "[5 9]" {
		t.Errorf("staged returned_range = %s, want [5 9]", got)
	}

	past, _ := readFileTool(repo, map[string]any{"path": "f.go", "offset": 999}, tree)["content"].(string)
	if !strings.Contains(past, "past EOF") {
		t.Errorf("staged past-EOF message missing:\n%s", past)
	}

	// Parity is the assertion, not a new behavior: the long-line handling is one
	// shared code path, so the index and the worktree must report identically.
	// Reported by length + tail, never verbatim — on failure the body is the
	// whole megabyte-long line, and a test that floods the log is unreadable.
	shape := func(s string) string {
		return fmt.Sprintf("%d bytes, tail %q", len(s), s[max(0, len(s)-80):])
	}
	stagedLong, _ := readFileTool(repo, map[string]any{"path": "long.go"}, tree)["content"].(string)
	wtLong, _ := readFileTool(repo, map[string]any{"path": "long.go"}, Tree{})["content"].(string)
	if !strings.Contains(stagedLong, "line truncated at") {
		t.Errorf("staged pathological long line not truncated: %s", shape(stagedLong))
	}
	if stagedLong != wtLong {
		t.Errorf("long-line handling diverges between trees:\n staged: %s\n worktree: %s",
			shape(stagedLong), shape(wtLong))
	}
}

// TestReadFileTool_StagedDeletionIsNamedNotJustAbsent: a path staged for
// deletion is absent from the index but still present in the worktree until the
// commit lands. "does not exist" would be a lie the reviewer could act on; the
// result has to say the path is not in the commit's content.
func TestReadFileTool_StagedDeletionIsNamedNotJustAbsent(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "doomed.go", "package p\nvar Doomed = 1\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "initial")
	if out, err := exec.Command("git", "-C", repo, "rm", "--cached", "-q", "doomed.go").CombinedOutput(); err != nil {
		t.Fatalf("git rm --cached: %v\n%s", err, out)
	}

	// Still on disk, so a worktree read would happily serve it.
	if _, err := os.Stat(filepath.Join(repo, "doomed.go")); err != nil {
		t.Fatalf("precondition: file should still be in the worktree: %v", err)
	}

	tree, err := PinIndexTree(repo)
	if err != nil {
		t.Fatalf("PinIndexTree: %v", err)
	}
	res := readFileTool(repo, map[string]any{"path": "doomed.go"}, tree)
	msg, _ := res["error"].(string)
	if msg == "" {
		t.Fatalf("staged read of a deleted path returned content, not an error: %v", res)
	}
	if !strings.Contains(msg, "index") {
		t.Errorf("staged-deletion error does not say the path is absent from the index: %q", msg)
	}
	if strings.Contains(msg, "does not exist") {
		t.Errorf("staged-deletion error reads as a missing file: %q", msg)
	}
}

// TestStagedContentRejectsGitRevisionMagic (roast finding R-a97d94): every
// staged reader builds `:`+path and hands it to git, which parses that in its
// REVISION grammar, not as a pathspec. `:0:f.go` is f.go's ordinary stage-0
// blob under a fabricated key, and `:/text` is a commit-message search — so an
// untrusted path can return bytes that are not the named path's. That matters
// most in the grounder, whose per-file corpora exist precisely so real content
// cannot ground a finding anchored to a fabricated path.
func TestStagedContentRejectsGitRevisionMagic(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "foo.go", "package p\nvar Real = 1\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "initial")

	// Establish the hazard is real and not a misreading of gitrevisions: git
	// resolves the decorated spelling to foo.go's blob.
	raw, err := exec.Command("git", "-C", repo, "show", ":0:foo.go").Output()
	if err != nil || !strings.Contains(string(raw), "var Real = 1") {
		t.Skipf("this git does not resolve :0:<path> to the stage-0 blob (%v); guard is still correct", err)
	}

	tree, perr := PinIndexTree(repo)
	if perr != nil {
		t.Fatalf("PinIndexTree: %v", perr)
	}

	// Leading ./ and ../ are the other alias family (roast finding R-d28385):
	// gitrevisions says a <rev>:<path> beginning ./ or ../ is resolved relative
	// to the cwd and rewritten worktree-root-relative, so `:./foo.go` is another
	// spelling of `:foo.go`. Every non-canonical segment is rejected rather than
	// just the documented leading forms — a canonical index path never contains
	// an empty, "." or ".." segment, so fail-closed costs nothing and does not
	// depend on which spellings this git version normalizes.
	for _, bad := range []string{
		"0:foo.go", "1:foo.go", "/foo.go", ":foo.go", "",
		"./foo.go", "../foo.go", "sub/../foo.go", "sub//foo.go", "foo.go/.",
	} {
		if c, ok := StagedContent(repo, tree, bad); ok {
			t.Errorf("StagedContent(%q) returned content under a path that is not that path: %q", bad, c)
		}
	}
	if _, ok := StagedContent(repo, tree, "foo.go"); !ok {
		t.Error("the guard rejected a legitimate index path")
	}

	// The consequence the guard exists to stop: real content grounding a
	// finding cited to a fabricated path.
	g := NewQuoteGrounder(repo, tree, "", "")
	for _, bad := range []string{"0:foo.go", "./foo.go"} {
		if g(bad, "var Real = 1") {
			t.Errorf("a finding cited to %q grounds against the real file's content — the "+
				"per-file keying the corpora depend on is defeated", bad)
		}
	}
	if !g("foo.go", "var Real = 1") {
		t.Error("the guard broke grounding for the honest path")
	}

	// read_file must not report one path while serving another's bytes.
	res := readFileTool(repo, map[string]any{"path": "0:foo.go"}, tree)
	if _, isErr := res["error"]; !isErr {
		t.Errorf("read_file served a stage-decorated path: %v", res)
	}
}

// TestGrepRepoTool_StagedSearchesTheIndex: grep_repo is the other half of the
// split. In staged scope it must match staged content and must NOT match an
// unstaged edit or an untracked file — neither is in the commit under review.
func TestGrepRepoTool_StagedSearchesTheIndex(t *testing.T) {
	repo := stagedDivergentRepo(t)
	writeFile(t, repo, "untracked.go", "package p // untrackedOnlySymbol\n")

	tree, err := PinIndexTree(repo)
	if err != nil {
		t.Fatalf("PinIndexTree: %v", err)
	}
	res := grepRepoTool(repo, map[string]any{"pattern": "OnlySymbol"}, tree)
	if _, ok := res["error"]; ok {
		t.Fatalf("unexpected error: %v", res["error"])
	}
	matches, _ := res["matches"].([]string)
	joined := strings.Join(matches, "\n")
	if !strings.Contains(joined, "stagedOnlySymbol") {
		t.Errorf("staged grep_repo missed staged content:\n%s", joined)
	}
	if strings.Contains(joined, "unstagedOnlySymbol") {
		t.Errorf("staged grep_repo leaked an unstaged edit:\n%s", joined)
	}
	if strings.Contains(joined, "untrackedOnlySymbol") {
		t.Errorf("staged grep_repo leaked an untracked file:\n%s", joined)
	}

	// A file staged as new IS in the index — --cached alone must find it, which
	// is why --cached and --untracked are alternatives and not companions.
	writeFile(t, repo, "added.go", "package p // freshlyStagedSymbol\n")
	gitAdd(t, repo, "added.go")
	tree2, err := PinIndexTree(repo)
	if err != nil {
		t.Fatalf("PinIndexTree: %v", err)
	}
	fresh := grepRepoTool(repo, map[string]any{"pattern": "freshlyStagedSymbol"}, tree2)
	freshMatches, _ := fresh["matches"].([]string)
	if !strings.Contains(strings.Join(freshMatches, "\n"), "added.go") {
		t.Errorf("staged grep_repo is blind to a newly staged file: %v", fresh)
	}
}

// TestGrepRepoTool_StagedMatchesAreNotOIDPrefixed: `git grep <tree>` prefixes
// every match line with the tree-ish it searched — measured:
//
//	git grep -n -e Foo --cached  → a.go:2:func Foo(){}
//	git grep -n -e Foo $T --     → e72cf53…:a.go:2:func Foo(){}
//
// These lines are handed to the model, which reads them as file:line:text and
// cites them. An un-stripped prefix teaches it a path that does not exist, and
// a follow-up read_file on "<OID>:a.go" lands in stagedPathIsMagic's
// stage-selector rejection — so the reviewer would be unable to open the file
// its own search just found. The two corpora must present matches in one shape.
//
// A Contains-based assertion cannot catch this (the substring is still there),
// which is why this checks the line's SHAPE.
func TestGrepRepoTool_StagedMatchesAreNotOIDPrefixed(t *testing.T) {
	repo := stagedDivergentRepo(t)
	tree, err := PinIndexTree(repo)
	if err != nil {
		t.Fatalf("PinIndexTree: %v", err)
	}
	if tree.OID == "" {
		t.Fatal("test premise: the tree must be pinned")
	}

	res := grepRepoTool(repo, map[string]any{"pattern": "OnlySymbol"}, tree)
	matches, _ := res["matches"].([]string)
	if len(matches) == 0 {
		t.Fatalf("no matches to inspect: %v", res)
	}
	for _, m := range matches {
		if strings.HasPrefix(m, tree.OID) {
			t.Errorf("match line carries the tree OID, which is not part of any path:\n%s", m)
		}
		// Positively: the line must start with a path that resolves in the
		// pinned tree, so the model can act on what it just read.
		path, _, found := strings.Cut(m, ":")
		if !found {
			t.Errorf("match line has no file:line shape: %q", m)
			continue
		}
		if !StagedPathCanonical(path) {
			t.Errorf("grep handed the model a path its own read_file guard rejects: %q (from %q)", path, m)
		}
		if _, ok := StagedContent(repo, tree, path); !ok {
			t.Errorf("grep cited %q, which does not resolve in the pinned tree (from %q)", path, m)
		}
	}
}

// TestGrepRepoTool_ZeroMatchesNamesWhichTreeWasSearched: with two possible
// corpora, "what could this search have seen" is the thing a reader most needs
// from an empty result.
func TestGrepRepoTool_ZeroMatchesNamesWhichTreeWasSearched(t *testing.T) {
	repo := stagedDivergentRepo(t)

	tree, err := PinIndexTree(repo)
	if err != nil {
		t.Fatalf("PinIndexTree: %v", err)
	}
	stagedRes := grepRepoTool(repo, map[string]any{"pattern": "XYZZY_NOT_FOUND_12345"}, tree)
	stagedSearched, _ := stagedRes["searched"].(string)
	if !strings.Contains(strings.ToLower(stagedSearched), "index") {
		t.Errorf("staged zero-match result does not name the index: %q", stagedSearched)
	}
	stagedNote, _ := stagedRes["note"].(string)
	if !strings.Contains(strings.ToLower(stagedNote), "unstaged") {
		t.Errorf("staged absence caveat does not mention unstaged edits: %q", stagedNote)
	}

	wtRes := grepRepoTool(repo, map[string]any{"pattern": "XYZZY_NOT_FOUND_12345"}, Tree{})
	wtSearched, _ := wtRes["searched"].(string)
	if !strings.Contains(strings.ToLower(wtSearched), "working tree") {
		t.Errorf("worktree zero-match result does not name the working tree: %q", wtSearched)
	}
	if strings.Contains(strings.ToLower(wtSearched), "index") {
		t.Errorf("worktree zero-match result claims to have searched the index: %q", wtSearched)
	}
}

// TestExplorationToolDescriptionsDiscloseTheirTree: honoring `staged` is half
// the fix; a model told "read a file from the repository" reasonably assumes
// the working tree, and in staged scope that is not what it is being shown.
// Corpus disclosure is a standing invariant of every search tool here.
func TestExplorationToolDescriptionsDiscloseTheirTree(t *testing.T) {
	for _, tc := range []struct {
		tree Tree
		want string
	}{
		{Tree{OID: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"}, "index"},
		{PinnedWorktree("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"), "captured worktree"},
		{Tree{}, "working tree"},
	} {
		for _, tool := range ExplorationTools(t.TempDir(), tc.tree) {
			if tool.Name != "read_file" && tool.Name != "grep_repo" && tool.Name != "list_files" {
				continue
			}
			if !strings.Contains(strings.ToLower(tool.Description), tc.want) {
				t.Errorf("staged=%v: %s description does not disclose %q: %s",
					tc.tree.Staged(), tool.Name, tc.want, tool.Description)
			}
		}
	}
}

func TestCapturedWorktreeReadersIgnoreLaterCheckoutEdits(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "a.go", "package p\nvar committedSymbol = 1\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "initial")

	writeFile(t, repo, "a.go", "package p\nvar capturedSymbol = 1\n")
	writeFile(t, repo, "new.go", "package p\nvar capturedUntrackedSymbol = 1\n")
	writeFile(t, repo, "sub/inside.go", "package sub\nvar capturedNestedSymbol = 1\n")
	gitAdd(t, repo, ".")
	rawOID, err := exec.Command("git", "-C", repo, "write-tree").Output()
	if err != nil {
		t.Fatal(err)
	}
	tree := PinnedWorktree(strings.TrimSpace(string(rawOID)))

	writeFile(t, repo, "a.go", "package p\nvar driftedSymbol = 1\n")
	writeFile(t, repo, "new.go", "package p\nvar driftedUntrackedSymbol = 1\n")
	writeFile(t, repo, "later.go", "package p\nvar laterSymbol = 1\n")
	symlinkedSub := false
	if err := os.RemoveAll(filepath.Join(repo, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(repo, "sub")); err == nil {
		symlinkedSub = true
	}

	check := func(source, text string) {
		t.Helper()
		if !strings.Contains(text, "capturedSymbol") {
			t.Errorf("%s did not read captured content:\n%s", source, text)
		}
		if strings.Contains(text, "driftedSymbol") {
			t.Errorf("%s read a later checkout edit:\n%s", source, text)
		}
	}
	check("NumberedSnippet", NumberedSnippet(repo, "a.go", 120_000, tree))
	read, _ := readFileTool(repo, map[string]any{"path": "a.go"}, tree)["content"].(string)
	check("read_file", read)
	matches, _ := grepRepoTool(repo, map[string]any{"pattern": "Symbol"}, tree)["matches"].([]string)
	joined := strings.Join(matches, "\n")
	check("grep_repo", joined)
	if !strings.Contains(joined, "capturedUntrackedSymbol") || strings.Contains(joined, "driftedUntrackedSymbol") || strings.Contains(joined, "laterSymbol") {
		t.Errorf("grep_repo did not stay on the captured worktree:\n%s", joined)
	}
	listed, _ := listFilesTool(repo, map[string]any{"pattern": "*.go"}, tree)["matches"].([]string)
	listedText := strings.Join(listed, "\n")
	if !strings.Contains(listedText, "a.go") || !strings.Contains(listedText, "new.go") || strings.Contains(listedText, "later.go") {
		t.Errorf("list_files did not stay on the captured worktree: %v", listed)
	}
	if symlinkedSub {
		nested, _ := readFileTool(repo, map[string]any{"path": "sub/inside.go"}, tree)["content"].(string)
		if !strings.Contains(nested, "capturedNestedSymbol") {
			t.Errorf("read_file consulted the later live symlink instead of the captured tree: %s", nested)
		}
		nestedFiles, _ := listFilesTool(repo, map[string]any{"directory": "sub", "pattern": "*.go"}, tree)["matches"].([]string)
		if !strings.Contains(strings.Join(nestedFiles, "\n"), "sub/inside.go") {
			t.Errorf("list_files consulted the later live symlink instead of the captured tree: %v", nestedFiles)
		}
	}
	g := NewQuoteGrounder(repo, tree, "", "")
	if !g("a.go", "var capturedSymbol = 1") || g("a.go", "var driftedSymbol = 1") {
		t.Error("grounder corpus does not match the captured worktree")
	}
}

// TestStagedContextSourcesAgreeUnderGitIndexFile is the gate path end to end at
// the context layer. Git runs a pre-commit hook with GIT_INDEX_FILE pointing at
// a temporary index holding exactly the commit's content, so for a path-scoped
// `git commit b.go` in a dirty tree there are THREE candidate trees: that
// index, the repo's live index, and the worktree. Every context source must
// land on the first one, and none of them may clear or override the env that
// says which it is.
func TestStagedContextSourcesAgreeUnderGitIndexFile(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "a.go", "package p\n// a-committed\n")
	writeFile(t, repo, "b.go", "package p\n// b-committed\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "initial")

	// The dirty shared state the gate must NOT review: a.go staged in the live
	// index, b.go edited further in the worktree after being staged.
	writeFile(t, repo, "a.go", "package p\nvar liveIndexSymbol = 1\n")
	gitAdd(t, repo, "a.go")

	// What git hands the hook for `git commit b.go`.
	alt := filepath.Join(t.TempDir(), "next-index")
	t.Setenv("GIT_INDEX_FILE", alt)
	if out, err := exec.Command("git", "-C", repo, "read-tree", "HEAD").CombinedOutput(); err != nil {
		t.Fatalf("read-tree: %v\n%s", err, out)
	}
	writeFile(t, repo, "b.go", "package p\nvar commitIndexSymbol = 1\n")
	gitAdd(t, repo, "b.go")
	writeFile(t, repo, "b.go", "package p\nvar worktreeSymbol = 1\n")

	tree, err := PinIndexTree(repo)
	if err != nil {
		t.Fatalf("PinIndexTree: %v", err)
	}

	present := "commitIndexSymbol"
	absent := []string{"worktreeSymbol", "liveIndexSymbol"}

	check := func(source, text string) {
		t.Helper()
		if !strings.Contains(text, present) {
			t.Errorf("%s did not see the commit's index content:\n%s", source, text)
		}
		for _, a := range absent {
			if strings.Contains(text, a) {
				t.Errorf("%s saw %s — a tree that is not under review:\n%s", source, a, text)
			}
		}
	}

	check("NumberedSnippet", NumberedSnippet(repo, "b.go", 120_000, tree))
	rf, _ := readFileTool(repo, map[string]any{"path": "b.go"}, tree)["content"].(string)
	check("read_file", rf)
	gr, _ := grepRepoTool(repo, map[string]any{"pattern": "Symbol"}, tree)["matches"].([]string)
	check("grep_repo", strings.Join(gr, "\n"))

	// And the grounder's corpus is exactly that set: no wider (a quote of the
	// worktree edit must not ground) and no narrower.
	g := NewQuoteGrounder(repo, tree, "", "")
	if !g("b.go", "var commitIndexSymbol = 1") {
		t.Error("grounder rejects a quote of the content actually under review")
	}
	if g("b.go", "var worktreeSymbol = 1") {
		t.Error("grounder admits a worktree quote no context source could have served")
	}
	if g("a.go", "var liveIndexSymbol = 1") {
		t.Error("grounder admits content from the live index, not this commit's")
	}
}

// TestStagedReadersSeeOneTreeUnderConcurrentIndexWrites is the load-bearing
// regression this refactor exists for:
// a `staged bool` re-resolves the live index on every call, so a concurrent
// `git add` between two reads of the same review — another session, a
// format-on-save, a `just` recipe staging generated output — shows up as
// drift: two readers, or two calls to the same reader, disagreeing about what
// "staged" means. Pinning an OID up front and threading it through closes
// that race. This is the test that fails against unfixed code: every reader
// below is called strictly AFTER the tree is pinned and AFTER the index moves
// again, so an unfixed `staged bool` reader observes the post-pin write while
// a fixed `Tree` reader does not.
func TestStagedReadersSeeOneTreeUnderConcurrentIndexWrites(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "f.go", "package p\n// committed\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "initial")

	writeFile(t, repo, "f.go", "package p\nvar pinnedSymbol = 1\n")
	gitAdd(t, repo, "f.go")

	tree, err := PinIndexTree(repo)
	if err != nil {
		t.Fatalf("PinIndexTree: %v", err)
	}

	// The index moves AFTER the pin: a second file is staged new, and the
	// already-staged file is edited and re-staged with different content.
	// Every reader below runs after both of these writes.
	writeFile(t, repo, "new.go", "package p\nvar concurrentlyAddedSymbol = 1\n")
	gitAdd(t, repo, "new.go")
	writeFile(t, repo, "f.go", "package p\nvar driftedSymbol = 1\n")
	gitAdd(t, repo, "f.go")

	snippet := NumberedSnippet(repo, "f.go", 120_000, tree)
	if !strings.Contains(snippet, "pinnedSymbol") {
		t.Errorf("NumberedSnippet drifted off the pinned tree:\n%s", snippet)
	}
	if strings.Contains(snippet, "driftedSymbol") {
		t.Errorf("NumberedSnippet picked up the post-pin index write:\n%s", snippet)
	}

	rf, _ := readFileTool(repo, map[string]any{"path": "f.go"}, tree)["content"].(string)
	if !strings.Contains(rf, "pinnedSymbol") {
		t.Errorf("read_file drifted off the pinned tree:\n%s", rf)
	}
	if strings.Contains(rf, "driftedSymbol") {
		t.Errorf("read_file picked up the post-pin index write:\n%s", rf)
	}

	gr, _ := grepRepoTool(repo, map[string]any{"pattern": "Symbol"}, tree)["matches"].([]string)
	joined := strings.Join(gr, "\n")
	if !strings.Contains(joined, "pinnedSymbol") {
		t.Errorf("grep_repo drifted off the pinned tree:\n%s", joined)
	}
	if strings.Contains(joined, "driftedSymbol") {
		t.Errorf("grep_repo picked up the post-pin index write to f.go:\n%s", joined)
	}
	if strings.Contains(joined, "concurrentlyAddedSymbol") || strings.Contains(joined, "new.go") {
		t.Errorf("grep_repo saw a file staged after the tree was pinned:\n%s", joined)
	}

	g := NewQuoteGrounder(repo, tree, "", "")
	if !g("f.go", "var pinnedSymbol = 1") {
		t.Error("grounder rejects the pinned content")
	}
	if g("f.go", "var driftedSymbol = 1") {
		t.Error("grounder admits content staged after the pin")
	}
}

// TestReadFileToolDoesNotBufferWholeBlob (spec test #3) pins that the staged
// read path streams `git cat-file blob` and kills the process once its window
// is filled, rather than draining it to EOF. A stub `git` on PATH stands in
// for `cat-file blob` only — every other invocation (the existence check, and
// anything PinIndexTree itself runs before the stub goes on PATH) delegates to
// the real binary. The stub emits lines with a real sleep between each: if
// readFileTool ever drained to EOF, this test would have to wait out every
// remaining sleep; since it kills the process once the requested window (plus
// one line, to confirm there is more) has been read, elapsed time is bounded
// by the WINDOW, not by how many lines the stub still has queued. The gap
// between "window filled" (~3 sleeps) and "fully drained" (~30 sleeps) is wide
// enough that this is not a timing coin-flip.
// TestReadFileToolDoesNotBufferAWholeSingleLineBlob: the OTHER half of "does
// not buffer". Windowing bounds how many LINES are held, and killing the
// cat-file process bounds how much of the blob is read — but neither bounds one
// LINE. bufio.Reader.ReadString accumulates until it finds the delimiter, so a
// minified bundle or a generated single-line file allocated the whole blob
// before maxLineBytes was ever consulted (roast R-a5c87d). readLineCapped stops
// AT the cap via ReadSlice, whose cost is the reader's buffer regardless of line
// length.
//
// Asserted by SCALING, not by an absolute figure, because "bounded by the cap
// rather than by the blob" is a statement about the slope. Measured on this
// code:
//
//	              4MB blob   16MB blob   64MB blob
//	unbounded      24.1MB      96.2MB     384.2MB   (~6x blob, linear)
//	capped          8.8MB       8.8MB       8.8MB   (flat)
//
// An absolute threshold cannot express that — pick one above 8.8MB and a small
// blob passes while unbounded; pick one below and the fixed code fails. A file
// of many short lines cannot catch it either: the sibling test emits exactly
// that and passes against the unbounded code.
func TestReadFileToolDoesNotBufferAWholeSingleLineBlob(t *testing.T) {
	// One line, no newline: the shape a minified JS bundle or a generated
	// fixture actually has. Two sizes 4x apart — if the line is accumulated to
	// its delimiter, the larger costs ~4x the smaller; if it is capped, both
	// cost the same.
	const small = 4 * maxLineBytes
	const large = 16 * maxLineBytes

	measure := func(t *testing.T, size int, tree func(string) Tree) uint64 {
		t.Helper()
		repo := initRepo(t)
		writeFile(t, repo, "min.js", strings.Repeat("x", size))
		gitAdd(t, repo, ".")
		var before, after runtime.MemStats
		tr := tree(repo)
		runtime.GC()
		runtime.ReadMemStats(&before)
		res := readFileTool(repo, map[string]any{"path": "min.js"}, tr)
		runtime.ReadMemStats(&after)
		if content, _ := res["content"].(string); !strings.Contains(content, "line truncated at") {
			t.Fatalf("the capped line was not reported as truncated (%d bytes returned)", len(content))
		}
		return after.TotalAlloc - before.TotalAlloc
	}

	for _, tc := range []struct {
		name string
		tree func(string) Tree
	}{
		{"staged", func(repo string) Tree {
			tr, err := PinIndexTree(repo)
			if err != nil {
				t.Fatalf("PinIndexTree: %v", err)
			}
			return tr
		}},
		{"worktree", func(string) Tree { return Tree{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotSmall := measure(t, small, tc.tree)
			gotLarge := measure(t, large, tc.tree)
			// 4x the input for 2x the allocation would already be sublinear;
			// requiring under 2x leaves generous room for GC noise while still
			// failing the ~4x an accumulating read produces.
			if gotLarge > 2*gotSmall {
				t.Errorf("allocation tracks blob size, not the line cap: %d bytes for a %d-byte blob vs %d bytes "+
					"for a %d-byte blob (%.1fx for 4x the input) — the line is still being accumulated to its "+
					"delimiter", gotLarge, large, gotSmall, small, float64(gotLarge)/float64(gotSmall))
			}
		})
	}
}

func TestReadFileToolDoesNotBufferWholeBlob(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}

	repo := initRepo(t)
	writeFile(t, repo, "big.go", "line\n")
	gitAdd(t, repo, ".")
	tree, err := PinIndexTree(repo)
	if err != nil {
		t.Fatalf("PinIndexTree: %v", err)
	}

	stubDir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "-C" ] && [ "$3" = "cat-file" ] && [ "$4" = "blob" ]; then
	i=1
	while [ "$i" -le 30 ]; do
		echo "line$i"
		sleep 1
		i=$((i + 1))
	done
	exit 0
fi
exec "%s" "$@"
`, realGit)
	if err := os.WriteFile(filepath.Join(stubDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	start := time.Now()
	res := readFileTool(repo, map[string]any{"path": "big.go", "offset": 1, "limit": 3}, tree)
	elapsed := time.Since(start)

	if _, ok := res["error"]; ok {
		t.Fatalf("unexpected error: %v", res["error"])
	}
	content, _ := res["content"].(string)
	if !strings.Contains(content, "line1") || !strings.Contains(content, "line3") {
		t.Errorf("window did not contain the expected lines:\n%s", content)
	}
	if strings.Contains(content, "line4") {
		t.Errorf("window leaked past its limit:\n%s", content)
	}
	// Draining all 30 stubbed lines would take ~30s; killing the process once
	// the 3-line window (plus the one-line lookahead) fills should land in
	// single-digit seconds.
	if elapsed > 10*time.Second {
		t.Errorf("readFileTool took %s — looks like it drained the stub to EOF instead of killing it once its window filled", elapsed)
	}
}

func TestListFilesGlobstarMatchesEveryDepth(t *testing.T) {
	// A repository, because the live corpus is Git's view of the working tree.
	repo := initRepo(t)
	for _, name := range []string{"root_test.go", "one/one_test.go", "one/two/two_test.go", "one/two/code.go"} {
		full := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("package test\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	result := listFilesTool(repo, map[string]any{"pattern": "**/*_test.go"}, Tree{})
	matches, ok := result["matches"].([]string)
	if !ok {
		t.Fatalf("matches = %#v", result["matches"])
	}
	for _, want := range []string{"root_test.go", "one/one_test.go", "one/two/two_test.go"} {
		if !slices.Contains(matches, want) {
			t.Errorf("matches %v do not contain %q", matches, want)
		}
	}
	if slices.Contains(matches, "one/two/code.go") {
		t.Errorf("matches unexpectedly contain non-test file: %v", matches)
	}
}

// liveFixture is a repository whose live working tree holds everything a
// pinned tree leaves out: Git's own directory, an ignored secret, an
// untracked denylisted credential, and a symlink into .git.
func liveFixture(t *testing.T) string {
	t.Helper()
	repo := initRepo(t)
	writeFile(t, repo, ".gitignore", ".env\nbuild/\n")
	writeFile(t, repo, "a.go", "package a\n\nfunc A() {}\n")
	writeFile(t, repo, "id_test.go", "package a\n\nfunc TrackedDenylistShape() {}\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "initial")
	if out, err := runGit(repo, "config", "remote.origin.url", "https://user:FAKETOKEN@example.com/x/y"); err != nil {
		t.Fatalf("git config: %v: %s", err, out)
	}
	writeFile(t, repo, ".env", "API_TOKEN=GITIGNORED_SECRET\n")
	writeFile(t, repo, "build/out.go", "package build // GITIGNORED_SECRET\n")
	writeFile(t, repo, "credentials.json", `{"token":"DENYLISTED_SECRET"}`+"\n")
	writeFile(t, repo, "new.go", "package a\n\nfunc New() {}\n")
	if err := os.Symlink(".git", filepath.Join(repo, "gitlink")); err != nil {
		t.Fatal(err)
	}
	return repo
}

func runGit(repo string, args ...string) (string, error) {
	out, err := literalGit(repo, args...).CombinedOutput()
	return string(out), err
}

func asJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// TestLiveTreeServesOnlyTheReviewCorpus: when no tree could be pinned, the
// live working tree is served under the rules a pinned tree enforces by
// construction — never .git (in any case spelling or through a symlink),
// never an ignored file, never an untracked denylisted credential.
func TestLiveTreeServesOnlyTheReviewCorpus(t *testing.T) {
	repo := liveFixture(t)
	live := Tree{}
	secrets := []string{"FAKETOKEN", "GITIGNORED_SECRET", "DENYLISTED_SECRET", "repositoryformatversion"}
	leaks := func(what string, v any) {
		t.Helper()
		s := asJSON(v)
		for _, secret := range secrets {
			if strings.Contains(s, secret) {
				t.Errorf("%s leaked %s: %s", what, secret, s)
			}
		}
	}

	for _, p := range []string{".git/config", ".GIT/config", ".Git/HEAD", "gitlink/config", ".env", "build/out.go", "credentials.json", "./.git/config", "a/../.git/config"} {
		res := readFileTool(repo, map[string]any{"path": p}, live)
		if _, ok := res["error"]; !ok {
			t.Errorf("read_file(%q) served content: %s", p, asJSON(res))
		}
		leaks("read_file "+p, res)
		leaks("NumberedSnippet "+p, NumberedSnippet(repo, p, 10_000, live))
	}
	for _, p := range []string{"a.go", "new.go", "id_test.go"} {
		if res := readFileTool(repo, map[string]any{"path": p}, live); res["error"] != nil {
			t.Errorf("read_file(%q) should serve a reviewed file: %v", p, res["error"])
		}
	}

	ground := NewQuoteGrounder(repo, live, "", "")
	if ground(".git/config", "url = https://user:FAKETOKEN@example.com/x/y") || ground(".env", "API_TOKEN=GITIGNORED_SECRET") || ground("credentials.json", `"token":"DENYLISTED_SECRET"`) {
		t.Error("grounder accepted a quote from outside the review corpus")
	}
	if !ground("new.go", "func New() {}") {
		t.Error("grounder must still ground untracked, non-ignored files")
	}

	grep := grepRepoTool(repo, map[string]any{"pattern": "SECRET|FAKETOKEN|func"}, live)
	leaks("grep_repo", grep["matches"])
	if !strings.Contains(asJSON(grep), "func New") {
		t.Errorf("grep_repo lost the reviewed untracked file: %s", asJSON(grep))
	}

	list := listFilesTool(repo, map[string]any{"pattern": "**"}, live)
	got := asJSON(list)
	for _, hidden := range []string{"credentials.json", "build/out.go", "gitlink", ".env"} {
		if strings.Contains(got, hidden) {
			t.Errorf("list_files listed %s: %s", hidden, got)
		}
	}
	for _, want := range []string{"a.go", "new.go", "id_test.go"} {
		if !strings.Contains(got, `"`+want+`"`) {
			t.Errorf("list_files lost %s: %s", want, got)
		}
	}
	for _, dir := range []string{".git", "build", "gitlink"} {
		if res := listFilesTool(repo, map[string]any{"directory": dir}, live); res["error"] == nil {
			t.Errorf("list_files(directory=%q) should be refused: %s", dir, asJSON(res))
		}
	}
}

// TestPinnedTreeLeavesOutWhatTheLiveGuardBlocks keeps the two corpora honest
// with each other: what the live guard refuses is exactly what pinning leaves
// out.
func TestPinnedTreeLeavesOutWhatTheLiveGuardBlocks(t *testing.T) {
	repo := liveFixture(t)
	pin, err := snapshot.PinWorktree(repo, nil)
	if err != nil {
		t.Fatalf("PinWorktree: %v", err)
	}
	tree := PinnedWorktree(pin.Tree)
	for _, p := range []string{".git/config", ".env", "build/out.go", "credentials.json"} {
		if res := readFileTool(repo, map[string]any{"path": p}, tree); res["error"] == nil {
			t.Errorf("pinned read_file(%q) served content: %s", p, asJSON(res))
		}
	}
	if res := readFileTool(repo, map[string]any{"path": "new.go"}, tree); res["error"] != nil {
		t.Errorf("pinned tree lost an untracked reviewed file: %v", res["error"])
	}
}

// TestToolResultsAreBounded: every exploration tool result stays within
// toolOutputCap whatever the model requests or the repository holds.
func TestToolResultsAreBounded(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "wide.txt", strings.Repeat(strings.Repeat("A", 100_000)+"\n", 40))
	writeFile(t, repo, "tall.txt", strings.Repeat("short line\n", 200_000))
	writeFile(t, repo, "min.js", strings.Repeat("B", 5_000_000))
	var many strings.Builder
	for i := 0; i < 3000; i++ {
		fmt.Fprintf(&many, "needle %d %s\n", i, strings.Repeat("x", 500))
	}
	writeFile(t, repo, "many.txt", many.String())
	long := strings.Repeat("d", 200)
	for i := 0; i < 400; i++ {
		writeFile(t, repo, fmt.Sprintf("%s/%s/f%03d.go", long, long, i), "package p\n")
	}
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "big")
	pin, err := snapshot.PinWorktree(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	const slack = 512 // the trailing hint lines
	for _, tree := range []Tree{{}, PinnedWorktree(pin.Tree)} {
		name := "live"
		if tree.Pinned() {
			name = "pinned"
		}
		for _, args := range []map[string]any{
			{"path": "wide.txt", "limit": 1e9},
			{"path": "tall.txt", "limit": 1e9},
			{"path": "tall.txt", "offset": 1e18, "limit": 1e18},
			{"path": "min.js"},
		} {
			res := readFileTool(repo, args, tree)
			content, _ := res["content"].(string)
			if len(content) > toolOutputCap+slack {
				t.Errorf("%s read_file(%v) returned %d bytes", name, args, len(content))
			}
			if args["path"] == "wide.txt" && (!strings.Contains(content, "output capped") || fmt.Sprint(res["more_below"]) != "true") {
				t.Errorf("%s read_file must say it capped the output: %.200s", name, content)
			}
		}
		res := readFileTool(repo, map[string]any{"path": "wide.txt", "offset": 1, "limit": 3}, tree)
		if r := fmt.Sprint(res["returned_range"]); r != "[1 1]" || !strings.Contains(res["content"].(string), "offset>=2") {
			t.Errorf("%s read_file must resume after the last whole line served: range %s", name, r)
		}
		for _, pattern := range []string{"needle", "B"} {
			out := asJSON(grepRepoTool(repo, map[string]any{"pattern": pattern}, tree))
			if len(out) > toolOutputCap+slack {
				t.Errorf("%s grep_repo(%s) returned %d bytes", name, pattern, len(out))
			}
			if !strings.Contains(out, "truncated") {
				t.Errorf("%s grep_repo(%s) must mark the truncation: %.200s", name, pattern, out)
			}
		}
		if out := asJSON(listFilesTool(repo, map[string]any{"pattern": "*.go"}, tree)); len(out) > toolOutputCap+slack || !strings.Contains(out, "truncated at") {
			t.Errorf("%s list_files returned %d bytes without a truncation marker", name, len(out))
		}
	}
}

// TestLongLineDoesNotSwallowTheNextLine: a line longer than the per-line cap
// but shorter than the read buffer arrives with its newline in the same
// chunk. Cutting it must not leave the reader to "drain" into the next line.
func TestLongLineDoesNotSwallowTheNextLine(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "f.txt", strings.Repeat("A", maxLineBytes+100)+"\nsecond line\n"+
		"needle "+strings.Repeat("n", grepLineBytes+100)+"\nneedle short\n")
	res := readFileTool(repo, map[string]any{"path": "f.txt"}, Tree{})
	content, _ := res["content"].(string)
	if !strings.Contains(content, "    2  second line") || !strings.Contains(content, "line truncated at") {
		t.Errorf("read_file lost the line after a cut one: %.120s ... %s", content, content[max(0, len(content)-300):])
	}
	got := asJSON(grepRepoTool(repo, map[string]any{"pattern": "needle"}, Tree{}))
	if !strings.Contains(got, "needle short") || !strings.Contains(got, "line truncated") {
		t.Errorf("grep_repo lost the match after a cut line: %.300s", got)
	}
}

// setupBranchRepo builds a repo with a committed base and a feature commit
// that changes changedFile, returning the repo path and the mergebase..HEAD
// range spec the GitDiffTool binds at construction.
func setupBranchRepo(t *testing.T, changedFile, baseContent, headContent string) (repo, rangeSpec string) {
	t.Helper()
	repo = initRepo(t)
	writeFile(t, repo, changedFile, baseContent)
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "base")
	base := gitOID(t, repo, "HEAD")

	writeFile(t, repo, changedFile, headContent)
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "feature change")
	head := gitOID(t, repo, "HEAD")

	return repo, base + ".." + head
}

func gitOID(t *testing.T, repo, ref string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "rev-parse", ref).CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse %s: %v\n%s", ref, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestGitDiffTool_ShowsFileDelta verifies a basic changed-file diff over the
// bound range, and that the rangeSpec (not the model) controls what's compared.
func TestGitDiffTool_ShowsFileDelta(t *testing.T) {
	repo, rangeSpec := setupBranchRepo(t, "f.go", "package p\n// original\n", "package p\n// modified\n")
	tool := GitDiffTool(repo, rangeSpec, []string{"f.go"})

	res, err := tool.Run(t.Context(), map[string]any{"path": "f.go"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	m := res.(map[string]any)
	content, _ := m["content"].(string)
	if !strings.Contains(content, "+// modified") {
		t.Errorf("expected added line in diff, got: %s", content)
	}
	if !strings.Contains(content, "-// original") {
		t.Errorf("expected removed line in diff, got: %s", content)
	}
	if m["truncated"].(bool) {
		t.Error("small diff should not be truncated")
	}
	if m["total_lines"].(int) == 0 {
		t.Error("expected non-zero total_lines")
	}
}

// TestGitDiffTool_RangeBinding verifies the range is bound at construction: a
// tool built with an empty range (base..base) sees no delta even though the
// file changed in a later commit.
func TestGitDiffTool_RangeBinding(t *testing.T) {
	repo, _ := setupBranchRepo(t, "f.go", "package p\n// v1\n", "package p\n// v2\n")
	base := gitOID(t, repo, "HEAD~1")

	// Bind the tool to base..base — an empty range. Model cannot widen it.
	tool := GitDiffTool(repo, base+".."+base, []string{"f.go"})
	res, err := tool.Run(t.Context(), map[string]any{"path": "f.go"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	m := res.(map[string]any)
	if m["total_lines"].(int) != 0 {
		t.Errorf("expected empty diff for base..base range, got %d lines: %v", m["total_lines"], m["content"])
	}
}

// TestGitDiffTool_Windowing verifies offset/limit windowing over the diff's
// lines with more_before/more_below flags.
func TestGitDiffTool_Windowing(t *testing.T) {
	// Base file with 100 lines; head replaces every line so the diff is large.
	var baseLines, headLines []string
	for i := 1; i <= 100; i++ {
		baseLines = append(baseLines, fmt.Sprintf("old line %d", i))
		headLines = append(headLines, fmt.Sprintf("new line %d", i))
	}
	repo, rangeSpec := setupBranchRepo(t, "big.go",
		strings.Join(baseLines, "\n")+"\n",
		strings.Join(headLines, "\n")+"\n")
	tool := GitDiffTool(repo, rangeSpec, []string{"big.go"})

	// First window.
	res, _ := tool.Run(t.Context(), map[string]any{"path": "big.go", "offset": 1, "limit": 10})
	m := res.(map[string]any)
	if m["more_below"].(bool) != true {
		t.Error("expected more_below for a truncated window over a large diff")
	}
	if m["more_before"].(bool) != false {
		t.Error("first window should have more_before=false")
	}
	firstContent := m["content"].(string)

	// A later window should differ and report more_before.
	res2, _ := tool.Run(t.Context(), map[string]any{"path": "big.go", "offset": 20, "limit": 10})
	m2 := res2.(map[string]any)
	if m2["more_before"].(bool) != true {
		t.Error("expected more_before=true for a window past line 1")
	}
	if m2["content"].(string) == firstContent {
		t.Error("later window should return different content")
	}
}

// TestGitDiffTool_ByteCap verifies the hard byte cap fires regardless of the
// requested limit.
func TestGitDiffTool_ByteCap(t *testing.T) {
	// Build a diff whose windowed slice exceeds gitDiffByteCap. Each changed
	// line is ~1 KB; 200 of them well exceeds the 64 KiB cap.
	var baseLines, headLines []string
	for i := 1; i <= 200; i++ {
		baseLines = append(baseLines, fmt.Sprintf("old-%d-%s", i, strings.Repeat("x", 1000)))
		headLines = append(headLines, fmt.Sprintf("new-%d-%s", i, strings.Repeat("y", 1000)))
	}
	repo, rangeSpec := setupBranchRepo(t, "huge.go",
		strings.Join(baseLines, "\n")+"\n",
		strings.Join(headLines, "\n")+"\n")
	tool := GitDiffTool(repo, rangeSpec, []string{"huge.go"})

	// Request a huge limit; the byte cap must clamp the output anyway.
	res, _ := tool.Run(t.Context(), map[string]any{"path": "huge.go", "offset": 1, "limit": 100000})
	m := res.(map[string]any)
	content := m["content"].(string)
	if len(content) > gitDiffByteCap {
		t.Errorf("content %d bytes exceeds cap %d", len(content), gitDiffByteCap)
	}
	if !m["truncated"].(bool) {
		t.Error("expected truncated=true when byte cap fires")
	}
	if !m["more_below"].(bool) {
		t.Error("expected more_below=true when output was byte-capped mid-diff")
	}
}

// TestGitDiffTool_OneFileEnforcement verifies globs, pathspec magic, and
// directory paths are rejected.
func TestGitDiffTool_OneFileEnforcement(t *testing.T) {
	repo, rangeSpec := setupBranchRepo(t, "f.go", "a\n", "b\n")
	tool := GitDiffTool(repo, rangeSpec, []string{"f.go"})

	bad := []string{
		"*.go",
		"internal/**",
		":(exclude)vendor",
		":/f.go",
		"internal/",
		"a[bc].go",
	}
	for _, p := range bad {
		res, _ := tool.Run(t.Context(), map[string]any{"path": p})
		m := res.(map[string]any)
		if _, ok := m["error"]; !ok {
			t.Errorf("path %q should have been rejected as not-a-single-file, got: %v", p, m)
		}
	}
}

// TestGitDiffTool_ExcludedPathRejected verifies a path that is NOT a member
// of the manifest's allowed file set is rejected even though git could diff
// it. This is the case that used to be handled by a special-cased
// scope.IsDefaultIgnored check; exact-membership subsumes it.
func TestGitDiffTool_ExcludedPathRejected(t *testing.T) {
	repo, rangeSpec := setupBranchRepo(t, "vendor/lib/x.go", "package p\n// a\n", "package p\n// b\n")
	tool := GitDiffTool(repo, rangeSpec, nil) // manifest did not include this file

	res, _ := tool.Run(t.Context(), map[string]any{"path": "vendor/lib/x.go"})
	m := res.(map[string]any)
	if _, ok := m["error"]; !ok {
		t.Errorf("path not in the allowed set should have been rejected, got: %v", m)
	}
}

// TestGitDiffTool_IncludeVendoredAllowed verifies a vendored path IS allowed
// when it is present in the manifest's allowed set (--include-vendored).
func TestGitDiffTool_IncludeVendoredAllowed(t *testing.T) {
	repo, rangeSpec := setupBranchRepo(t, "vendor/lib/x.go", "package p\n// a\n", "package p\n// b\n")
	tool := GitDiffTool(repo, rangeSpec, []string{"vendor/lib/x.go"})

	res, err := tool.Run(t.Context(), map[string]any{"path": "vendor/lib/x.go"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	m := res.(map[string]any)
	if _, ok := m["error"]; ok {
		t.Errorf("vendored path present in the allowed set should not be rejected, got: %v", m)
	}
}

// TestGitDiffTool_DirectoryNotInManifestRejected verifies a bare directory
// path that exists on disk (no trailing slash, so it passes the one-file
// syntactic checks) is still rejected because a directory is never a member
// of the manifest's file set.
func TestGitDiffTool_DirectoryNotInManifestRejected(t *testing.T) {
	repo, rangeSpec := setupBranchRepo(t, "internal/context/f.go", "a\n", "b\n")
	tool := GitDiffTool(repo, rangeSpec, []string{"internal/context/f.go"})

	res, _ := tool.Run(t.Context(), map[string]any{"path": "internal/context"})
	m := res.(map[string]any)
	if _, ok := m["error"]; !ok {
		t.Errorf("bare directory path should have been rejected (not a manifest member), got: %v", m)
	}
}

// TestGitDiffTool_PathEscapeRejected verifies path traversal is rejected via
// the ResolveUnderRepo chokepoint.
func TestGitDiffTool_PathEscapeRejected(t *testing.T) {
	repo, rangeSpec := setupBranchRepo(t, "f.go", "a\n", "b\n")
	tool := GitDiffTool(repo, rangeSpec, []string{"f.go"})

	res, _ := tool.Run(t.Context(), map[string]any{"path": "../../etc/passwd"})
	m := res.(map[string]any)
	if _, ok := m["error"]; !ok {
		t.Errorf("escaping path should have been rejected, got: %v", m)
	}
}

// TestGitDiffReadersIgnoreUserDiffDrivers: git_diff and the survey range
// grounder are patch-producing git calls too. A repository attribute naming
// the user's textconv driver must not run it, and color or prefix settings
// must not change the text they parse.
func TestGitDiffReadersIgnoreUserDiffDrivers(t *testing.T) {
	repo, rangeSpec := setupBranchRepo(t, "f.go", "package p\n// original\n", "package p\n// modified\n")
	writeFile(t, repo, ".gitattributes", "*.go diff=pdf\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "route .go to the pdf driver")
	rangeSpec = strings.SplitN(rangeSpec, "..", 2)[0] + ".." + gitOID(t, repo, "HEAD")

	dir := t.TempDir()
	ran := filepath.Join(dir, "driver-ran")
	script := filepath.Join(dir, "driver.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch '"+ran+"'\necho CONVERTED\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "gitconfig")
	body := "[diff \"pdf\"]\n\ttextconv = " + script + "\n[diff]\n\texternal = " + script + "\n\tnoprefix = true\n[color]\n\tui = always\n"
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	res, err := GitDiffTool(repo, rangeSpec, []string{"f.go"}).Run(t.Context(), map[string]any{"path": "f.go"})
	if err != nil {
		t.Fatal(err)
	}
	content, _ := res.(map[string]any)["content"].(string)
	if strings.Contains(content, "CONVERTED") || strings.Contains(content, "\x1b") || !strings.Contains(content, "+++ b/f.go") || !strings.Contains(content, "+// modified") {
		t.Errorf("git_diff did not return the plain diff: %q", content)
	}
	if !NewQuoteGrounder(repo, Tree{}, "", rangeSpec)("f.go", "// original") {
		t.Error("the range grounder must parse the plain old side")
	}
	if _, err := os.Stat(ran); err == nil {
		t.Error("a user diff driver ran on reviewed content")
	}
}
