package mcpserver

import (
	"strings"
	"testing"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/index"
)

func TestSelectionNote_SpeaksOnlyWhenASelectionWasCut(t *testing.T) {
	attr := index.AttrFilter{Key: "gitops", Value: "yes"}
	if n := selectionNote(index.Select{Attr: attr}, 30, 20); n == "" {
		t.Fatalf("a cut selection must say so")
	} else if !strings.Contains(n, "30") || !strings.Contains(n, "20") {
		t.Fatalf("the note must carry both the set size and what was returned: %q", n)
	}
	if n := selectionNote(index.Select{Attr: attr}, 7, 7); n != "" {
		t.Errorf("a complete selection needs no note, got %q", n)
	}
	if n := selectionNote(index.Select{Attr: attr}, 3, 20); n != "" {
		t.Errorf("fewer matches than the cap is complete, got %q", n)
	}
	if n := selectionNote(index.Select{}, 30, 5); n != "" {
		t.Errorf("a ranked search is not a selection, got %q", n)
	}
}

// A selection may return more than a ranked search: cutting a ranked list drops
// a less-relevant tail, cutting a selection drops members the caller asked for.
func TestClampLimit_SelectionGetsTheLargerCap(t *testing.T) {
	cases := []struct {
		requested int
		selecting bool
		want      int
	}{
		{0, false, defaultSearchLimit},
		{0, true, defaultSearchLimit},
		{-1, true, defaultSearchLimit},
		{10, false, 10},
		{50, false, maxSearchLimit},
		{50, true, 50},
		{500, true, maxSelectionLimit},
		{500, false, maxSearchLimit},
	}
	for _, c := range cases {
		if got := clampLimit(c.requested, c.selecting); got != c.want {
			t.Errorf("clampLimit(%d, selecting=%v) = %d, want %d", c.requested, c.selecting, got, c.want)
		}
	}
}

func TestParseSelect_ReadsKindsAndAttr(t *testing.T) {
	sel, err := parseSelect(map[string]any{"kind": "Student, Answer", "attr": "years=30"})
	if err != nil {
		t.Fatalf("parseSelect: %v", err)
	}
	if len(sel.Kinds) != 2 || !sel.Kinds["Student"] || !sel.Kinds["Answer"] {
		t.Fatalf("comma-separated kinds not parsed: %v", sel.Kinds)
	}
	if sel.Attr.Key != "years" || sel.Attr.Value != "30" {
		t.Fatalf("attr not parsed: %+v", sel.Attr)
	}
	empty, err := parseSelect(map[string]any{})
	if err != nil {
		t.Fatalf("parseSelect empty: %v", err)
	}
	if !empty.Empty() {
		t.Fatalf("no arguments must name no set, got %+v", empty)
	}
	if sel, _ := parseSelect(map[string]any{"kind": " , ,"}); !sel.Empty() {
		t.Errorf("blank kinds must name no set, got %+v", sel)
	}
}

// A selection that matched nothing because the map does not use that name is a
// vocabulary mismatch, not an absent answer — the reply says which kinds it has.
func TestEmptySelectionNote_NamesTheKindsTheMapHolds(t *testing.T) {
	g := catalogGraph() // holds Entry and Field nodes
	sel := index.Select{Kinds: map[string]bool{"Student": true}}
	n := emptySelectionNote(sel, g)
	if n == "" {
		t.Fatalf("a kind the map does not hold must be explained")
	}
	if !strings.Contains(n, "Entry") || !strings.Contains(n, "Field") {
		t.Fatalf("the note must name the kinds the map does hold: %q", n)
	}
	if got := emptySelectionNote(index.Select{Kinds: map[string]bool{"Entry": true}}, g); got != "" {
		t.Errorf("a kind the map holds needs no note, got %q", got)
	}
	if got := emptySelectionNote(index.Select{Attr: index.AttrFilter{Key: "a", Value: "b"}}, g); got != "" {
		t.Errorf("an attribute miss is not a vocabulary mismatch, got %q", got)
	}
}

// An order is part of the selection: it is parsed like attr, a malformed one is
// an error rather than a silent fall-back to relevance order, and the note that
// describes a cut selection names it.
func TestParseSelect_ReadsOrder(t *testing.T) {
	sel, err := parseSelect(map[string]any{"kind": "Student", "order": "years:desc"})
	if err != nil {
		t.Fatalf("parseSelect: %v", err)
	}
	if sel.Order.Key != "years" || !sel.Order.Desc {
		t.Fatalf("order not parsed: %+v", sel.Order)
	}
	if d := describeSelect(sel); !strings.Contains(d, "order years desc") {
		t.Errorf("describeSelect must name the order, got %q", d)
	}
	if n := selectionNote(sel, 37, 5); !strings.Contains(n, "order years desc") {
		t.Errorf("a cut ordered selection must say how it was ordered, got %q", n)
	}
	only, err := parseSelect(map[string]any{"order": "years"})
	if err != nil {
		t.Fatalf("parseSelect order-only: %v", err)
	}
	if only.Empty() || only.Order.Desc {
		t.Errorf("an order alone selects the whole scope, ascending by default: %+v", only)
	}
	if _, err := parseSelect(map[string]any{"kind": "Student", "order": "years:sideways"}); err == nil {
		t.Errorf("a malformed order must be an error, not relevance order")
	}
}

// A child hit carries its containing node; a top-level hit's shape is unchanged.
func TestBriefHit_CarriesParentOnlyForChildren(t *testing.T) {
	part := briefHit(index.Hit{QN: "m//c.json::alice.aire", Kind: "Answer", Parent: "m//c.json::alice"})
	if got, _ := part["parent"].(string); got != "m//c.json::alice" {
		t.Fatalf("a part must point back to its entity, got %v", part["parent"])
	}
	entry := briefHit(index.Hit{QN: "m//c.json::alice", Kind: "Student"})
	if _, has := entry["parent"]; has {
		t.Fatalf("a top-level hit must not carry a parent key: %v", entry)
	}
}
