package snapshot_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/roast/internal/snapshot"
)

// git runs a git command in dir and fails the test on error.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, stderr)
	}
	return string(out)
}

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// messyRepo builds a repo with every state that has to survive a round trip:
// a clean committed file, a staged modification, an unstaged modification, an
// unstaged deletion, an untracked file, an untracked file with the exec bit, an
// untracked symlink, a .gitignore'd secret, and an untracked-non-ignored file
// whose name matches the secret denylist.
func messyRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "config", "user.email", "test@example.com")
	git(t, dir, "config", "user.name", "test")

	write(t, dir, ".gitignore", "ignored-secret.txt\n")
	write(t, dir, "clean.go", "package a\n\nfunc Clean() {}\n")
	write(t, dir, "staged.go", "package a\n\nfunc Staged() {}\n")
	write(t, dir, "unstaged.go", "package a\n\nfunc Unstaged() {}\n")
	write(t, dir, "doomed.go", "package a\n\nfunc Doomed() {}\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "base")

	write(t, dir, "staged.go", "package a\n\nfunc Staged() { println(\"staged edit\") }\n")
	git(t, dir, "add", "staged.go")
	write(t, dir, "unstaged.go", "package a\n\nfunc Unstaged() { println(\"unstaged edit\") }\n")
	if err := os.Remove(filepath.Join(dir, "doomed.go")); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "untracked.go", "package a\n\nfunc Untracked() {}\n")
	write(t, dir, "script.sh", "#!/bin/sh\necho hi\n")
	if err := os.Chmod(filepath.Join(dir, "script.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("clean.go", filepath.Join(dir, "link.go")); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "ignored-secret.txt", "AKIA-IGNORED-SECRET\n")
	write(t, dir, "deploy.pem", "-----BEGIN PRIVATE KEY-----\nDENYLISTED\n")
	return dir
}

func headOID(t *testing.T, dir string) string {
	t.Helper()
	return strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))
}

