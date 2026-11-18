package context

import (
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bluesky-social/roast/internal/snapshot"
)

func TestQuoteGrounder(t *testing.T) {
	// A repository, because the live corpus is Git's view of the working tree.
	dir := initRepo(t)
	src := "func Foo() {\n\treturn\tbar(1,\n\t\t2)\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	diff := "diff --git a/b.go b/b.go\n--- a/b.go\n+++ b/b.go\n@@ -1 +0,0 @@\n-deletedLine := old()\n" +
		"diff --git a/c.go b/c.go\n--- a/c.go\n+++ b/c.go\n@@ -0,0 +1 @@\n+addedLine := new()\n"
	g := NewQuoteGrounder(dir, Tree{}, diff, "")

	t.Run("exact quote grounds", func(t *testing.T) {
		if !g("a.go", "func Foo() {") {
			t.Error("byte-exact quote should ground")
		}
	})

	t.Run("whitespace variant grounds", func(t *testing.T) {
		// Quote uses single spaces where the file has tabs and a newline —
		// the 3/103 case byte-exact matching would wrongly drop.
		if !g("a.go", "return bar(1, 2)") {
			t.Error("whitespace-normalized quote should ground")
		}
	})

	t.Run("deleted code grounds via its file's diff section", func(t *testing.T) {
		if !g("b.go", "deletedLine := old()") {
			t.Error("quote of a deleted line should ground against its file's diff")
		}
	})

	t.Run("diff content does not ground a different file", func(t *testing.T) {
		if g("a.go", "deletedLine := old()") {
			t.Error("b.go's diff content must not ground a finding cited to a.go")
		}
		if g("missing.go", "addedLine := new()") {
			t.Error("c.go's diff content must not ground a fabricated path")
		}
	})

	t.Run("hybrid old+new quote does not ground", func(t *testing.T) {
		// "deletedLine := old()" was replaced by nothing here, but construct
		// a replacement hunk to pin the side separation.
		d := "diff --git a/d.go b/d.go\n--- a/d.go\n+++ b/d.go\n@@ -1 +1 @@\n-oldCall()\n+newCall()\n"
		gd := NewQuoteGrounder(dir, Tree{}, d, "")
		if gd("d.go", "oldCall() newCall()") {
			t.Error("a quote spanning the old and new side existed in neither version and must not ground")
		}
		if !gd("d.go", "oldCall()") || !gd("d.go", "newCall()") {
			t.Error("each side must ground on its own")
		}
	})

	t.Run("old-side multiline quote grounds across an interleaved change", func(t *testing.T) {
		d := "diff --git a/e.go b/e.go\n--- a/e.go\n+++ b/e.go\n@@ -1,3 +1,3 @@\n ctx1()\n-oldA()\n+newA()\n ctx2()\n"
		gd := NewQuoteGrounder(dir, Tree{}, d, "")
		if !gd("e.go", "oldA()\nctx2()") {
			t.Error("old-side quote spanning an interleaved +line must ground")
		}
		if !gd("e.go", "ctx1()\nnewA()\nctx2()") {
			t.Error("new-side quote with context must ground")
		}
	})

	t.Run("confabulated quote does not ground", func(t *testing.T) {
		if g("a.go", "func Bar() { panic(\"nope\") }") {
			t.Error("quote absent from file and diff must not ground")
		}
	})

	t.Run("empty quote does not ground", func(t *testing.T) {
		if g("a.go", "  \t\n ") {
			t.Error("whitespace-only quote must not ground")
		}
	})

	t.Run("NUL in quote does not bridge sentinels", func(t *testing.T) {
		d := "diff --git a/d.go b/d.go\n--- a/d.go\n+++ b/d.go\n@@ -1 +1 @@\n-oldCall()\n+newCall()\n"
		gd := NewQuoteGrounder(dir, Tree{}, d, "")
		// The corpus holds "oldCall()" and "newCall()" joined by " \x00 ";
		// a quote carrying its own NUL must not be allowed to align with it.
		if gd("d.go", "oldCall() \x00 newCall()") {
			t.Error("quote containing NUL must be rejected, not matched across a sentinel")
		}
	})

	t.Run("header-shaped hunk content does not steal the section key", func(t *testing.T) {
		// A deleted line whose content is `-- a/other.go` renders in the
		// hunk as `--- a/other.go`; it must not key real.go's section to
		// other.go.
		d := "diff --git a/real.go b/real.go\n--- a/real.go\n+++ b/real.go\n" +
			"@@ -1,2 +1 @@\n--- a/other.go\n realContent := 1\n"
		gd := NewQuoteGrounder(dir, Tree{}, d, "")
		if gd("other.go", "realContent := 1") {
			t.Error("hunk content must not be attributed to a path parsed from inside the hunk")
		}
		if !gd("real.go", "realContent := 1") {
			t.Error("section must stay keyed to its real header path")
		}
		if !gd("real.go", "-- a/other.go") {
			t.Error("the deleted line itself is real old-side content of real.go")
		}
	})

	t.Run("git-quoted path keys its section", func(t *testing.T) {
		d := "diff --git \"a/f\\303\\266.go\" \"b/f\\303\\266.go\"\n" +
			"--- \"a/f\\303\\266.go\"\n+++ \"b/f\\303\\266.go\"\n@@ -1 +0,0 @@\n-umlautGone := 1\n"
		gd := NewQuoteGrounder(dir, Tree{}, d, "")
		if !gd("fö.go", "umlautGone := 1") {
			t.Error("quote from a core.quotePath-quoted section should ground for its decoded path")
		}
		if gd("other.go", "umlautGone := 1") {
			t.Error("quoted-path section content must not ground an unrelated file")
		}
	})
}

