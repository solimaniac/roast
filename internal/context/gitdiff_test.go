package context

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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
