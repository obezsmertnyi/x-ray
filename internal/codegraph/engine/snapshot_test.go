package engine

// Unit coverage for the semantic-router summary fix (measured 8/25 top-5
// routing accuracy across 5 served maps before this, docs/todo/corpus-map-router):
// summaryFor's precedence (explicit -summary flag > README > stats) and
// statsSummary's docs-only fallback (Doc titles instead of an empty Symbols:
// list). Pure Go — docs.Parse and graph.Build need no Tree-sitter, so this file
// carries no `cgo` build tag, unlike registry_test.go.

import (
	"strings"
	"testing"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/docs"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/graph"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/parser"
)

// buildDocsGraph parses each path->markdown pair with docs.Parse and assembles
// the resulting Doc/Section nodes+edges into one graph — the same pipeline a
// real docs-only map (e.g. ccmem, skills, commerce) goes through.
func buildDocsGraph(repo string, files map[string]string) *graph.Graph {
	rg := graph.RepoGraph{Repo: repo}
	for path, content := range files {
		res := docs.Parse(path, []byte(content))
		rg.Nodes = append(rg.Nodes, res.Nodes...)
		rg.Edges = append(rg.Edges, res.Edges...)
	}
	g := graph.New()
	g.Build(rg)
	return g
}

func TestSummaryForPrecedence(t *testing.T) {
	readmeMD := "# Widget\n\n## Overview\n\nWidget does widget things across the fleet.\n"
	g := buildDocsGraph("widget", map[string]string{"README.md": readmeMD})

	// Explicit wins outright, even with a README present.
	summary, source := summaryFor(g, "widget", "what does the widget do and how is it configured")
	if source != "explicit" {
		t.Fatalf("source = %q, want explicit", source)
	}
	if !strings.HasPrefix(summary, "widget: what does the widget do") {
		t.Fatalf("summary = %q, want explicit text prefixed by label", summary)
	}

	// No explicit: falls back to the README.
	summary, source = summaryFor(g, "widget", "")
	if source != "readme" {
		t.Fatalf("source = %q, want readme", source)
	}
	if !strings.Contains(summary, "Widget does widget things") {
		t.Fatalf("summary = %q, want README prose", summary)
	}

	// No explicit, no README: falls back to stats.
	g2 := buildDocsGraph("nodoc", map[string]string{"notes.md": "## Just a section\n\nSome prose about nodoc.\n"})
	summary, source = summaryFor(g2, "nodoc", "")
	if source != "stats" {
		t.Fatalf("source = %q, want stats", source)
	}
	if !strings.Contains(summary, "nodoc") {
		t.Fatalf("summary = %q, want it to at least name the label", summary)
	}
}

// TestStatsSummaryDocsOnlyFallback is the direct regression test for the root
// cause: before this fix, a docs-only map's stats fallback was built entirely
// from Func/Class/Type symbol names, which such a map has none of — every
// docs-only map embedded roughly the same near-empty "<label> — code graph
// (...). Symbols: " text, making them indistinguishable to the router. statsSummary
// must now name real documents instead.
func TestStatsSummaryDocsOnlyFallback(t *testing.T) {
	// Three docs, no README, with a deliberately different number of "## "
	// sections each so the most-connected ranking has something to discriminate
	// on. "big.md" (3 sections) should rank first; "small.md" (1 section)
	// should still appear (docTitleSampleCap is generous); a duplicate title
	// must be deduped.
	files := map[string]string{
		"docs/big.md":   "# Big Topic\n\n## One\nprose\n\n## Two\nprose\n\n## Three\nprose\n",
		"docs/mid.md":   "# Mid Topic\n\n## One\nprose\n\n## Two\nprose\n",
		"docs/small.md": "# Small Topic\n\n## Only\nprose\n",
		"docs/dup.md":   "# Big Topic\n\n## Other\nprose\n", // same title as big.md — must be deduped
	}
	g := buildDocsGraph("corpus", files)

	// Sanity: this graph really has zero Func/Class/Type nodes, so the old
	// unconditional code-graph line would have produced empty Symbols text.
	for _, n := range g.Nodes() {
		if n.Kind == "Func" || n.Kind == "Class" || n.Kind == "Type" {
			t.Fatalf("test fixture unexpectedly contains a code symbol: %+v", n)
		}
	}

	s := statsSummary(g, "corpus")
	if !strings.Contains(s, "document collection") {
		t.Fatalf("statsSummary = %q, want the docs-only fallback shape", s)
	}
	if !strings.Contains(s, "Documents:") {
		t.Fatalf("statsSummary = %q, want a Documents: list", s)
	}
	if !strings.Contains(s, "Big Topic") {
		t.Fatalf("statsSummary = %q, want it to name the most-connected doc (Big Topic)", s)
	}
	if !strings.Contains(s, "Small Topic") {
		t.Fatalf("statsSummary = %q, want it to also name a less-connected doc (Small Topic)", s)
	}
	// "Big Topic" must appear exactly once despite two source files sharing it.
	if got := strings.Count(s, "Big Topic"); got != 1 {
		t.Fatalf("statsSummary = %q, want deduped title, got %d occurrences", s, got)
	}

	// docTitles itself: "Big Topic" (3 sections) must rank ahead of "Small
	// Topic" (1 section) — the most-connected-first ordering the router summary
	// relies on to lead with the more substantial document.
	titles := docTitles(g, 10)
	bigIdx, smallIdx := -1, -1
	for i, ti := range titles {
		if ti == "Big Topic" {
			bigIdx = i
		}
		if ti == "Small Topic" {
			smallIdx = i
		}
	}
	if bigIdx == -1 || smallIdx == -1 {
		t.Fatalf("docTitles = %v, want both Big Topic and Small Topic present", titles)
	}
	if bigIdx >= smallIdx {
		t.Fatalf("docTitles = %v, want Big Topic (more sections) ranked before Small Topic", titles)
	}
}

// TestStatsSummaryCodeMapUnchanged pins the pre-existing code-map behavior: a
// graph with Func/Class/Type symbols still gets the Symbols: line, not the new
// Documents: fallback, even when it also has a Doc node lying around (mirrors a
// real repo's mix of code + a README/docs).
func TestStatsSummaryCodeMapUnchanged(t *testing.T) {
	docRes := docs.Parse("NOTES.md", []byte("## Section\nprose\n"))
	rg := graph.RepoGraph{
		Repo:  "coderepo",
		Nodes: append(append([]parser.NodeInfo{}, docRes.Nodes...), parser.NodeInfo{Kind: "Func", Name: "DoThing", FilePath: "main.go"}),
		Edges: docRes.Edges,
	}
	g := graph.New()
	g.Build(rg)

	s := statsSummary(g, "coderepo")
	if !strings.Contains(s, "Symbols:") {
		t.Fatalf("statsSummary = %q, want the code-map Symbols: line preserved", s)
	}
	if !strings.Contains(s, "DoThing") {
		t.Fatalf("statsSummary = %q, want the Func name sampled", s)
	}
	if strings.Contains(s, "document collection") {
		t.Fatalf("statsSummary = %q, must not take the docs-only fallback when symbols exist", s)
	}
}