// TestRoundTrip is the fidelity claim: restoring a snapshot of a deliberately
// messy tree reproduces both diffs byte-identically and the same status modulo
// the deliberately-excluded denylisted file.
func TestRoundTrip(t *testing.T) {
	src := messyRepo(t)
	head := headOID(t, src)

	beforeStatus := git(t, src, "status", "--porcelain")
	beforeCached := git(t, src, "diff", "--cached", "HEAD")
	beforeWorking := git(t, src, "diff", "HEAD")

	snap, err := snapshot.Capture(src, snapshot.Options{RunID: "20260725T000000Z-deadbeef", HeadOID: head})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if snap.Commit == "" || snap.IndexTree == "" || snap.WorktreeTree == "" {
		t.Fatalf("incomplete snapshot: %+v", snap)
	}
	if len(snap.Excluded) != 1 || snap.Excluded[0] != "deploy.pem" {
		t.Fatalf("Excluded = %v, want [deploy.pem]", snap.Excluded)
	}

	// The live index must be untouched: the staged set is still exactly what
	// it was, with no untracked file swept in by the snapshot's `add -A`.
	if got := git(t, src, "status", "--porcelain"); got != beforeStatus {
		t.Errorf("capture mutated the live repo state:\nbefore:\n%s\nafter:\n%s", beforeStatus, got)
	}

	dest := filepath.Join(t.TempDir(), "restored")
	if _, err := snapshot.Restore(src, "20260725T000000Z-deadbeef", dest); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	t.Cleanup(func() { _ = snapshot.RemoveWorktree(src, dest) })

	if got := git(t, dest, "diff", "--cached", "HEAD"); got != beforeCached {
		t.Errorf("staged diff differs after restore:\nwant:\n%s\ngot:\n%s", beforeCached, got)
	}
	if got := git(t, dest, "diff", "HEAD"); got != beforeWorking {
		t.Errorf("working diff differs after restore:\nwant:\n%s\ngot:\n%s", beforeWorking, got)
	}

	// Status matches except for the denylisted file, which was deliberately
	// not captured.
	wantStatus := dropLine(beforeStatus, "deploy.pem")
	if got := git(t, dest, "status", "--porcelain"); got != wantStatus {
		t.Errorf("status differs after restore:\nwant:\n%s\ngot:\n%s", wantStatus, got)
	}

	// Untracked content byte-identical, exec bit preserved, symlink still a
	// symlink.
	if got := readFile(t, dest, "untracked.go"); got != "package a\n\nfunc Untracked() {}\n" {
		t.Errorf("untracked.go content = %q", got)
	}
	info, err := os.Lstat(filepath.Join(dest, "script.sh"))
	if err != nil {
		t.Fatalf("script.sh missing after restore: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("script.sh lost its exec bit: mode %v", info.Mode())
	}
	link, err := os.Lstat(filepath.Join(dest, "link.go"))
	if err != nil {
		t.Fatalf("link.go missing after restore: %v", err)
	}
	if link.Mode()&os.ModeSymlink == 0 {
		t.Errorf("link.go is not a symlink after restore: mode %v", link.Mode())
	}
}

// TestSecretHygiene proves the three cases the snapshot denylist has to
// get right.
func TestSecretHygiene(t *testing.T) {
	src := messyRepo(t)
	snap, err := snapshot.Capture(src, snapshot.Options{RunID: "run-secret", HeadOID: headOID(t, src)})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	tree := git(t, src, "ls-tree", "-r", "--name-only", snap.Ref+":worktree")
	has := func(name string) bool {
		for _, l := range strings.Split(tree, "\n") {
			if l == name {
				return true
			}
		}
		return false
	}

	if has("ignored-secret.txt") {
		t.Error("gitignored secret was captured into the worktree tree")
	}
	if has("deploy.pem") {
		t.Error("denylisted untracked file was captured into the worktree tree")
	}
	if !has("untracked.go") {
		t.Error("normal untracked file was NOT captured into the worktree tree")
	}
	if len(snap.Excluded) != 1 || snap.Excluded[0] != "deploy.pem" {
		t.Errorf("Excluded = %v, want [deploy.pem] recorded", snap.Excluded)
	}

	// And the excluded file is still on disk in the source repo — the
	// denylist filters the capture, it does not touch the user's tree.
	if _, err := os.Stat(filepath.Join(src, "deploy.pem")); err != nil {
		t.Errorf("capture removed the denylisted file from the working tree: %v", err)
	}
}

// TestDenylistLiteralPathspec guards the vendor-escape bug class: a denylisted
// filename containing pathspec metacharacters must exclude only itself.
func TestDenylistLiteralPathspec(t *testing.T) {
	src := messyRepo(t)
	write(t, src, "*.key", "literal-star-name\n")
	write(t, src, "keep.go", "package a\n")

	snap, err := snapshot.Capture(src, snapshot.Options{RunID: "run-literal", HeadOID: headOID(t, src)})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	tree := git(t, src, "ls-tree", "-r", "--name-only", snap.Ref+":worktree")
	if strings.Contains(tree, "*.key") {
		t.Error("literally-named `*.key` was captured")
	}
	if !strings.Contains(tree, "keep.go") {
		t.Error("`*.key` exclusion swallowed unrelated files")
	}
}

// TestCaptureUnmergedIndexFails documents the degrade path: write-tree refuses
// on conflicts, and the caller records snapshot: failed rather than aborting.
func TestCaptureUnmergedIndexFails(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "config", "user.email", "test@example.com")
	git(t, dir, "config", "user.name", "test")
	write(t, dir, "f.txt", "base\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "base")
	git(t, dir, "checkout", "-q", "-b", "other")
	write(t, dir, "f.txt", "other\n")
	git(t, dir, "commit", "-q", "-am", "other")
	git(t, dir, "checkout", "-q", "main")
	write(t, dir, "f.txt", "main\n")
	git(t, dir, "commit", "-q", "-am", "main")
	// Deliberately ignore the merge's non-zero exit: the conflict is the point.
	_ = exec.Command("git", "-C", dir, "merge", "other").Run()

	if _, err := snapshot.Capture(dir, snapshot.Options{RunID: "run-conflict", HeadOID: headOID(t, dir)}); err == nil {
		t.Fatal("Capture on an unmerged index should fail so the caller can degrade")
	}
}

// TestCaptureRefusesUnbornHead: a repo with no commits cannot be restored (the
// only commit-ish would be the composite snapshot commit, whose tree is
// index/+worktree/, not a baseline that ever existed). Refusing beats
// advertising a snapshot that replay would have to reject.
func TestCaptureRefusesUnbornHead(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	write(t, dir, "new.go", "package a\n")
	git(t, dir, "add", "-A")

	_, err := snapshot.Capture(dir, snapshot.Options{RunID: "run-unborn"})
	if err == nil {
		t.Fatal("Capture must refuse a repository with no commits")
	}
	if !strings.Contains(err.Error(), "no commits") {
		t.Errorf("error should name the cause: %v", err)
	}
	if refExists(t, dir, snapshot.RefPrefix+"run-unborn") {
		t.Error("a refused capture must not leave a ref behind")
	}
}

// TestCaptureRecordsParents verifies HEAD and the branch base are pinned as
// commit parents so a later branch delete cannot strand the reviewed range.
func TestCaptureRecordsParents(t *testing.T) {
	src := messyRepo(t)
	head := headOID(t, src)
	write(t, src, "second.go", "package a\n")
	git(t, src, "add", "second.go")
	git(t, src, "commit", "-q", "-m", "second")
	newHead := headOID(t, src)

	snap, err := snapshot.Capture(src, snapshot.Options{RunID: "run-parents", HeadOID: newHead, BaseOID: head})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	loaded, err := snapshot.Load(src, "run-parents")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Head != newHead {
		t.Errorf("Head = %s, want %s", loaded.Head, newHead)
	}
	if loaded.Base != head {
		t.Errorf("Base = %s, want %s", loaded.Base, head)
	}
	if loaded.IndexTree != snap.IndexTree || loaded.WorktreeTree != snap.WorktreeTree {
		t.Errorf("Load disagrees with Capture: %+v vs %+v", loaded, snap)
	}
}

// TestSurvivesGC is the durability claim: the ref keeps every snapshot object
// reachable through an aggressive gc.
func TestSurvivesGC(t *testing.T) {
	src := messyRepo(t)
	if _, err := snapshot.Capture(src, snapshot.Options{RunID: "run-gc", HeadOID: headOID(t, src)}); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	git(t, src, "gc", "--prune=now", "--aggressive", "-q")
	if _, err := snapshot.Load(src, "run-gc"); err != nil {
		t.Fatalf("snapshot did not survive gc: %v", err)
	}
}

// TestPrune deletes only aged refs under roast's own namespace.
func TestPrune(t *testing.T) {
	src := messyRepo(t)
	if _, err := snapshot.Capture(src, snapshot.Options{RunID: "run-fresh", HeadOID: headOID(t, src)}); err != nil {
		t.Fatalf("Capture: %v", err)
	}

	// A ref that looks old: commit-tree with a backdated committer date.
	old := backdatedSnapshot(t, src, "run-old", 60*24*time.Hour)

	dry, err := snapshot.Prune(src, 30*24*time.Hour, true)
	if err != nil {
		t.Fatalf("Prune(dry): %v", err)
	}
	if len(dry) != 1 || dry[0].Ref != old || dry[0].Deleted {
		t.Fatalf("dry run = %+v, want one non-deleted %s", dry, old)
	}
	if !refExists(t, src, old) {
		t.Fatal("dry run deleted the ref")
	}

	got, err := snapshot.Prune(src, 30*24*time.Hour, false)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(got) != 1 || !got[0].Deleted {
		t.Fatalf("Prune = %+v, want one deleted", got)
	}
	if refExists(t, src, old) {
		t.Error("aged ref survived prune")
	}
	if _, err := snapshot.Load(src, "run-fresh"); err != nil {
		t.Errorf("fresh ref was pruned: %v", err)
	}
}

func TestEnabled(t *testing.T) {
	t.Setenv("ROAST_SNAPSHOT", "")
	if !snapshot.Enabled() {
		t.Error("capture must be default-on")
	}
	for _, v := range []string{"0", "false", "no", "OFF"} {
		t.Setenv("ROAST_SNAPSHOT", v)
		if snapshot.Enabled() {
			t.Errorf("ROAST_SNAPSHOT=%q should disable capture", v)
		}
	}
}

// backdatedSnapshot mints a snapshot-shaped ref with an old committer date so
// prune has something aged to find.
func backdatedSnapshot(t *testing.T, repo, runID string, age time.Duration) string {
	t.Helper()
	when := time.Now().Add(-age).Format(time.RFC3339)
	tree := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD^{tree}"))
	cmd := exec.Command("git", "-C", repo, "commit-tree", tree, "-m", "old snapshot")
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=roast", "GIT_AUTHOR_EMAIL=roast@localhost",
		"GIT_COMMITTER_NAME=roast", "GIT_COMMITTER_EMAIL=roast@localhost",
		"GIT_AUTHOR_DATE="+when, "GIT_COMMITTER_DATE="+when,
	)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("commit-tree: %v", err)
	}
	ref := snapshot.RefPrefix + runID
	git(t, repo, "update-ref", ref, strings.TrimSpace(string(out)))
	return ref
}

