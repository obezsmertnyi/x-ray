package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/rs/zerolog"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/graph"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/index"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/parser"
)

// harnessGraph is the shape of the catalog map the incident happened on:
// Student entries carrying years/role and namespaced flags, each with an
// Answer part carrying no attribute at all. Years are chosen so that a string
// sort would misplace them ("10" < "3").
func harnessGraph(t *testing.T) (*graph.Graph, *index.Index) {
	t.Helper()
	years := []string{"3", "8", "10", "30", "11", "30"}
	names := []string{"alice", "bob", "carol", "dave", "erin", "octocat42"}
	var nodes []parser.NodeInfo
	for i, name := range names {
		nodes = append(nodes,
			parser.NodeInfo{Kind: "Student", Name: name, FilePath: "academy.catalog.json",
				Content: "student " + name,
				Attrs: map[string]string{"years": years[i], "role": "engineer",
					"level:ide-only": "yes", "tool:gitops": "no"}},
			parser.NodeInfo{Kind: "Answer", Name: "aire", ParentName: name, FilePath: "academy.catalog.json",
				Content: "answer of " + name},
		)
	}
	g := graph.New()
	g.Build(graph.RepoGraph{Repo: "academy", Nodes: nodes})
	ix := index.New(g, textEmbedder{}, zerolog.Nop())
	if err := ix.Build(context.Background()); err != nil {
		t.Fatal(err)
	}
	return g, ix
}

// directReply runs SearchDirect against a one-map resolver and decodes the reply.
func directReply(t *testing.T, g *graph.Graph, ix *index.Index, sel index.Select) map[string]any {
	t.Helper()
	d := Deps{Resolver: &fakeResolver{label: "academy", g: g, ix: ix}}
	s, err := SearchDirect(context.Background(), d, "academy", "most experienced student", 5, codeKinds, sel, false)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// liveReply runs the live-mode search handler and decodes the reply.
func liveReply(t *testing.T, g *graph.Graph, ix *index.Index, args map[string]any) map[string]any {
	t.Helper()
	res, err := runSearch(context.Background(), Deps{Graph: g, Index: ix}, "search_graph", args, codeKinds)
	if err != nil {
		t.Fatal(err)
	}
	text, ok := res.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("unexpected content %T", res.Content[0])
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text.Text), &out); err != nil {
		t.Fatalf("%v in %s", err, text.Text)
	}
	return out
}

func noteOf(out map[string]any) string {
	n, _ := out["note"].(string)
	return n
}

// (a) An order on an attribute no Student carries fell through to cosine order
// under a healthy-looking note, and "most experienced" was answered from the
// top hit. The set must still come back — it is the right set — but the note
// must say the order did not apply and name the keys the set does carry.
func TestAttrNote_OrderOnUnknownKeyNamesTheRealKeys(t *testing.T) {
	g, ix := harnessGraph(t)
	sel := index.Select{Kinds: map[string]bool{"Student": true}, Order: index.Order{Key: "experience", Desc: true}}
	out := directReply(t, g, ix, sel)
	if out["count"].(float64) != 5 || out["selected"].(float64) != 6 {
		t.Fatalf("the set must still be returned: %v", out)
	}
	n := noteOf(out)
	want := `no Student node carries attribute "experience", so this order did not apply and the set is in query order; Student nodes carry: level:ide-only=6 role=6 tool:gitops=6 years=6`
	if !strings.Contains(n, want) {
		t.Fatalf("note %q\nmust contain %q", n, want)
	}
	// The live surface shapes the same note.
	live := liveReply(t, g, ix, map[string]any{"query": "most experienced student", "kind": "Student", "order": "experience:desc"})
	if !strings.Contains(noteOf(live), want) {
		t.Fatalf("live note %q\nmust contain %q", noteOf(live), want)
	}
}