// TestQuoteGrounderStagedCorpusIsTheIndex: the grounder's corpus must be
// exactly the set of bytes the finder could have seen — no wider, no narrower.
// Wider fails open on confabulation; narrower drops genuine findings. Once
// read_file and grep_repo honor `staged`, nothing in staged scope serves
// worktree content, so admitting the worktree would be a hole.
func TestQuoteGrounderStagedCorpusIsTheIndex(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "f.go", "package p\n// committed\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "initial")

	writeFile(t, repo, "f.go", "package p\nvar stagedOnlySymbol = 1\n")
	gitAdd(t, repo, "f.go")
	writeFile(t, repo, "f.go", "package p\nvar unstagedOnlySymbol = 2\n")

	tree, err := PinIndexTree(repo)
	if err != nil {
		t.Fatalf("PinIndexTree: %v", err)
	}
	g := NewQuoteGrounder(repo, tree, "", "")
	if !g("f.go", "var stagedOnlySymbol = 1") {
		t.Error("a quote of staged content must ground — it is what the reviewer was shown")
	}
	if g("f.go", "var unstagedOnlySymbol = 2") {
		t.Error("a quote of unstaged-only content must not ground: no staged-scope " +
			"context source serves the worktree, so the finder could not have seen it")
	}

	// Non-staged scope is unchanged: the worktree is the corpus.
	wt := NewQuoteGrounder(repo, Tree{}, "", "")
	if !wt("f.go", "var unstagedOnlySymbol = 2") {
		t.Error("non-staged grounding should read the worktree")
	}
}

// TestQuoteGrounderStagedAdmitsTheDiff: the staged corpus is the index blob
// PLUS the file's diff section, which is where a quote of deleted code lives —
// narrowing to the index alone would drop those genuine findings.
func TestQuoteGrounderStagedAdmitsTheDiff(t *testing.T) {
	repo := initRepo(t)
	diff := "diff --git a/f.go b/f.go\n" +
		"--- a/f.go\n+++ b/f.go\n" +
		"@@ -1,2 +1,2 @@\n-removedSymbol := 1\n+addedSymbol := 2\n"

	tree, err := PinIndexTree(repo)
	if err != nil {
		t.Fatalf("PinIndexTree: %v", err)
	}
	g := NewQuoteGrounder(repo, tree, diff, "")
	if !g("f.go", "removedSymbol := 1") {
		t.Error("a quote of code the staged change deletes must still ground via the diff")
	}
}

func TestQuoteGrounderRangeSpec(t *testing.T) {
	// Branch-survey shape: a file deleted between base and HEAD exists in no
	// local file and no scope diff (survey sc.Diff is empty), but git_diff
	// exposes it — the range corpus must ground it.
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "gone.go"), []byte("survivorBias := true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "gone.go")
	run("commit", "-q", "-m", "add")
	run("rm", "-q", "gone.go")
	run("commit", "-q", "-m", "delete")

	g := NewQuoteGrounder(repo, Tree{}, "", "HEAD~1..HEAD")
	if !g("gone.go", "survivorBias := true") {
		t.Error("deleted-on-branch quote should ground via the range corpus")
	}
	if g("other.go", "survivorBias := true") {
		t.Error("range corpus must be scoped to the cited file")
	}
	if g("gone.go", "neverExisted := 1") {
		t.Error("quote absent from the range delta must not ground")
	}
	if g(".", "survivorBias := true") {
		t.Error("a directory-cited finding must not ground via pathspec expansion")
	}
}

