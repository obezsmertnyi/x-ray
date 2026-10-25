package index

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/graph"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/parser"
)

// The vocabulary is computed over the scope an order/attr applies to — the
// tool's kinds, the repo and the selection's KINDS — and never narrowed by the
// selection's own attr, or an unknown key would report an empty scope.
func TestAttrVocabulary_ScopesByKindsNotByAttr(t *testing.T) {
	nodes := []parser.NodeInfo{
		{Kind: "Student", Name: "a", FilePath: "c.json", Content: "a", Attrs: map[string]string{"years": "3", "role": "dev"}},
		{Kind: "Student", Name: "b", FilePath: "c.json", Content: "b", Attrs: map[string]string{"years": "30"}},
		{Kind: "Student", Name: "c", FilePath: "c.json", Content: "c", Attrs: map[string]string{"years": "30", "fw:none": "yes"}},
		{Kind: "Answer", Name: "x", FilePath: "c.json", Content: "x", Attrs: map[string]string{"other": "1"}},
	}
	g := graph.New()
	g.Build(graph.RepoGraph{Repo: "m", Nodes: nodes})
	ix := New(g, &hashEmbedder{dims: 8}, zerolog.Nop())
	if err := ix.Build(context.Background()); err != nil {
		t.Fatal(err)
	}
	sel := Select{Kinds: map[string]bool{"Student": true}, Attr: AttrFilter{Key: "experience", Value: "30"}}
	v := ix.AttrVocabulary(nil, "", sel, "years")
	if v.Nodes != 3 {
		t.Fatalf("scope is the three Students, got %d", v.Nodes)
	}
	if v.Keys["years"] != 3 || v.Keys["role"] != 1 || v.Keys["fw:none"] != 1 || v.Keys["other"] != 0 {
		t.Fatalf("keys: %v", v.Keys)
	}
	if v.Values["30"] != 2 || v.Values["3"] != 1 || len(v.Values) != 2 {
		t.Fatalf("values of years: %v", v.Values)
	}
	// The tool's kinds and repo filter narrow the scope the same way wants does.
	if got := ix.AttrVocabulary(map[string]bool{"Answer": true}, "", Select{}, "").Keys; got["other"] != 1 || got["years"] != 0 {
		t.Fatalf("tool kinds must scope: %v", got)
	}
	if got := ix.AttrVocabulary(nil, "elsewhere", Select{}, ""); got.Nodes != 0 {
		t.Fatalf("repo filter must scope: %+v", got)
	}

	// A title match is case-insensitive, ignores a leading '@', also matches
	// the qn's last segment, and respects the scope and the cap.
	if got := ix.TitledNodes(nil, "", sel, "@B", 3); len(got) != 1 || got[0].QN != "m//c.json::b" {
		t.Fatalf("title match: %v", got)
	}
	if got := ix.TitledNodes(nil, "", sel, "x", 3); len(got) != 0 {
		t.Fatalf("an Answer is outside a Student scope: %v", got)
	}
	if got := ix.TitledNodes(nil, "", Select{}, "x", 3); len(got) != 1 {
		t.Fatalf("without kinds the whole map is the scope: %v", got)
	}
	if got := ix.TitledNodes(nil, "", sel, "nobody", 3); got != nil {
		t.Fatalf("no match, no nodes: %v", got)
	}
}