// (b) An order on a key the set carries needs no such note.
func TestAttrNote_OrderOnKnownKeyIsSilent(t *testing.T) {
	g, ix := harnessGraph(t)
	sel := index.Select{Kinds: map[string]bool{"Student": true}, Order: index.Order{Key: "years", Desc: true}}
	out := directReply(t, g, ix, sel)
	if n := noteOf(out); strings.Contains(n, "carries attribute") {
		t.Fatalf("a known key must not be reported as unknown: %q", n)
	}
	results, _ := out["results"].([]any)
	if len(results) == 0 {
		t.Fatal("no results")
	}
	if top := results[0].(map[string]any)["title"]; top != "dave" && top != "octocat42" {
		t.Fatalf("years:desc must lead with a 30-year student, got %v", top)
	}
}

// (c) attr on a key no Student carries: the reply is empty, and says which keys
// it could have filtered by.
func TestAttrNote_AttrOnUnknownKeyNamesTheRealKeys(t *testing.T) {
	g, ix := harnessGraph(t)
	sel := index.Select{Kinds: map[string]bool{"Student": true}, Attr: index.AttrFilter{Key: "experience", Value: "30"}}
	out := directReply(t, g, ix, sel)
	if out["count"].(float64) != 0 {
		t.Fatalf("no node has the key, so nothing matches: %v", out)
	}
	n := noteOf(out)
	want := `no Student node carries attribute "experience"; Student nodes carry: level:ide-only=6 role=6 tool:gitops=6 years=6`
	if !strings.Contains(n, want) {
		t.Fatalf("note %q\nmust contain %q", n, want)
	}
	if strings.Contains(n, "is the title of") {
		t.Fatalf("\"30\" names no node, so no title sentence: %q", n)
	}
}

// (d) attr on a known key with a value none has: the note lists the values,
// numerically ordered — "3 8 10 11 30", not "10 11 3 30 8".
func TestAttrNote_AttrOnKnownKeyUnknownValueListsValuesNumerically(t *testing.T) {
	g, ix := harnessGraph(t)
	sel := index.Select{Kinds: map[string]bool{"Student": true}, Attr: index.AttrFilter{Key: "years", Value: "31"}}
	out := directReply(t, g, ix, sel)
	want := `no Student node has years=31; values: 3 8 10 11 30`
	if n := noteOf(out); !strings.Contains(n, want) {
		t.Fatalf("note %q\nmust contain %q", n, want)
	}
	// A value the set has needs no note.
	hit := directReply(t, g, ix, index.Select{Kinds: map[string]bool{"Student": true}, Attr: index.AttrFilter{Key: "years", Value: "30"}})
	if hit["count"].(float64) != 2 || strings.Contains(noteOf(hit), "values:") {
		t.Fatalf("a matching value must be silent: %v", hit)
	}
}

// Addendum: attr=github=octocat42 — "github" is no attribute, the handle is a
// Student's title. The note must point at the node, once, with the qn; a value
// that names nothing gets no such sentence.
func TestAttrNote_UnknownKeyWhoseValueIsATitleNamesTheNode(t *testing.T) {
	g, ix := harnessGraph(t)
	sel := index.Select{Kinds: map[string]bool{"Student": true}, Attr: index.AttrFilter{Key: "github", Value: "@Octocat42"}}
	out := directReply(t, g, ix, sel)
	n := noteOf(out)
	want := `"@Octocat42" is the title of Student node academy//academy.catalog.json::octocat42 — read it with get_graph_node, or put the handle in the query`
	if !strings.Contains(n, want) {
		t.Fatalf("note %q\nmust contain %q", n, want)
	}
	miss := directReply(t, g, ix, index.Select{Kinds: map[string]bool{"Student": true}, Attr: index.AttrFilter{Key: "github", Value: "nobody"}})
	if strings.Contains(noteOf(miss), "is the title of") {
		t.Fatalf("a value naming no node must not claim a title: %q", noteOf(miss))
	}
}