// TestNormalizeWSMatchesFields pins the allocation-light normalizer to the
// strings.Fields definition it replaced, including non-ASCII whitespace and
// invalid UTF-8, and the streaming form to the whole-string form.
func TestNormalizeWSMatchesFields(t *testing.T) {
	alphabet := []string{"a", "é", " ", "\t", "\n", "\r", "\v", "\f", " ", "\u0085", " ", "　", "\xff", "\xe2\x80", "x", "\x00"}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 2000; i++ {
		var b strings.Builder
		for j := rng.Intn(40); j > 0; j-- {
			b.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		s := b.String()
		want := strings.Join(strings.Fields(s), " ")
		if got := normalizeWS(s); got != want {
			t.Fatalf("normalizeWS(%q) = %q, want %q", s, got, want)
		}
		// Stream the same input in random chunks, splitting multi-byte runes.
		var n wsNormalizer
		var out strings.Builder
		for rest := s; ; {
			k := min(len(rest), rng.Intn(4))
			n.write(&out, rest[:k], false)
			rest = rest[k:]
			if rest == "" {
				n.write(&out, "", true)
				break
			}
		}
		if out.String() != want {
			t.Fatalf("streamed normalize(%q) = %q, want %q", s, out.String(), want)
		}
	}
}

// referenceDiffSides is normalizedDiffSides as it was written before it
// streamed: raw sides first, then strings.Fields.
func referenceDiffSides(s string) string {
	var oldSide, newSide strings.Builder
	sentinel := func(b *strings.Builder) {
		if b.Len() > 0 {
			b.WriteString("\n\x00\n")
		}
	}
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	inHunk := false
	for l := range strings.SplitSeq(s, "\n") {
		switch {
		case strings.HasPrefix(l, "@@"):
			sentinel(&oldSide)
			sentinel(&newSide)
			inHunk = true
			continue
		case strings.HasPrefix(l, "diff --git "):
			inHunk = false
			continue
		}
		if !inHunk || l == "" {
			continue
		}
		switch l[0] {
		case ' ':
			oldSide.WriteString(l[1:] + "\n")
			newSide.WriteString(l[1:] + "\n")
		case '-':
			oldSide.WriteString(l[1:] + "\n")
		case '+':
			newSide.WriteString(l[1:] + "\n")
		}
	}
	o, n := norm(oldSide.String()), norm(newSide.String())
	switch {
	case o == "":
		return n
	case n == "":
		return o
	default:
		return o + " \x00 " + n
	}
}

func TestNormalizedDiffSidesMatchesReference(t *testing.T) {
	lines := []string{"@@ -1 +1 @@", "diff --git a/x b/x", " ctx  line", "-old\tline", "+new line", "+", "-", " ", "+\xe2", "-\x80 tail", "\\ No newline at end of file", "", "+   ", "--- a/x", "+++ b/x"}
	rng := rand.New(rand.NewSource(2))
	for i := 0; i < 3000; i++ {
		var parts []string
		for j := rng.Intn(12); j > 0; j-- {
			parts = append(parts, lines[rng.Intn(len(lines))])
		}
		s := strings.Join(parts, "\n")
		if got, want := normalizedDiffSides(s), referenceDiffSides(s); got != want {
			t.Fatalf("normalizedDiffSides(%q) = %q, want %q", s, got, want)
		}
	}
}

// TestGrounderBoundsHugeFiles: a quote from deep inside a file far larger
// than the grounding cache still grounds (read_file can page to it), and
// grounding a huge diff does not cost a multiple of its size in word slices.
func TestGrounderBoundsHugeFiles(t *testing.T) {
	repo := initRepo(t)
	body := strings.Repeat("filler words here\n", (groundingCacheBytes/18)*2) + "func deepNeedle() {}\n"
	writeFile(t, repo, "huge.txt", body)
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "huge")
	pin, err := snapshot.PinWorktree(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tree := range []Tree{{}, PinnedWorktree(pin.Tree)} {
		g := NewQuoteGrounder(repo, tree, "", "")
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		ok := g("huge.txt", "func   deepNeedle()\n{}")
		runtime.ReadMemStats(&after)
		if !ok {
			t.Errorf("pinned=%v: a quote at the end of a huge file must ground", tree.Pinned())
		}
		if g("huge.txt", "func absentNeedle() {}") {
			t.Errorf("pinned=%v: an absent quote grounded", tree.Pinned())
		}
		// Streaming converts each 64 KiB read once (about 1x the file in
		// short-lived chunks); reading the file whole and normalizing it
		// costs several times its size, and held all of it at once.
		if alloc := after.TotalAlloc - before.TotalAlloc; alloc > uint64(len(body))*3/2 {
			t.Errorf("pinned=%v: grounding allocated %d bytes for a %d-byte file", tree.Pinned(), alloc, len(body))
		}
	}

	var diff strings.Builder
	diff.WriteString("diff --git a/data.txt b/data.txt\nnew file mode 100644\n--- /dev/null\n+++ b/data.txt\n@@ -0,0 +1,1000000 @@\n")
	for i := 0; i < 1_000_000; i++ {
		diff.WriteString("+a b c d e f g h\n")
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	g := NewQuoteGrounder(repo, Tree{}, diff.String(), "")
	runtime.ReadMemStats(&after)
	// strings.Fields allocated a 16-byte header per word: 8 words per
	// 17-byte line made it about 8x the diff before the join, on top of the
	// raw copies. Normalizing into presized builders costs about 2x.
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 3*uint64(diff.Len()) {
		t.Errorf("grounder construction allocated %d bytes for a %d-byte diff", alloc, diff.Len())
	}
	if !g("data.txt", "h a b c") {
		t.Error("diff content must still ground")
	}
}
