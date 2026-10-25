package graph

import (
	"testing"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/parser"
)

// Sibling parts repeat their name under every parent, so an unqualified dump
// title renders distinct nodes as duplicates. Search qualified; Dump did not.
func TestDumpQualifiesChildTitles(t *testing.T) {
	g := New()
	g.Build(RepoGraph{Repo: "r", Nodes: []parser.NodeInfo{
		{Kind: "Entry", Name: "alpha", FilePath: "c.json", Content: "first entry."},
		{Kind: "Part", Name: "outcome", ParentName: "alpha", FilePath: "c.json", Content: "alpha outcome."},
		{Kind: "Entry", Name: "beta", FilePath: "c.json", Content: "second entry."},
		{Kind: "Part", Name: "outcome", ParentName: "beta", FilePath: "c.json", Content: "beta outcome."},
	}})

	titles := map[string]int{}
	dump := g.Dump()
	for _, n := range dump.Nodes {
		titles[n.Title]++
	}
	if titles["outcome"] != 0 {
		t.Errorf("a child must not dump under its bare name: %v", titles)
	}
	for _, want := range []string{"alpha.outcome", "beta.outcome"} {
		if titles[want] != 1 {
			t.Errorf("want one node titled %q, got %d (all: %v)", want, titles[want], titles)
		}
	}
	// Distinct nodes must have distinct titles.
	if len(titles) != len(dump.Nodes) {
		t.Errorf("%d nodes share only %d titles: %v", len(dump.Nodes), len(titles), titles)
	}
}

// A top-level node keeps its plain name: qualifying children must not rename
// anything already unambiguous.
func TestDisplayTitleLeavesParentlessNodesAlone(t *testing.T) {
	plain := &Node{NodeInfo: parser.NodeInfo{Name: "Search"}}
	if got := DisplayTitle(plain); got != "Search" {
		t.Errorf("DisplayTitle = %q, want Search", got)
	}
	child := &Node{NodeInfo: parser.NodeInfo{Name: "Search", ParentName: "Index"}}
	if got := DisplayTitle(child); got != "Index.Search" {
		t.Errorf("DisplayTitle = %q, want Index.Search", got)
	}
}