// Without a kind the scope is the whole map, and the note says so by name.
func TestAttrNote_ScopeWordingWithoutKinds(t *testing.T) {
	g, ix := harnessGraph(t)
	out := directReply(t, g, ix, index.Select{Order: index.Order{Key: "experience"}})
	want := `no node in map "academy" carries attribute "experience"`
	if n := noteOf(out); !strings.Contains(n, want) {
		t.Fatalf("note %q\nmust contain %q", n, want)
	}
}

// An unknown KIND is already explained by emptySelectionNote; the vocabulary
// note must not pile "carry no attributes" on top of it.
func TestAttrNote_UnknownKindLeavesItToTheKindNote(t *testing.T) {
	g, ix := harnessGraph(t)
	out := directReply(t, g, ix, index.Select{Kinds: map[string]bool{"Teacher": true}, Order: index.Order{Key: "years"}})
	if n := noteOf(out); strings.Contains(n, "carries attribute") {
		t.Fatalf("an empty scope has no vocabulary to report: %q", n)
	}
}

// Past a dozen keys the namespaced flags fold into their prefix so the plain
// keys stay visible; below that every key is listed.
func TestAttrKeyList_CollapsesNamespacedKeysWhenLong(t *testing.T) {
	keys := map[string]int{"years": 37, "role": 37}
	for _, k := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		keys["level:"+k] = 5
	}
	for _, k := range []string{"a", "b", "c", "d"} {
		keys["tool:"+k] = 3
	}
	keys["fw:none"] = 1
	got := attrKeyList(keys)
	want := "role=37 years=37 level:* (8 keys) tool:* (4 keys) fw:* (1 keys)"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if short := attrKeyList(map[string]int{"years": 2, "level:x": 1}); short != "years=2 level:x=1" {
		t.Fatalf("a short list is not collapsed: %q", short)
	}
}

// Many distinct values collapse to a count; non-numeric values sort as strings
// and a value with a space is quoted.
func TestAttrValuesClause_CountsWhenManyAndQuotesSpaces(t *testing.T) {
	many := map[string]int{}
	for i := 0; i < 21; i++ {
		many[string(rune('a'+i))] = 1
	}
	if got := attrValuesClause(many); got != "values: 21 distinct" {
		t.Fatalf("got %q", got)
	}
	if got := attrValuesClause(map[string]int{"staff engineer": 1, "lead": 2, "10": 1}); got != `values: 10 lead "staff engineer"` {
		t.Fatalf("got %q", got)
	}
}

// (e) get_graph_stats lists the attribute keys per kind for a catalog map,
// skips kinds whose nodes carry none, and adds nothing to a code map's entry.
func TestMapStatsEntry_ListsAttrKeysPerCatalogKind(t *testing.T) {
	g, _ := harnessGraph(t)
	entry := mapStatsEntry(Resolved{Label: "academy", Graph: g})
	attrs, ok := entry["attrs"].(map[string]string)
	if !ok {
		t.Fatalf("a catalog map's stats must carry attrs, got %v", entry["attrs"])
	}
	if attrs["Student"] != "level:ide-only=6 role=6 tool:gitops=6 years=6" {
		t.Fatalf("Student keys: %q", attrs["Student"])
	}
	if _, has := attrs["Answer"]; has {
		t.Fatalf("a kind with no attributes must be absent: %v", attrs)
	}
	code := mapStatsEntry(Resolved{Label: "code", Graph: codeGraph(t)})
	if _, has := code["attrs"]; has {
		t.Fatalf("a code map's stats entry must be unchanged: %v", code)
	}
	// The same entry serves StatsDirect, so the field reaches the wire.
	d := Deps{Resolver: &fakeResolver{label: "academy", g: g}}
	s, err := StatsDirect(context.Background(), d, "academy", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s, `"attrs"`) || !strings.Contains(s, "years=6") {
		t.Fatalf("StatsDirect must carry attrs: %s", s)
	}
}