// refExists reports whether a ref resolves, without requiring it to be a
// well-formed composite snapshot (the prune fixture is a bare commit).
func refExists(t *testing.T, repo, ref string) bool {
	t.Helper()
	return exec.Command("git", "-C", repo, "rev-parse", "--verify", "--quiet", ref).Run() == nil
}

func readFile(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// dropLine removes every line containing needle.
func dropLine(s, needle string) string {
	var kept []string
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, needle) {
			continue
		}
		kept = append(kept, l)
	}
	return strings.Join(kept, "\n")
}

func TestCaptureHonorsGitIndexFile(t *testing.T) {
	// Under the native pre-commit gate, git points GIT_INDEX_FILE at the
	// temporary index holding exactly the commit's content, and roast's
	// --staged scope reads that. The snapshot must capture the same index,
	// or a path-scoped gate run records a state that was never reviewed and
	// fails its own replay digest check.
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "base.txt")
	run("commit", "-qm", "init")

	// Live index: stage a.txt. Alternate index: stage b.txt only.
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "a.txt")
	if err := os.WriteFile(filepath.Join(repo, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	alt := filepath.Join(t.TempDir(), "alt-index")
	cmd := exec.Command("git", "add", "b.txt")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+alt)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("stage into alt index: %v\n%s", err, out)
	}

	head := snapshot.HeadOID(repo)
	t.Setenv("GIT_INDEX_FILE", alt)
	snap, err := snapshot.Capture(repo, snapshot.Options{RunID: "20260727T000000Z-deadbeef", HeadOID: head})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}

	// The captured index tree must reflect the ALTERNATE index (b.txt
	// staged), not the live one (a.txt staged).
	ls := exec.Command("git", "ls-tree", "--name-only", snap.Ref+":index")
	ls.Dir = repo
	out, err := ls.CombinedOutput()
	if err != nil {
		t.Fatalf("ls-tree: %v\n%s", err, out)
	}
	got := string(out)
	if !strings.Contains(got, "b.txt") {
		t.Errorf("captured index tree should contain b.txt (the alternate index's content), got:\n%s", got)
	}
	if strings.Contains(got, "a.txt") {
		t.Errorf("captured index tree must NOT contain a.txt (that is the live index, not the reviewed one), got:\n%s", got)
	}
}

