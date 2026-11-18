package gate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newRepo creates an isolated temp git repo. The developer's own ~/.gitconfig
// is deliberately cut out (GIT_CONFIG_GLOBAL/SYSTEM): a machine that sets
// core.hooksPath globally would otherwise redirect every hook path these
// tests assert on.
func newRepo(t *testing.T) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	dir := t.TempDir()
	// macOS puts TempDir under /var, a symlink to /private/var; git reports
	// the resolved path, so resolve here or every path comparison drifts.
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	git(t, dir, "init", "-q")
	// These disposable repos do not need background object maintenance. Git
	// commits may detach it, racing TempDir's removal of .git/objects.
	git(t, dir, "config", "maintenance.auto", "false")
	git(t, dir, "config", "gc.auto", "0")
	git(t, dir, "config", "user.email", "roast-test@example.com")
	git(t, dir, "config", "user.name", "roast test")
	git(t, dir, "config", "commit.gpgsign", "false")
	return dir
}

func git(t *testing.T, repo string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestRepoDisablesAutomaticMaintenance(t *testing.T) {
	repo := newRepo(t)
	for key, want := range map[string]string{"maintenance.auto": "false", "gc.auto": "0"} {
		if got := git(t, repo, "config", "--local", "--get", key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func writeHook(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestHookScriptCarriesMarker guards the recognition contract: Inspect finds
// our own hook by looking for HookMarker in the file, so the embedded script
// must actually contain it.
func TestHookScriptCarriesMarker(t *testing.T) {
	if !strings.Contains(HookScript, HookMarker) {
		t.Fatalf("embedded hook script does not contain %q", HookMarker)
	}
	if !strings.HasPrefix(HookScript, "#!/bin/sh\n") {
		t.Error("hook script must be POSIX sh")
	}
}

func TestInspectNotEnrolled(t *testing.T) {
	repo := newRepo(t)
	st, err := Inspect(repo)
	if err != nil {
		t.Fatal(err)
	}
	if st.Exists || st.Active() || st.Decision != DecisionNone {
		t.Fatalf("fresh repo should be unenrolled: %+v", st)
	}
	if want := filepath.Join(repo, ".git", "hooks", "pre-commit"); st.HookPath != want {
		t.Errorf("HookPath = %q, want %q", st.HookPath, want)
	}
	if !strings.Contains(st.Summary(), "not enrolled") {
		t.Errorf("summary = %q", st.Summary())
	}
}

func TestInstallCreatesExecutableHook(t *testing.T) {
	repo := newRepo(t)
	action, st, err := Install(repo)
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionInstalled {
		t.Fatalf("action = %q, want %q", action, ActionInstalled)
	}
	if got := read(t, st.HookPath); got != HookScript {
		t.Error("installed hook does not match the embedded script")
	}
	info, err := os.Stat(st.HookPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("hook is not executable: %v", info.Mode())
	}

	// Idempotent.
	again, _, err := Install(repo)
	if err != nil {
		t.Fatal(err)
	}
	if again != ActionUpToDate {
		t.Errorf("second install = %q, want %q", again, ActionUpToDate)
	}
}

// TestInstallRefreshesStaleOwnHook covers the "present-and-ours-but-stale →
// refresh silently" case: an older roast wrote the file, so rewriting it is
// not a clobber.
func TestInstallRefreshesStaleOwnHook(t *testing.T) {
	repo := newRepo(t)
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	writeHook(t, hook, "#!/bin/sh\n"+HookMarker+"\n# an older version\nexit 0\n", 0o755)

	st, err := Inspect(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Ours || !st.Stale {
		t.Fatalf("expected ours+stale, got %+v", st)
	}

	action, _, err := Install(repo)
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionRefreshed {
		t.Fatalf("action = %q, want %q", action, ActionRefreshed)
	}
	if read(t, hook) != HookScript {
		t.Error("stale hook was not refreshed to the embedded script")
	}
}

// TestInstallRepairsLostExecuteBit: a hook git will not run is as broken as a
// missing one, and it is ours, so repairing it needs no consent.
func TestInstallRepairsLostExecuteBit(t *testing.T) {
	repo := newRepo(t)
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	writeHook(t, hook, HookScript, 0o644)

	st, err := Inspect(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Ours || !st.Stale {
		t.Fatalf("non-executable own hook should read as stale: %+v", st)
	}
	if _, _, err := Install(repo); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(hook)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("execute bit not repaired: %v", info.Mode())
	}
}

// TestInstallAppendsToForeignShellHook is the never-clobber contract for a
// hook we CAN extend: their content survives byte-for-byte and our block
// invokes a sidecar script.
func TestInstallAppendsToForeignShellHook(t *testing.T) {
	repo := newRepo(t)
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	foreign := "#!/bin/bash\n# somebody else's hook\nmake lint\n"
	writeHook(t, hook, foreign, 0o755)

	action, st, err := Install(repo)
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionAppended {
		t.Fatalf("action = %q, want %q", action, ActionAppended)
	}
	body := read(t, hook)
	// Their content survives verbatim, in order, with our block inserted
	// directly after the shebang (never at EOF — see appendBlock).
	if !strings.HasPrefix(body, "#!/bin/bash\n") {
		t.Errorf("shebang no longer first:\n%s", body)
	}
	if !strings.HasSuffix(body, "# somebody else's hook\nmake lint\n") {
		t.Errorf("foreign hook content was modified:\n%s", body)
	}
	if !strings.Contains(body, appendBegin) || !strings.Contains(body, ManualLine()) {
		t.Errorf("append block missing:\n%s", body)
	}
	if strings.Index(body, appendBegin) > strings.Index(body, "make lint") {
		t.Errorf("gate block lands after the foreign hook's code:\n%s", body)
	}
	if read(t, st.SidecarPath) != HookScript {
		t.Error("sidecar script not written")
	}

	// Re-inspect: appended, not foreign, and not stale.
	st2, err := Inspect(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !st2.Appended || st2.Foreign || st2.Stale {
		t.Fatalf("expected appended+current, got %+v", st2)
	}
	if again, _, err := Install(repo); err != nil || again != ActionUpToDate {
		t.Fatalf("second install = %q (%v), want %q", again, err, ActionUpToDate)
	}
}

// TestInstallRefreshesStaleSidecar: the foreign hook's block still points at
// the sidecar, so only the sidecar needs rewriting — their file is untouched.
func TestInstallRefreshesStaleSidecar(t *testing.T) {
	repo := newRepo(t)
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	writeHook(t, hook, "#!/bin/sh\nexit 0\n", 0o755)
	if _, st, err := Install(repo); err != nil {
		t.Fatal(err)
	} else {
		writeHook(t, st.SidecarPath, "#!/bin/sh\n"+HookMarker+"\n# old\n", 0o755)
	}
	before := read(t, hook)

	action, st, err := Install(repo)
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionRefreshed {
		t.Fatalf("action = %q, want %q", action, ActionRefreshed)
	}
	if read(t, st.SidecarPath) != HookScript {
		t.Error("sidecar not refreshed")
	}
	if read(t, hook) != before {
		t.Error("foreign hook was rewritten during a sidecar refresh")
	}
}

// TestInstallNeverTouchesNonShellHook: a python (or binary, or symlinked)
// hook gets the manual one-liner, never an edit.
func TestInstallNeverTouchesNonShellHook(t *testing.T) {
	repo := newRepo(t)
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	foreign := "#!/usr/bin/env python3\nprint('hi')\n"
	writeHook(t, hook, foreign, 0o755)

	action, st, err := Install(repo)
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionManual {
		t.Fatalf("action = %q, want %q", action, ActionManual)
	}
	if read(t, hook) != foreign {
		t.Error("a non-appendable foreign hook was modified")
	}
	// The sidecar IS written so the printed one-liner works immediately.
	if read(t, st.SidecarPath) != HookScript {
		t.Error("sidecar not written for the manual case")
	}
}

func TestInstallTreatsSymlinkedHookAsForeign(t *testing.T) {
	repo := newRepo(t)
	target := filepath.Join(t.TempDir(), "their-hook.sh")
	writeHook(t, target, "#!/bin/sh\nexit 0\n", 0o755)
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, hook); err != nil {
		t.Fatal(err)
	}

	st, err := Inspect(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Foreign || st.Appendable {
		t.Fatalf("symlinked hook must be foreign and non-appendable: %+v", st)
	}
	action, _, err := Install(repo)
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionManual {
		t.Fatalf("action = %q, want %q", action, ActionManual)
	}
	info, err := os.Lstat(hook)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("symlink was replaced")
	}
	if read(t, target) != "#!/bin/sh\nexit 0\n" {
		t.Error("symlink target was rewritten")
	}
}

func TestRemoveDeletesOwnHook(t *testing.T) {
	repo := newRepo(t)
	if _, _, err := Install(repo); err != nil {
		t.Fatal(err)
	}
	action, st, err := Remove(repo)
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionRemoved {
		t.Fatalf("action = %q, want %q", action, ActionRemoved)
	}
	if _, err := os.Stat(st.HookPath); !os.IsNotExist(err) {
		t.Errorf("hook still present: %v", err)
	}
}

func TestRemoveStripsAppendedBlockOnly(t *testing.T) {
	repo := newRepo(t)
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	foreign := "#!/bin/sh\nmake lint\n"
	writeHook(t, hook, foreign, 0o755)
	if _, _, err := Install(repo); err != nil {
		t.Fatal(err)
	}

	action, st, err := Remove(repo)
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionRemoved {
		t.Fatalf("action = %q, want %q", action, ActionRemoved)
	}
	if got := read(t, hook); got != foreign {
		t.Errorf("foreign hook not restored byte-for-byte:\n%q\nwant\n%q", got, foreign)
	}
	if _, err := os.Stat(st.SidecarPath); !os.IsNotExist(err) {
		t.Error("sidecar not removed")
	}
}

func TestRemoveLeavesForeignHookAlone(t *testing.T) {
	repo := newRepo(t)
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	foreign := "#!/usr/bin/env python3\nprint('hi')\n"
	writeHook(t, hook, foreign, 0o755)

	action, _, err := Remove(repo)
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionNothing {
		t.Fatalf("action = %q, want %q", action, ActionNothing)
	}
	if read(t, hook) != foreign {
		t.Error("foreign hook was modified by disable")
	}
}

// TestOwnershipIsAnchored is the regression for roast finding R-911598: a
// foreign file that merely mentions our markers must not be treated as ours
// (which would let `gate enable` overwrite it and `gate disable` delete it).
func TestOwnershipIsAnchored(t *testing.T) {
	repo := newRepo(t)
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	// A hook that documents roast rather than being roast.
	foreign := "#!/bin/sh\n# we used to use " + HookMarker + " here\nmake lint\n"
	writeHook(t, hook, foreign, 0o755)

	st, err := Inspect(repo)
	if err != nil {
		t.Fatal(err)
	}
	if st.Ours {
		t.Fatalf("a foreign hook mentioning the marker was claimed as ours: %+v", st)
	}
	if !st.Foreign || !st.Appendable {
		t.Fatalf("expected foreign+appendable, got %+v", st)
	}
	if action, _, err := Remove(repo); err != nil || action != ActionNothing {
		t.Fatalf("remove = %q (%v), want %q", action, err, ActionNothing)
	}
	if read(t, hook) != foreign {
		t.Error("disable modified a foreign hook")
	}
}

// TestMalformedAppendBlockIsNeverStripped: a begin marker with no end (a
// hand-edit or a crash) must leave the file alone rather than deleting
// everything to EOF.
func TestMalformedAppendBlockIsNeverStripped(t *testing.T) {
	repo := newRepo(t)
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	broken := "#!/bin/sh\n" + appendBegin + "\n# end marker deleted by hand\nmake lint\n"
	writeHook(t, hook, broken, 0o755)

	st, err := Inspect(repo)
	if err != nil {
		t.Fatal(err)
	}
	if st.Appended {
		t.Fatalf("a truncated block read as an installed gate: %+v", st)
	}
	if _, _, err := Remove(repo); err != nil {
		t.Fatal(err)
	}
	if read(t, hook) != broken {
		t.Errorf("a malformed block was stripped, taking the file with it:\n%s", read(t, hook))
	}
}

// TestAppendPreservesForeignMode is the regression for roast finding
// R-5dae8f: chmod -x is how people disable a hook, and roast must not undo
// that just because it appended a block.
func TestAppendPreservesForeignMode(t *testing.T) {
	repo := newRepo(t)
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	writeHook(t, hook, "#!/bin/sh\nmake lint\n", 0o644)

	if action, _, err := Install(repo); err != nil || action != ActionAppended {
		t.Fatalf("install = %q (%v)", action, err)
	}
	info, err := os.Stat(hook)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 != 0 {
		t.Errorf("append made a deliberately non-executable hook executable: %v", info.Mode())
	}
	st, err := Inspect(repo)
	if err != nil {
		t.Fatal(err)
	}
	if st.Active() {
		t.Error("a non-executable hook reports as an active gate")
	}
	if !strings.Contains(st.Summary(), "NOT RUNNING") {
		t.Errorf("summary hides the dead gate: %q", st.Summary())
	}
}

// TestSidecarIsNeverClobbered is the regression for roast finding R-a90671:
// never-clobber covers every file roast did not write, not just `pre-commit`.
func TestSidecarIsNeverClobbered(t *testing.T) {
	repo := newRepo(t)
	writeHook(t, filepath.Join(repo, ".git", "hooks", "pre-commit"), "#!/bin/sh\nmake lint\n", 0o755)
	sidecar := filepath.Join(repo, ".git", "hooks", SidecarName)
	theirs := "#!/bin/sh\n# someone else got here first\n"
	writeHook(t, sidecar, theirs, 0o755)

	if _, _, err := Install(repo); err == nil {
		t.Fatal("install overwrote a foreign sidecar without complaint")
	}
	if read(t, sidecar) != theirs {
		t.Error("foreign sidecar was overwritten")
	}
}

// TestRemoveLeavesForeignSidecarAlone is the delete-side mirror of
// TestSidecarIsNeverClobbered (roast finding R-01c2b0): `gate disable` must
// not delete a file at the sidecar path that roast did not write.
func TestRemoveLeavesForeignSidecarAlone(t *testing.T) {
	repo := newRepo(t)
	sidecar := filepath.Join(repo, ".git", "hooks", SidecarName)
	theirs := "#!/bin/sh\n# not roast's\n"
	writeHook(t, sidecar, theirs, 0o755)

	if _, _, err := Remove(repo); err != nil {
		t.Fatal(err)
	}
	if read(t, sidecar) != theirs {
		t.Error("disable deleted a foreign file at the sidecar path")
	}

	// Ours, by contrast, does go.
	writeHook(t, sidecar, HookScript, 0o755)
	if _, _, err := Remove(repo); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
		t.Errorf("roast's own sidecar survived disable: %v", err)
	}
}

// TestAtomicWriteDoesNotFollowAPlantedTempSymlink is the regression for roast
// finding R-306969: with core.hooksPath inside the working tree, a repo could
// plant a symlink at the old predictable temp path and have roast truncate
// whatever it pointed at.
func TestAtomicWriteDoesNotFollowAPlantedTempSymlink(t *testing.T) {
	repo := newRepo(t)
	git(t, repo, "config", "core.hooksPath", ".githooks")
	hooks := filepath.Join(repo, ".githooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "precious")
	writeHook(t, victim, "do not touch\n", 0o644)
	if err := os.Symlink(victim, filepath.Join(hooks, "pre-commit.roast-tmp")); err != nil {
		t.Fatal(err)
	}

	if _, _, err := Install(repo); err != nil {
		t.Fatal(err)
	}
	if got := read(t, victim); got != "do not touch\n" {
		t.Errorf("a planted temp symlink was followed; victim now:\n%s", got)
	}
}

// TestMarkerPairWithoutInvocationIsForeign is the regression for roast
// finding R-742ef8: the markers are public comment text, so a copy-pasted
// pair with no gate invocation between them is not roast's block.
func TestMarkerPairWithoutInvocationIsForeign(t *testing.T) {
	repo := newRepo(t)
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	foreign := "#!/bin/sh\n" + appendBegin + "\n# we deleted roast's line\n" + appendEnd + "\nmake lint\n"
	writeHook(t, hook, foreign, 0o755)

	st, err := Inspect(repo)
	if err != nil {
		t.Fatal(err)
	}
	if st.Appended {
		t.Fatalf("a marker pair with no invocation read as roast's block: %+v", st)
	}
	if action, _, err := Remove(repo); err != nil || action != ActionNothing {
		t.Fatalf("remove = %q (%v), want %q", action, err, ActionNothing)
	}
	if read(t, hook) != foreign {
		t.Error("disable deleted a region of a foreign hook")
	}
}

func TestDecisionRoundTrip(t *testing.T) {
	repo := newRepo(t)
	for _, d := range []Decision{DecisionEnabled, DecisionDeclined} {
		if err := RecordDecision(repo, d); err != nil {
			t.Fatal(err)
		}
		st, err := Inspect(repo)
		if err != nil {
			t.Fatal(err)
		}
		if st.Decision != d {
			t.Fatalf("decision = %q, want %q", st.Decision, d)
		}
	}
	// An unrecognizable recorded value reads as "still unanswered" rather
	// than silently enabling or permanently declining.
	git(t, repo, "config", "--local", "roast.gate", "maybe")
	st, err := Inspect(repo)
	if err != nil {
		t.Fatal(err)
	}
	if st.Decision != DecisionNone {
		t.Errorf("garbage decision = %q, want none", st.Decision)
	}
}

// TestRecordDecisionClearsNotice: the one-time hint belongs to the undecided
// state only. Leaving it behind would silence a legitimate hint if the
// decision were later cleared.
func TestRecordDecisionClearsNotice(t *testing.T) {
	repo := newRepo(t)
	if err := RecordNotice(repo); err != nil {
		t.Fatal(err)
	}
	if st, _ := Inspect(repo); !st.NoticeSent {
		t.Fatal("notice was not recorded")
	}
	if err := RecordDecision(repo, DecisionEnabled); err != nil {
		t.Fatal(err)
	}
	st, err := Inspect(repo)
	if err != nil {
		t.Fatal(err)
	}
	if st.NoticeSent {
		t.Error("recording a decision left the hint marker behind")
	}
}

func TestHonorsCoreHooksPath(t *testing.T) {
	repo := newRepo(t)
	git(t, repo, "config", "core.hooksPath", ".githooks")

	action, st, err := Install(repo)
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionInstalled {
		t.Fatalf("action = %q", action)
	}
	want := filepath.Join(repo, ".githooks", "pre-commit")
	if st.HookPath != want {
		t.Fatalf("HookPath = %q, want %q", st.HookPath, want)
	}
	if read(t, want) != HookScript {
		t.Error("hook not written to the core.hooksPath location")
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "hooks", "pre-commit")); !os.IsNotExist(err) {
		t.Error("hook also landed in .git/hooks despite core.hooksPath")
	}
}

func TestAppendable(t *testing.T) {
	cases := map[string]bool{
		"#!/bin/sh\n":              true,
		"#!/bin/bash -e\n":         true,
		"#!/usr/bin/env bash\n":    true,
		"#!/usr/bin/env zsh\nx\n":  true,
		"#!/usr/bin/env python3\n": false,
		"#!/usr/bin/perl\n":        false,
		"#!/usr/bin/env -S bash\n": false,
		"no shebang\n":             false,
		"":                         false,
		"\x00\x01binary":           false,
	}
	for body, want := range cases {
		if got := appendable(body); got != want {
			t.Errorf("appendable(%q) = %v, want %v", body, got, want)
		}
	}
}

// These tests drive the embedded hook through a REAL `git commit` in a
// throwaway repo, with a stub standing in for the roast binary (ROAST_BIN) so
// nothing ever calls a model. What they prove is the part no unit test can:
// that git's hook environment — above all GIT_INDEX_FILE — reaches the review
// unmodified, and that every failure mode lets the commit through.

// stubScript records exactly what the commit's index looks like from inside
// the hook, then behaves as the test dictates.
const stubScript = `#!/bin/sh
{
  echo "NAMES: $(git -C "$(pwd)" diff --cached --name-only HEAD | tr '\n' ' ')"
  echo "DIFF-START"
  git -C "$(pwd)" diff --cached HEAD
  echo "DIFF-END"
} >> "$STUB_LOG"
if [ -n "${STUB_SLEEP:-}" ]; then sleep "$STUB_SLEEP"; fi
if [ -n "${STUB_STDOUT:-}" ]; then printf '%s\n' "$STUB_STDOUT"; fi
if [ -n "${STUB_STDERR:-}" ]; then printf '%s\n' "$STUB_STDERR" >&2; fi
exit "${STUB_EXIT:-0}"
`

type stub struct {
	bin string
	log string
}

func newStub(t *testing.T) stub {
	t.Helper()
	dir := t.TempDir()
	s := stub{bin: filepath.Join(dir, "roast-stub"), log: filepath.Join(dir, "scope.log")}
	writeHook(t, s.bin, stubScript, 0o755)
	return s
}

// ran reports the stub's recorded observations ("" when it never ran).
func (s stub) ran(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(s.log)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// gitCommit runs a real commit with extra env, returning the combined output
// and whether it succeeded.
func gitCommit(t *testing.T, repo string, extraEnv []string, args ...string) (string, bool) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo, "commit"}, args...)...)
	cmd.Env = append(os.Environ(), extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

func writeFile(t *testing.T, repo, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, rel), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// seededRepo returns a repo with a.txt/b.txt committed, both modified in the
// worktree, and a.txt staged — the dirty shared index the old PreToolUse gate
// could not review correctly.
func seededRepo(t *testing.T) string {
	t.Helper()
	repo := newRepo(t)
	writeFile(t, repo, "a.txt", "a-committed\n")
	writeFile(t, repo, "b.txt", "b-committed\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "seed")

	writeFile(t, repo, "a.txt", "a-STAGED-NOT-COMMITTED\n")
	writeFile(t, repo, "b.txt", "b-IN-THIS-COMMIT\n")
	git(t, repo, "add", "a.txt")
	// These execution tests exercise an explicitly enrolled repository.
	if err := RecordDecision(repo, DecisionEnabled); err != nil {
		t.Fatal(err)
	}
	return repo
}

func stubEnv(s stub, extra ...string) []string {
	return append([]string{
		"ROAST_BIN=" + s.bin,
		"STUB_LOG=" + s.log,
	}, extra...)
}

// TestGateReviewsOnlyThePathScopedCommitsContent is the whole point of the
// native hook. The tree has a staged a.txt that is NOT part of this commit;
// the review must see b.txt's content and nothing else. The retired
// PreToolUse mechanism reviewed the whole shared index here — and reviewed it
// before `git commit <path>` had even staged anything.
func TestGateReviewsOnlyThePathScopedCommitsContent(t *testing.T) {
	repo := seededRepo(t)
	if _, _, err := Install(repo); err != nil {
		t.Fatal(err)
	}
	s := newStub(t)

	out, ok := gitCommit(t, repo, stubEnv(s, `STUB_STDOUT={"findings":[]}`), "b.txt", "-m", "scoped")
	if !ok {
		t.Fatalf("commit failed:\n%s", out)
	}

	seen := s.ran(t)
	if seen == "" {
		t.Fatal("the hook never ran the review")
	}
	names := ""
	for _, line := range strings.Split(seen, "\n") {
		if strings.HasPrefix(line, "NAMES: ") {
			names = strings.TrimSpace(strings.TrimPrefix(line, "NAMES: "))
		}
	}
	if names != "b.txt" {
		t.Errorf("review scope = %q, want exactly \"b.txt\"\n%s", names, seen)
	}
	if !strings.Contains(seen, "b-IN-THIS-COMMIT") {
		t.Errorf("review did not see the committed content:\n%s", seen)
	}
	if strings.Contains(seen, "a-STAGED-NOT-COMMITTED") {
		t.Errorf("review saw staged content that is NOT part of this commit:\n%s", seen)
	}

	// And the commit really was path-scoped: a.txt's staged change is still
	// staged afterwards.
	if staged := git(t, repo, "diff", "--cached", "--name-only"); staged != "a.txt" {
		t.Errorf("post-commit staged set = %q, want a.txt", staged)
	}
}

func TestGateBlocksOnFindingsAndPrintsThemToStderr(t *testing.T) {
	repo := seededRepo(t)
	if _, _, err := Install(repo); err != nil {
		t.Fatal(err)
	}
	s := newStub(t)

	out, ok := gitCommit(t, repo, stubEnv(s,
		`STUB_STDOUT={"findings":[{"severity":"HIGH"}]}`,
		"STUB_STDERR=## R-abc123 HIGH — nil deref in handler",
		"STUB_EXIT=1",
	), "b.txt", "-m", "scoped")

	if ok {
		t.Fatalf("commit succeeded despite a gating finding:\n%s", out)
	}
	if !strings.Contains(out, "R-abc123") {
		t.Errorf("findings markdown missing from the failed commit output:\n%s", out)
	}
	if !strings.Contains(out, "ROAST_SKIP=1") {
		t.Errorf("no bypass instructions in the denial:\n%s", out)
	}
	// Nothing was committed.
	if head := git(t, repo, "log", "--oneline"); strings.Count(head, "\n") != 0 {
		t.Errorf("a commit landed anyway: %q", head)
	}
}

func TestGateSkipBypassesEntirely(t *testing.T) {
	repo := seededRepo(t)
	if _, _, err := Install(repo); err != nil {
		t.Fatal(err)
	}
	s := newStub(t)

	out, ok := gitCommit(t, repo, stubEnv(s, "ROAST_SKIP=1", "STUB_EXIT=1"), "b.txt", "-m", "scoped")
	if !ok {
		t.Fatalf("ROAST_SKIP=1 did not bypass the gate:\n%s", out)
	}
	if seen := s.ran(t); seen != "" {
		t.Errorf("roast ran despite ROAST_SKIP=1:\n%s", seen)
	}
}

// TestGateTreatsEmptyStdoutAsInfrastructureFailure: no JSON result means the
// review never ran. That must never read as clean — but it must also not
// wedge the commit.
func TestGateTreatsEmptyStdoutAsInfrastructureFailure(t *testing.T) {
	for _, exit := range []string{"STUB_EXIT=0", "STUB_EXIT=3"} {
		t.Run(exit, func(t *testing.T) {
			repo := seededRepo(t)
			if _, _, err := Install(repo); err != nil {
				t.Fatal(err)
			}
			s := newStub(t)
			out, ok := gitCommit(t, repo, stubEnv(s, exit), "b.txt", "-m", "scoped")
			if !ok {
				t.Fatalf("commit blocked by an infrastructure failure:\n%s", out)
			}
			if !strings.Contains(out, "WITHOUT review") {
				t.Errorf("empty stdout was not reported as an unreviewed commit:\n%s", out)
			}
		})
	}
}

func TestGateAllowsCommitOnOperationalFailure(t *testing.T) {
	repo := seededRepo(t)
	if _, _, err := Install(repo); err != nil {
		t.Fatal(err)
	}
	s := newStub(t)
	out, ok := gitCommit(t, repo, stubEnv(s,
		`STUB_STDOUT={"findings":[]}`,
		"STUB_STDERR=inference endpoint unreachable",
		"STUB_EXIT=4",
	), "b.txt", "-m", "scoped")
	if !ok {
		t.Fatalf("commit blocked by a partial-failure exit:\n%s", out)
	}
	if !strings.Contains(out, "exit 4") || !strings.Contains(out, "WITHOUT review") {
		t.Errorf("operational failure not surfaced:\n%s", out)
	}
}

func TestGateAllowsCommitWhenRoastIsMissing(t *testing.T) {
	repo := seededRepo(t)
	if _, _, err := Install(repo); err != nil {
		t.Fatal(err)
	}
	out, ok := gitCommit(t, repo, []string{"ROAST_BIN=/nonexistent/roast"}, "b.txt", "-m", "scoped")
	if !ok {
		t.Fatalf("a missing roast binary blocked the commit:\n%s", out)
	}
	if !strings.Contains(out, "not found") {
		t.Errorf("missing binary not reported:\n%s", out)
	}
}

func TestGateTimesOutWithoutBlocking(t *testing.T) {
	if testing.Short() {
		t.Skip("timeout test sleeps")
	}
	repo := seededRepo(t)
	if _, _, err := Install(repo); err != nil {
		t.Fatal(err)
	}
	s := newStub(t)
	out, ok := gitCommit(t, repo, stubEnv(s,
		"STUB_SLEEP=10",
		`STUB_STDOUT={"findings":[]}`,
		"ROAST_GATE_TIMEOUT=1",
	), "b.txt", "-m", "scoped")
	if !ok {
		t.Fatalf("a timed-out review blocked the commit:\n%s", out)
	}
	if !strings.Contains(out, "timed out") {
		t.Errorf("timeout not reported:\n%s", out)
	}
}

// TestGateRunsAlongsideAForeignHook: on a passing review both the gate and
// the foreign hook run; on a blocking one the gate fails the commit. The
// block sits right after the shebang, so a denial short-circuits the foreign
// hook — the documented ordering tradeoff in appendBlock.
func TestGateRunsAlongsideAForeignHook(t *testing.T) {
	repo := seededRepo(t)
	marker := filepath.Join(t.TempDir(), "foreign-ran")
	writeHook(t, filepath.Join(repo, ".git", "hooks", "pre-commit"),
		"#!/bin/sh\ntouch \""+marker+"\"\n", 0o755)
	if action, _, err := Install(repo); err != nil || action != ActionAppended {
		t.Fatalf("install = %q (%v)", action, err)
	}
	s := newStub(t)

	out, ok := gitCommit(t, repo, stubEnv(s,
		`STUB_STDOUT={"findings":[{"severity":"HIGH"}]}`,
		"STUB_STDERR=## R-deadbe HIGH — boom",
		"STUB_EXIT=1",
	), "b.txt", "-m", "scoped")
	if ok {
		t.Fatalf("appended gate did not fail the commit:\n%s", out)
	}
	if !strings.Contains(out, "R-deadbe") {
		t.Errorf("appended gate did not surface findings:\n%s", out)
	}

	// Passing review: the commit lands and the foreign hook ran too.
	out, ok = gitCommit(t, repo, stubEnv(s, `STUB_STDOUT={"findings":[]}`), "b.txt", "-m", "scoped")
	if !ok {
		t.Fatalf("commit failed on a clean review:\n%s", out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the foreign hook did not run: %v", err)
	}
}

// TestGateRunsInAForeignHookThatExitsEarly is the regression for roast
// finding R-5351f8: a hook ending in `exit 0` (a perfectly ordinary shape)
// would never reach a block appended at EOF, so the gate would report itself
// installed and never run once.
func TestGateRunsInAForeignHookThatExitsEarly(t *testing.T) {
	repo := seededRepo(t)
	writeHook(t, filepath.Join(repo, ".git", "hooks", "pre-commit"),
		"#!/bin/sh\nexit 0\n", 0o755)
	if action, _, err := Install(repo); err != nil || action != ActionAppended {
		t.Fatalf("install = %q (%v)", action, err)
	}
	s := newStub(t)

	out, ok := gitCommit(t, repo, stubEnv(s,
		`STUB_STDOUT={"findings":[{"severity":"HIGH"}]}`,
		"STUB_STDERR=## R-c0ffee HIGH — boom",
		"STUB_EXIT=1",
	), "b.txt", "-m", "scoped")
	if ok {
		t.Fatalf("gate never ran in a hook that exits early:\n%s", out)
	}
	if s.ran(t) == "" {
		t.Fatal("the review never ran")
	}
	if !strings.Contains(out, "R-c0ffee") {
		t.Errorf("findings not surfaced:\n%s", out)
	}
}

// TestGateDoesNotRunWhenHookIsNotExecutable: git skips a non-executable hook,
// so Inspect must not call that an active gate (roast finding R-1375c9).
func TestGateDoesNotRunWhenHookIsNotExecutable(t *testing.T) {
	repo := seededRepo(t)
	writeHook(t, filepath.Join(repo, ".git", "hooks", "pre-commit"), "#!/bin/sh\nmake lint\n", 0o755)
	if _, st, err := Install(repo); err != nil {
		t.Fatal(err)
	} else if err := os.Chmod(st.HookPath, 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := Inspect(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Appended {
		t.Fatalf("expected an appended hook: %+v", st)
	}
	if st.Active() {
		t.Error("a non-executable hook reports as an active gate")
	}
	if !strings.Contains(st.Summary(), "NOT RUNNING") {
		t.Errorf("summary hides the dead gate: %q", st.Summary())
	}

	s := newStub(t)
	out, ok := gitCommit(t, repo, stubEnv(s, "STUB_EXIT=1"), "b.txt", "-m", "scoped")
	if !ok {
		t.Fatalf("git ran a non-executable hook?\n%s", out)
	}
	if s.ran(t) != "" {
		t.Error("the review ran from a hook git should have skipped")
	}
}

// Configure an inherited hooksPath to exercise the original cross-repo bug.
func sharedRepos(t *testing.T) (first, second, hooks string) {
	t.Helper()
	first, second = seededRepo(t), seededRepo(t)
	hooks = t.TempDir()
	global := filepath.Join(t.TempDir(), "gitconfig")
	git(t, first, "config", "--file", global, "core.hooksPath", hooks)
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	return first, second, hooks
}

func TestSharedGateRequiresLocalConsent(t *testing.T) {
	for _, decision := range []string{"", "declined", "unexpected"} {
		t.Run("decision="+decision, func(t *testing.T) {
			first, second, _ := sharedRepos(t)
			if _, _, err := Install(first); err != nil {
				t.Fatal(err)
			}
			if err := RecordDecision(second, DecisionNone); err != nil {
				t.Fatal(err)
			}
			if decision != "" {
				git(t, second, "config", "--local", configKeyDecision, decision)
			}
			// A global enrollment value must not authorize a different repo.
			git(t, second, "config", "--global", configKeyDecision, "enabled")
			s := newStub(t)
			out, ok := gitCommit(t, second, stubEnv(s, "STUB_EXIT=1"), "b.txt", "-m", "not enrolled")
			if !ok || s.ran(t) != "" {
				t.Fatalf("unenrolled shared gate ran: success=%v, log=%q, output=%s", ok, s.ran(t), out)
			}
			status, err := Inspect(second)
			if err != nil || !status.Shared || status.Active() {
				t.Fatalf("unenrolled shared status = %#v, %v", status, err)
			}
			if !strings.Contains(status.Summary(), "declined") && !strings.Contains(status.Summary(), "not enrolled") {
				t.Fatalf("summary hides enrollment: %q", status.Summary())
			}
			// The same dispatcher still runs in the repo that opted in.
			out, ok = gitCommit(t, first, stubEnv(s, `STUB_STDOUT={"findings":[]}`), "b.txt", "-m", "enrolled")
			if !ok || s.ran(t) == "" {
				t.Fatalf("enrolled gate failed: success=%v, output=%s", ok, out)
			}
		})
	}
}

func TestGateEnrollmentAliasesMatchDispatcher(t *testing.T) {
	for _, decision := range []string{"enabled", "enable", "true", "yes", "on", "1", "  EnAbLeD  "} {
		t.Run(decision, func(t *testing.T) {
			repo := seededRepo(t)
			git(t, repo, "config", "--local", configKeyDecision, decision)
			if _, _, err := Install(repo); err != nil {
				t.Fatal(err)
			}
			status, err := Inspect(repo)
			if err != nil || !status.Active() {
				t.Fatalf("alias status = %#v, %v", status, err)
			}
			s := newStub(t)
			out, ok := gitCommit(t, repo, stubEnv(s, `STUB_STDOUT={"findings":[]}`), "b.txt", "-m", "alias")
			if !ok || s.ran(t) == "" {
				t.Fatalf("alias gate failed: success=%v, output=%s", ok, out)
			}
		})
	}
}

func TestBootstrapDoesNotAdoptInheritedGate(t *testing.T) {
	for _, interactive := range []bool{false, true} {
		t.Run(map[bool]string{false: "noninteractive", true: "interactive"}[interactive], func(t *testing.T) {
			first, second, hooks := sharedRepos(t)
			if err := RecordDecision(second, DecisionNone); err != nil {
				t.Fatal(err)
			}
			writeHook(t, filepath.Join(hooks, HookName), "#!/bin/sh\n"+HookMarker+"\n# legacy dispatcher\nexit 0\n", 0o755)
			var prompt func(string) (bool, error)
			asked := false
			if interactive {
				prompt = func(string) (bool, error) { asked = true; return false, nil }
			}
			outcome, _ := bootstrapForTest(t, second, prompt)
			if interactive && (!asked || outcome != OutcomeDeclined) {
				t.Fatalf("inherited gate suppressed consent: asked=%v, outcome=%s", asked, outcome)
			}
			if !interactive && outcome != OutcomeHinted {
				t.Fatalf("noninteractive bootstrap = %s", outcome)
			}
			status, err := Inspect(second)
			if err != nil || status.Decision == DecisionEnabled || status.Active() || status.Stale {
				t.Fatalf("inherited gate status = %#v, %v", status, err)
			}
			// Refreshing the owned dispatcher did not revoke A's enrollment.
			status, err = Inspect(first)
			if err != nil || !status.Active() || status.Stale {
				t.Fatalf("original repo status = %#v, %v", status, err)
			}
		})
	}
}

func TestBootstrapRefreshesDeclinedLegacyDispatcher(t *testing.T) {
	_, second, hooks := sharedRepos(t)
	if err := RecordDecision(second, DecisionDeclined); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(hooks, HookName)
	writeHook(t, path, "#!/bin/sh\n"+HookMarker+"\n# old shared script\nexit 0\n", 0o755)
	outcome, output := bootstrapForTest(t, second, func(string) (bool, error) {
		t.Fatal("declined repository was prompted")
		return false, nil
	})
	if outcome != OutcomeQuiet || output != "" || read(t, path) != HookScript {
		t.Fatalf("declined refresh = %s, %q", outcome, output)
	}
	if status, err := Inspect(second); err != nil || status.Decision != DecisionDeclined || status.Active() {
		t.Fatalf("declined status = %#v, %v", status, err)
	}
}

func TestSharedGateDisablePreservesOtherRepository(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(map[bool]string{false: "standalone", true: "appended"}[foreign], func(t *testing.T) {
			first, second, hooks := sharedRepos(t)
			path := filepath.Join(hooks, HookName)
			if foreign {
				writeHook(t, path, "#!/bin/sh\n# foreign hook\nexit 0\n", 0o755)
			}
			if _, _, err := Install(first); err != nil {
				t.Fatal(err)
			}
			before := read(t, path)
			action, _, err := Remove(second)
			if err != nil || action != ActionPreserved || read(t, path) != before {
				t.Fatalf("shared removal = %s, %v", action, err)
			}
			if err := RecordDecision(second, DecisionDeclined); err != nil {
				t.Fatal(err)
			}
			if foreign {
				if _, err := os.Stat(filepath.Join(hooks, SidecarName)); err != nil {
					t.Fatal("shared sidecar was removed:", err)
				}
			}
			s := newStub(t)
			out, ok := gitCommit(t, second, stubEnv(s, "STUB_EXIT=1"), "b.txt", "-m", "disabled")
			if !ok || s.ran(t) != "" {
				t.Fatalf("disabled shared gate ran: success=%v, output=%s", ok, out)
			}
			out, ok = gitCommit(t, first, stubEnv(s, `STUB_STDOUT={"findings":[]}`), "b.txt", "-m", "still enabled")
			if !ok || s.ran(t) == "" {
				t.Fatalf("other repo lost its gate: success=%v, output=%s", ok, out)
			}
		})
	}
}

func TestSharedGateDisableRefreshesLegacyDispatchers(t *testing.T) {
	for _, manual := range []bool{false, true} {
		t.Run(map[bool]string{false: "standalone", true: "manual sidecar"}[manual], func(t *testing.T) {
			_, repo, hooks := sharedRepos(t)
			path := filepath.Join(hooks, HookName)
			foreign := "#!/usr/bin/env python3\n# manual hook\n"
			if manual {
				writeHook(t, path, foreign, 0o755)
				path = filepath.Join(hooks, SidecarName)
			}
			writeHook(t, path, "#!/bin/sh\n"+HookMarker+"\n# legacy\nexit 0\n", 0o755)
			action, _, err := Remove(repo)
			if err != nil || action != ActionPreserved || read(t, path) != HookScript {
				t.Fatalf("legacy shared removal = %s, %v", action, err)
			}
			if manual && read(t, filepath.Join(hooks, HookName)) != foreign {
				t.Fatal("manual foreign hook was changed")
			}
		})
	}
}

func TestSharedGateDisableNeverTouchesForeignSidecar(t *testing.T) {
	_, repo, hooks := sharedRepos(t)
	path := filepath.Join(hooks, SidecarName)
	const foreign = "#!/bin/sh\n# not Roast-owned\nexit 0\n"
	writeHook(t, path, foreign, 0o755)
	if _, _, err := Remove(repo); err != nil {
		t.Fatal(err)
	}
	if read(t, path) != foreign {
		t.Fatal("foreign sidecar was modified")
	}
}
