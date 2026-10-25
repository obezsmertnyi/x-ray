package engine

// Update must never read a file outside the repo it serves. The relative branch
// of locate once kept a path's ".." after cleaning, so "../outside/private.md"
// was read, indexed and handed back by get_graph_node to any client that could
// reach update_code_graph or POST /update. Untagged, with Markdown fixtures, so
// both builds prove it: the static build parses Markdown without Tree-sitter.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/parser"
)

// escapeFixture lays out <tmp>/repo (the served root, with one in-repo note)
// beside <tmp>/outside/private.md, and returns an Indexer serving the repo.
func escapeFixture(t *testing.T) (ixr *Indexer, repo, outside string) {
	t.Helper()
	base := t.TempDir()
	repo = filepath.Join(base, "repo")
	outside = filepath.Join(base, "outside")
	for dir, file := range map[string]string{repo: "inside.md", outside: "private.md"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "# " + strings.TrimSuffix(file, ".md") + "\n\n## Secret\n\nmarker-" + file + "\n"
		if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ixr, err := NewIndexerFromResults(context.Background(), "r", repo, map[string]parser.Result{}, &stubEmbedder{dims: 16}, testLogger())
	if err != nil {
		t.Fatalf("NewIndexerFromResults: %v", err)
	}
	return ixr, repo, outside
}

// graphMentions reports whether any node's QN or content carries marker.
func graphMentions(ixr *Indexer, marker string) bool {
	for _, n := range ixr.Graph().Nodes() {
		if strings.Contains(n.QN, marker) || strings.Contains(n.Content, marker) {
			return true
		}
	}
	return false
}

func TestUpdateRefusesPathsOutsideTheRepo(t *testing.T) {
	ixr, repo, outside := escapeFixture(t)
	if err := os.Symlink(outside, filepath.Join(repo, "linked")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	escapes := []string{
		"../outside/private.md",              // relative, straight out
		"sub/../../outside/private.md",       // relative, out after cleaning
		filepath.Join(outside, "private.md"), // absolute, outside every root
		repo + "/../outside/private.md",      // absolute, out after cleaning
		"linked/private.md",                  // through a symlinked directory the walk skips
		filepath.Join(repo, "linked", "private.md"),
	}
	changed, _, _, unowned, err := ixr.Update(context.Background(), escapes)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if changed != 0 || len(unowned) != len(escapes) {
		t.Fatalf("changed=%d unowned=%v; want 0 changed and all %d paths unowned", changed, unowned, len(escapes))
	}
	if graphMentions(ixr, "private") {
		t.Fatal("a file outside the repo was indexed")
	}

	// The same call still indexes a file that is inside.
	changed, _, _, unowned, err = ixr.Update(context.Background(), []string{"inside.md"})
	if err != nil || changed != 1 || len(unowned) != 0 {
		t.Fatalf("inside.md: changed=%d unowned=%v err=%v; want 1 changed", changed, unowned, err)
	}
	if !graphMentions(ixr, "marker-inside.md") {
		t.Fatal("inside.md was not indexed")
	}
}

// followSymlinks is the operator's opt-in to index through symlinked
// directories; an update then reaches what the full walk reaches.
func TestUpdateFollowsSymlinkedDirWhenOptedIn(t *testing.T) {
	ixr, repo, outside := escapeFixture(t)
	if err := os.Symlink(outside, filepath.Join(repo, "linked")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	ixr.followSymlinks = true
	changed, _, _, unowned, err := ixr.Update(context.Background(), []string{"linked/private.md"})
	if err != nil || changed != 1 || len(unowned) != 0 {
		t.Fatalf("changed=%d unowned=%v err=%v; want 1 changed", changed, unowned, err)
	}
}