func treeNames(t *testing.T, dir, tree string) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	for _, l := range strings.Split(git(t, dir, "ls-tree", "-r", "--name-only", tree), "\n") {
		if l != "" {
			names[l] = true
		}
	}
	return names
}

// TestPinWorktreeMatchesCapture: the pinned corpus and the recorded snapshot
// must be the same tree, and Capture must reuse a pin rather than re-pin.
func TestPinWorktreeMatchesCapture(t *testing.T) {
	src := messyRepo(t)
	pin, err := snapshot.PinWorktree(src, nil)
	if err != nil {
		t.Fatalf("PinWorktree: %v", err)
	}
	names := treeNames(t, src, pin.Tree)
	if names["ignored-secret.txt"] || names["deploy.pem"] {
		t.Errorf("pin captured an ignored or denylisted file: %v", names)
	}
	if !names["untracked.go"] || !names["unstaged.go"] {
		t.Errorf("pin dropped reviewable content: %v", names)
	}
	if len(pin.Excluded) != 1 || pin.Excluded[0] != "deploy.pem" {
		t.Errorf("Excluded = %v, want [deploy.pem]", pin.Excluded)
	}

	snap, err := snapshot.Capture(src, snapshot.Options{RunID: "run-pinned", HeadOID: headOID(t, src), Worktree: pin})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if snap.WorktreeTree != pin.Tree || len(snap.Excluded) != 1 {
		t.Errorf("Capture recorded %s %v, want the pin %s %v", snap.WorktreeTree, snap.Excluded, pin.Tree, pin.Excluded)
	}
	fresh, err := snapshot.PinWorktree(src, nil)
	if err != nil || fresh.Tree != pin.Tree {
		t.Errorf("an unchanged tree must pin to the same OID: %s vs %s (%v)", fresh.Tree, pin.Tree, err)
	}
}

