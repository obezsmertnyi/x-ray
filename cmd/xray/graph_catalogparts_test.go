package main

import (
	"testing"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/parser"
)

// Fields concatenated into one entry share one vector and one head-truncated
// preview, so a field at the end of a long record is unreachable. Each part must
// become its own node, under the entry and reachable from it.
func TestCatalogParts_BecomeTheirOwnNodes(t *testing.T) {
	entries := []catalogEntry{{
		Name: "alpha",
		Kind: "Student",
		Text: "alpha. field engineer. 7 years.",
		Parts: []catalogPart{
			{Name: "summary", Kind: "Part", Text: "Summary: what the entry is working on."},
			{Name: "outcome", Kind: "Part", Text: "Outcome: what the entry wants next."},
			{Name: "blank", Kind: "Part", Text: "   "}, // field left empty: no node
		},
	}}
	res, linked, skipped := catalogResults(entries, "cat")
	if skipped != 0 {
		t.Errorf("no link targets are unknown, got skipped=%d", skipped)
	}
	if linked != 2 {
		t.Errorf("two non-empty parts must each be linked, got %d", linked)
	}

	byQN := map[string]parser.NodeInfo{}
	for _, n := range res.Nodes {
		byQN[n.Qualified()] = n
	}
	if len(byQN) != 3 {
		t.Fatalf("want entry + 2 parts = 3 nodes, got %d: %v", len(byQN), byQN)
	}
	part, ok := byQN["cat.catalog.json::alpha.outcome"]
	if !ok {
		t.Fatalf("part node missing; have %v", byQN)
	}
	if part.ParentName != "alpha" {
		t.Errorf("part must sit under its entry, ParentName=%q", part.ParentName)
	}
	if part.Kind != "Part" {
		t.Errorf("part kind = %q, want Part", part.Kind)
	}
	// The part's own text is what gets embedded and previewed.
	if part.Content != "Outcome: what the entry wants next." {
		t.Errorf("part content = %q", part.Content)
	}
	// Attrs select entities, so a part must not carry them: an attr-filtered
	// search has to return the entity, not three of its fields.
	if len(part.Attrs) != 0 {
		t.Errorf("part must carry no attrs, got %v", part.Attrs)
	}

	var contains int
	for _, e := range res.Edges {
		if e.Kind == parser.EdgeContains && e.Source == "cat.catalog.json::alpha" {
			contains++
		}
	}
	if contains != 2 {
		t.Errorf("entry must contain its 2 parts, got %d contains edges", contains)
	}
}

// A part with no kind derives one from its entry; a link may target a part by
// <entry>.<part>.
func TestCatalogParts_DefaultKindAndLinkTargets(t *testing.T) {
	entries := []catalogEntry{
		{Name: "a", Kind: "Entry", Text: "a.",
			Parts: []catalogPart{{Name: "note", Text: "a note."}},
			Links: []catalogLink{{Rel: "peer", To: "b.note"}}},
		{Name: "b", Kind: "Entry", Text: "b.",
			Parts: []catalogPart{{Name: "note", Text: "another note."}}},
	}
	res, _, skipped := catalogResults(entries, "cat")
	if skipped != 0 {
		t.Errorf("a link to a part must resolve, got skipped=%d", skipped)
	}
	for _, n := range res.Nodes {
		if n.Name == "note" && n.Kind != "EntryPart" {
			t.Errorf("kindless part should derive from its entry, got %q", n.Kind)
		}
	}
}
