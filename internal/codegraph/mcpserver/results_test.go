package mcpserver

import (
	"strings"
	"testing"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/docs"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/graph"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/parser"
)

// docOnlyGraph is the shape of an agent-memory map (ccmem): Doc and Section
// nodes joined by contains edges, nothing else — built through docs.Parse so
// it is the real shape, not a hand-made approximation.
func docOnlyGraph(t *testing.T) *graph.Graph {
	t.Helper()
	res := docs.Parse("notes/a.md", []byte("# Title\n\nIntro.\n\n## One\n\nBody.\n"))
	g := graph.New()
	g.Build(graph.RepoGraph{Repo: "m", Nodes: res.Nodes, Edges: res.Edges})
	return g
}

// codeGraph has one call edge (a → b) and one function nothing touches.
func codeGraph(t *testing.T) *graph.Graph {
	t.Helper()
	nodes := []parser.NodeInfo{
		{Kind: "Func", Name: "a", FilePath: "x.go"},
		{Kind: "Func", Name: "b", FilePath: "x.go"},
		{Kind: "Func", Name: "lonely", FilePath: "x.go"},
	}
	edges := []parser.EdgeInfo{{
		Kind: parser.EdgeCalls, Source: "x.go::a", Target: "b",
		Extra: map[string]string{"callName": "b", "bare": "1"},
	}}
	g := graph.New()
	g.Build(graph.RepoGraph{Repo: "m", Nodes: nodes, Edges: edges})
	return g
}

func TestSearchKinds_WidensToWhatTheMapHolds(t *testing.T) {
	g := docOnlyGraph(t)
	kinds, note := searchKinds("search_graph", codeKinds, searchedMap{Label: "ccmem", Graph: g})
	if kinds != nil {
		t.Fatalf("a doc-only map under search_graph should be searched without a kind filter, got %v", kinds)
	}
	// The note must hand over the lever — kind=<the map's kinds>, most numerous
	// first, and that a selection can be ordered — not just describe the map:
	// a caller that only learns what a map holds still answers from the few
	// hits a query happened to rank.
	for _, want := range []string{`map "ccmem"`, "Class/Config/Deployment/File/Func/Image/Manifest/Routine/Sequence/Table/Test/Trigger/Type/View", "Section=2 Doc=1", "search_graph ranked all of its kinds", "kind=Section or kind=Doc", "order=", `"selected"`} {
		if !strings.Contains(note, want) {
			t.Errorf("note %q should mention %q", note, want)
		}
	}
	// Live mode serves one unnamed graph.
	if _, note := searchKinds("search_graph", codeKinds, searchedMap{Graph: g}); !strings.HasPrefix(note, "this map holds no") {
		t.Errorf("unnamed map should read as 'this map', got %q", note)
	}
	// The reverse: a code-only map under search_graph_docs.
	if kinds, note := searchKinds("search_graph_docs", docKinds, searchedMap{Label: "code", Graph: codeGraph(t)}); kinds != nil || !strings.Contains(note, "search_graph_docs ranked all") {
		t.Errorf("code-only map under search_graph_docs: kinds=%v note=%q", kinds, note)
	}
}

func TestSearchKinds_KeepsToolKindsWhenTheMapHasThem(t *testing.T) {
	g := docOnlyGraph(t)
	if kinds, note := searchKinds("search_graph_docs", docKinds, searchedMap{Label: "ccmem", Graph: g}); note != "" || len(kinds) != len(docKinds) {
		t.Fatalf("a docs search over a doc map keeps its filter: kinds=%v note=%q", kinds, note)
	}
	if kinds, note := searchKinds("search_graph", codeKinds, searchedMap{Label: "code", Graph: codeGraph(t)}); note != "" || len(kinds) != len(codeKinds) {
		t.Fatalf("a code search over a code map keeps its filter: kinds=%v note=%q", kinds, note)
	}
	if kinds, note := searchKinds("search_graph", codeKinds, searchedMap{Label: "empty", Graph: graph.New()}); note != "" || len(kinds) != len(codeKinds) {
		t.Fatalf("an empty map has nothing to widen to: kinds=%v note=%q", kinds, note)
	}
	if kinds, note := searchKinds("search", nil, searchedMap{Label: "ccmem", Graph: g}); note != "" || kinds != nil {
		t.Fatalf("a search with no scope has nothing to widen: kinds=%v note=%q", kinds, note)
	}
}

func TestImpactResult_DocSeedSaysWhyItIsAlone(t *testing.T) {
	out := impactResult(docOnlyGraph(t), []string{"m//notes/a.md"}, 2, 200)
	if out["count"] != 1 {
		t.Fatalf("a Doc seed reaches nothing, count=%v", out["count"])
	}
	note, _ := out["note"].(string)
	if !strings.Contains(note, "no dependency edges at all") || !strings.Contains(note, "contains=") {
		t.Fatalf("note should explain the map is structure-only, got %q", note)
	}
}

func TestImpactResult_IsolatedCodeSeedSaysNothingReaches(t *testing.T) {
	g := codeGraph(t)
	out := impactResult(g, []string{"m//x.go::lonely"}, 2, 200)
	note, _ := out["note"].(string)
	if out["count"] != 1 || !strings.Contains(note, "reaches beyond the seeds") {
		t.Fatalf("isolated seed: count=%v note=%q", out["count"], note)
	}

	out = impactResult(g, []string{"m//x.go::a"}, 2, 200)
	if _, has := out["note"]; has || out["count"] != 2 {
		t.Fatalf("a connected seed needs no note: count=%v note=%v", out["count"], out["note"])
	}
}

func TestImpactResult_UnknownSeedsAreReportedNotTraversed(t *testing.T) {
	g := codeGraph(t)
	out := impactResult(g, []string{"m//x.go::nope", "m//x.go::a"}, 2, 200)
	unknown, _ := out["unknown"].([]string)
	if len(unknown) != 1 || unknown[0] != "m//x.go::nope" {
		t.Fatalf("unknown=%v", out["unknown"])
	}
	if out["count"] != 2 {
		t.Fatalf("the known seed still traverses: count=%v", out["count"])
	}
	if note, _ := out["note"].(string); !strings.Contains(note, "unknown seeds") {
		t.Fatalf("note should mention unknown seeds, got %q", note)
	}

	out = impactResult(g, []string{"m//x.go::nope"}, 2, 200)
	impacted, _ := out["impacted"].([]graph.ImpactNode)
	if out["count"] != 0 || impacted == nil || len(impacted) != 0 {
		t.Fatalf("all-unknown seeds: count=%v impacted=%#v (want an empty, non-nil list)", out["count"], out["impacted"])
	}
}

// ImpactDirectGraph backs the live /impact route; its zero depth and max_nodes
// must mean the tool's defaults, or `xray-memory impact` would answer a
// different question than get_graph_impact.
func TestImpactDirectGraph_ZeroMeansToolDefaults(t *testing.T) {
	g := codeGraph(t)
	got, err := ImpactDirectGraph(g, []string{"m//x.go::a"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := jsonString(impactResult(g, []string{"m//x.go::a"}, 2, 200))
	if got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
	if _, err := ImpactDirectGraph(g, nil, 0, 0); err == nil {
		t.Fatal("no seeds: want an error")
	}
}
