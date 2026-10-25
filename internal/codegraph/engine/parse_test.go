//go:build cgo

package engine

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// walkResult runs FilepathWalk and collects every reported rel path.
func walkResult(t *testing.T, root string, followSymlinks bool) []string {
	t.Helper()
	var got []string
	if err := FilepathWalk(root, followSymlinks, func(rel string, _ []byte) {
		got = append(got, filepath.ToSlash(rel))
	}); err != nil {
		t.Fatalf("FilepathWalk: %v", err)
	}
	sort.Strings(got)
	return got
}

// TestFilepathWalkSymlinkedDirSkippedByDefault proves the opt-in default:
// with followSymlinks=false a symlinked directory is invisible to the walk,
// exactly like before this parameter existed (files=4, not files=11, on the
// real ~/.claude/skills tree that motivated this change).
func TestFilepathWalkSymlinkedDirSkippedByDefault(t *testing.T) {
	root := t.TempDir()
	real := t.TempDir()
	mustWrite(t, filepath.Join(real, "SKILL.md"), "# real skill\n")
	mustWrite(t, filepath.Join(root, "direct.txt"), "direct")
	if err := os.Symlink(real, filepath.Join(root, "linked")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	got := walkResult(t, root, false)
	want := []string{"direct.txt"}
	if !equalStrings(got, want) {
		t.Fatalf("followSymlinks=false: got %v, want %v", got, want)
	}
}

// TestFilepathWalkFollowSymlinksDescends proves the opt-in behaviour: with
// followSymlinks=true the file behind a symlinked directory is found, and
// reported under the path AS REACHED (through the symlink's own name), not
// the resolved real path — so a caller that later hands that path back (e.g.
// update_code_graph on "skills/linked/SKILL.md") still routes correctly.
func TestFilepathWalkFollowSymlinksDescends(t *testing.T) {
	root := t.TempDir()
	real := t.TempDir()
	mustWrite(t, filepath.Join(real, "SKILL.md"), "# real skill\n")

	if err := os.Symlink(real, filepath.Join(root, "linked")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	got := walkResult(t, root, true)
	want := []string{"linked/SKILL.md"}
	if !equalStrings(got, want) {
		t.Fatalf("followSymlinks=true: got %v, want %v", got, want)
	}
}

// TestFilepathWalkFollowSymlinksCycleProtection guards against a symlink that
// points back at one of its own ancestors, directly or through a chain — an
// unguarded walk would recurse forever. It must terminate promptly and must
// not descend into the same real directory more than once.
func TestFilepathWalkFollowSymlinksCycleProtection(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "child")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	mustWrite(t, filepath.Join(child, "file.txt"), "hi")
	// loop points at root, an ancestor of the directory that contains it.
	if err := os.Symlink(root, filepath.Join(child, "loop")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	type result struct {
		got []string
		err error
	}
	done := make(chan result, 1)
	go func() {
		var got []string
		err := FilepathWalk(root, true, func(rel string, _ []byte) {
			got = append(got, filepath.ToSlash(rel))
		})
		sort.Strings(got)
		done <- result{got, err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("FilepathWalk: %v", r.err)
		}
		// child/file.txt directly, plus one pass through the loop back to root
		// (child/loop/child/file.txt) before the cycle guard stops the second
		// re-entry into root's real path — never an unbounded/duplicated walk.
		want := []string{"child/file.txt", "child/loop/child/file.txt"}
		if !equalStrings(r.got, want) {
			t.Fatalf("got %v, want %v", r.got, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("FilepathWalk did not terminate — symlink cycle not guarded")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestFilepathWalkMissingRootIsAnError: a wrong -repo path used to walk zero
// files and report success, so `xray-memory snapshot` wrote an empty graph over a
// real one. The root must exist and be a directory.
func TestFilepathWalkMissingRootIsAnError(t *testing.T) {
	if err := FilepathWalk(filepath.Join(t.TempDir(), "does-not-exist"), false, func(string, []byte) {}); err == nil {
		t.Fatal("a missing repo root must be an error, got nil")
	}
	file := filepath.Join(t.TempDir(), "a.go")
	mustWrite(t, file, "package a\n")
	if err := FilepathWalk(file, false, func(string, []byte) {}); err == nil {
		t.Fatal("a file as repo root must be an error, got nil")
	}
}

// TestNewIndexerRefusesEmptyRepo: a directory with nothing parseable is not a
// repo to index — building it would embed, write and serve an empty graph.
func TestNewIndexerRefusesEmptyRepo(t *testing.T) {
	empty := t.TempDir()
	mustWrite(t, filepath.Join(empty, "notes.bin"), "\x00\x01")
	_, err := NewIndexer(context.Background(), []string{"empty=" + empty}, &stubEmbedder{}, testLogger(), false)
	if err == nil || !strings.Contains(err.Error(), "no parseable files") {
		t.Fatalf("expected a no-parseable-files error, got %v", err)
	}
	_, err = NewIndexer(context.Background(), []string{"gone=" + filepath.Join(empty, "missing")}, &stubEmbedder{}, testLogger(), false)
	if err == nil || !strings.Contains(err.Error(), "repo root") {
		t.Fatalf("expected a repo-root error for a missing path, got %v", err)
	}
}

// TestSkipDirVendoringNames: third_party is vendor under its other conventional
// name. Left in, a repo's vendored tree can outweigh the repo itself — one
// application controller parsed to 70,508 nodes, 68,801 of them under
// third_party/, against ~1,700 of its own.
func TestSkipDirVendoringNames(t *testing.T) {
	for _, name := range []string{"vendor", "third_party", "node_modules", ".git"} {
		if !skipDir(name) {
			t.Errorf("%s should be skipped", name)
		}
	}
	for _, name := range []string{"internal", "cmd", "docs", "third"} {
		if skipDir(name) {
			t.Errorf("%s must not be skipped", name)
		}
	}
}

// TestFilepathWalkSkipsMinifiedBundles proves the walk drops a vendored bundle
// that skipDir cannot reach. skipDir only excludes whole directories by name
// (vendor/, node_modules/, dist/), so a d3.v7.min.js committed into an ordinary
// assets/ directory used to be parsed in full — ~2000 single-letter "functions"
// that outnumbered the repo's real symbols and were embedded into every
// snapshot. Real sources beside it must still be walked.
func TestFilepathWalkSkipsMinifiedBundles(t *testing.T) {
	root := t.TempDir()
	assets := filepath.Join(root, "internal", "ui", "assets")
	if err := os.MkdirAll(assets, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// Named as a bundle, tiny body: the name alone must be enough.
	mustWrite(t, filepath.Join(assets, "d3.v7.min.js"), "var a=1;\n")
	// Named innocuously, but 200 KB on 2 lines: content must be enough.
	mustWrite(t, filepath.Join(assets, "main.a1b2c3.js"), strings.Repeat("x", 100<<10)+"\n"+strings.Repeat("y", 100<<10))
	// Ordinary sources that must survive the walk.
	mustWrite(t, filepath.Join(root, "main.go"), "package main\n")
	mustWrite(t, filepath.Join(assets, "index.html"), "<html></html>\n")

	got := walkResult(t, root, false)
	want := []string{"internal/ui/assets/index.html", "main.go"}
	if !equalStrings(got, want) {
		t.Fatalf("walk got %v, want %v", got, want)
	}
}

// countingEmbedder counts the node texts it was asked to embed.
type countingEmbedder struct {
	stubEmbedder
	embedded int
}

func (c *countingEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	c.embedded += len(texts)
	return c.stubEmbedder.EmbedBatch(ctx, texts)
}

// A rebuild seeded with the previous build's cache embeds only what changed,
// still parses everything, and does not carry the changed node's old vector
// into what it would write.
func TestNewIndexerSeeded_EmbedsOnlyNewTextAndPrunes(t *testing.T) {
	repo := t.TempDir()
	mustWrite(t, filepath.Join(repo, "a.go"), "package a\n\nfunc A() int { return 1 }\n\nfunc B() int { return 2 }\n")
	ctx := context.Background()

	cold := &countingEmbedder{stubEmbedder: stubEmbedder{dims: 8}}
	first, err := NewIndexer(ctx, []string{"r=" + repo}, cold, testLogger(), false)
	if err != nil {
		t.Fatal(err)
	}
	seed := first.Index().CacheSnapshot()
	if cold.embedded == 0 || len(seed) != cold.embedded {
		t.Fatalf("cold build embedded %d, cached %d", cold.embedded, len(seed))
	}

	mustWrite(t, filepath.Join(repo, "a.go"), "package a\n\nfunc A() int { return 1 }\n\nfunc B() int { return 3 }\n")
	warm := &countingEmbedder{stubEmbedder: stubEmbedder{dims: 8}}
	second, err := NewIndexerSeeded(ctx, []string{"r=" + repo}, warm, testLogger(), false, seed, false)
	if err != nil {
		t.Fatal(err)
	}
	if warm.embedded != 1 {
		t.Fatalf("seeded rebuild embedded %d texts, want only B's changed one", warm.embedded)
	}
	if st := second.Graph().Stats(); st.Embedded != st.Nodes {
		t.Fatalf("embedded %d of %d nodes", st.Embedded, st.Nodes)
	}
	if got := len(second.Index().CacheSnapshot()); got != len(seed) {
		t.Fatalf("cache holds %d vectors, want %d: B's old text must be pruned", got, len(seed))
	}
}