// TestPinWorktreeWithoutHead: a repository with no commits cannot be recorded
// as a replayable snapshot, but its review must still be pinned.
func TestPinWorktreeWithoutHead(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	write(t, dir, ".gitignore", ".env\n")
	write(t, dir, "new.go", "package a\n")
	write(t, dir, ".env", "TOKEN=ignored\n")
	write(t, dir, ".env.local", "TOKEN=denylisted\n")

	pin, err := snapshot.PinWorktree(dir, nil)
	if err != nil {
		t.Fatalf("PinWorktree without HEAD: %v", err)
	}
	names := treeNames(t, dir, pin.Tree)
	if !names["new.go"] || names[".env"] || names[".env.local"] {
		t.Errorf("unexpected pinned tree %v", names)
	}
	if out := git(t, dir, "for-each-ref", snapshot.RefPrefix); out != "" {
		t.Errorf("pinning must not record a ref: %s", out)
	}
}

// TestPinWorktreeUnmergedIndex: Capture degrades on a conflicted index, but
// pinning resolves the conflict in a private index copy and never touches the
// live one.
func TestPinWorktreeUnmergedIndex(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "config", "user.email", "test@example.com")
	git(t, dir, "config", "user.name", "test")
	write(t, dir, "f.txt", "base\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "base")
	git(t, dir, "checkout", "-q", "-b", "other")
	write(t, dir, "f.txt", "other\n")
	git(t, dir, "commit", "-q", "-am", "other")
	git(t, dir, "checkout", "-q", "main")
	write(t, dir, "f.txt", "main\n")
	git(t, dir, "commit", "-q", "-am", "main")
	_ = exec.Command("git", "-C", dir, "merge", "other").Run()

	pin, err := snapshot.PinWorktree(dir, nil)
	if err != nil {
		t.Fatalf("PinWorktree on an unmerged index: %v", err)
	}
	blob := git(t, dir, "cat-file", "blob", pin.Tree+":f.txt")
	if !strings.Contains(blob, "<<<<<<<") {
		t.Errorf("pinned f.txt should hold the conflicted worktree content, got %q", blob)
	}
	if unmerged := git(t, dir, "ls-files", "-u"); unmerged == "" {
		t.Error("pinning must not resolve the conflict in the live index")
	}
}

func TestDenylistCoversCredentialFiles(t *testing.T) {
	for _, name := range []string{
		".env.local", "config/.env", "id_ed25519", "id_rsa", "server.pem", "tls.key",
		".npmrc", ".pypirc", ".netrc", ".git-credentials", "credentials.json",
		"sub/credentials.json", "creds.txt", "cert.p12", ".ENV", "Config/.Env.Local", "ID_ED25519", "Credentials.JSON",
		"id_ed25519_github", "id_rsa_work", "ID_ECDSA_deploy", "id_ecdsa_sk", "id_ed25519_sk.pub", "keys/id_xmss_old", "id_dsa.bak",
	} {
		if !snapshot.Denylisted(name) {
			t.Errorf("%s should be denylisted", name)
		}
	}
	for _, name := range []string{"main.go", "credentials.go", "npmrc.go", "README.md", "id_map.go", "internal/id_test.go", "identity.go"} {
		if snapshot.Denylisted(name) {
			t.Errorf("%s should not be denylisted", name)
		}
	}
}
