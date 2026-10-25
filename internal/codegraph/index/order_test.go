package index

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/graph"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/parser"
)

// studentCorpus is the team map's shape: Student entries carrying a numeric
// "years" attribute (one without), each with an Answer part under it linked by
// "contains". The incident: "most experienced student" ranked the parts, and a
// kind=Student selection ranked by cosine led with an 11-year student.
func studentCorpus(t *testing.T) *Index {
	t.Helper()
	students := []struct{ name, years string }{
		{"jane", "11"}, {"morgan", "30"}, {"riley", "8"}, {"casey", "20"}, {"nobody", ""},
	}
	var nodes []parser.NodeInfo
	var edges []parser.EdgeInfo
	for _, s := range students {
		attrs := map[string]string{"role": "eng"}
		if s.years != "" {
			attrs["years"] = s.years
		}
		nodes = append(nodes,
			parser.NodeInfo{Kind: "Student", Name: s.name, FilePath: "h.json",
				Content: s.name + " engineer " + s.years + " years", Attrs: attrs},
			parser.NodeInfo{Kind: "Answer", Name: "aire", ParentName: s.name, FilePath: "h.json",
				Content: s.name + " took the course"},
		)
		edges = append(edges, parser.EdgeInfo{
			Kind: parser.EdgeContains, Source: "h.json::" + s.name, Target: "h.json::" + s.name + ".aire", File: "h.json",
		})
	}
	g := graph.New()
	g.Build(graph.RepoGraph{Repo: "h", Nodes: nodes, Edges: edges})
	ix := New(g, &hashEmbedder{dims: 8}, zerolog.Nop())
	ix.SetBruteForce(true)
	if err := ix.Build(context.Background()); err != nil {
		t.Fatalf("Build: %v", err)
	}
	return ix
}

func TestParseOrder(t *testing.T) {
	cases := []struct {
		in      string
		want    Order
		wantErr bool
	}{
		{"", Order{}, false},
		{"years", Order{Key: "years"}, false},
		{"years:desc", Order{Key: "years", Desc: true}, false},
		{"years:asc", Order{Key: "years"}, false},
		{"years:up", Order{}, true},
		{":desc", Order{}, true},
	}
	for _, c := range cases {
		got, err := ParseOrder(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("ParseOrder(%q) err=%v, wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if got != c.want {
			t.Errorf("ParseOrder(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
	if (Select{Order: Order{Key: "years"}}).Empty() {
		t.Errorf("an order alone names a selection")
	}
	if !(Select{}).Empty() {
		t.Errorf("the zero Select names nothing")
	}
}

func titles(hits []Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.Title
	}
	return out
}

func equal(a, b []string) bool {
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

// A superlative is a sort: the selected set comes back in attribute order,
// numerically, with a member that lacks the attribute last either way.
func TestSearch_OrderSortsASelectionByAttribute(t *testing.T) {
	ix := studentCorpus(t)
	students := map[string]bool{"Student": true}

	hits, err := ix.Search(context.Background(), "most experienced student", nil, "",
		Select{Kinds: students, Order: Order{Key: "years", Desc: true}}, 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got, want := titles(hits), []string{"morgan", "casey", "jane", "riley", "nobody"}; !equal(got, want) {
		t.Fatalf("desc order = %v, want %v", got, want)
	}
	for i := 1; i < len(hits); i++ {
		if hits[i-1].Fused <= hits[i].Fused {
			t.Fatalf("an ordered selection must still stamp a descending fused score")
		}
	}

	hits, err = ix.Search(context.Background(), "most experienced student", nil, "",
		Select{Kinds: students, Order: Order{Key: "years"}}, 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got, want := titles(hits), []string{"riley", "jane", "casey", "morgan", "nobody"}; !equal(got, want) {
		t.Fatalf("asc order = %v, want %v", got, want)
	}

	// k still caps: the top of a desc order is the answer to "most".
	hits, err = ix.Search(context.Background(), "most experienced student", nil, "",
		Select{Kinds: students, Order: Order{Key: "years", Desc: true}}, 1)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got := titles(hits); !equal(got, []string{"morgan"}) {
		t.Fatalf("k=1 of a desc order = %v, want [morgan]", got)
	}
}

// Values that are not numbers sort as strings, and a mixed pair falls back to
// strings too rather than failing.
func TestSearch_OrderFallsBackToStringCompare(t *testing.T) {
	nodes := []parser.NodeInfo{
		{Kind: "Svc", Name: "s1", FilePath: "c.json", Content: "s1", Attrs: map[string]string{"tier": "b"}},
		{Kind: "Svc", Name: "s2", FilePath: "c.json", Content: "s2", Attrs: map[string]string{"tier": "a"}},
		{Kind: "Svc", Name: "s3", FilePath: "c.json", Content: "s3", Attrs: map[string]string{"tier": "c"}},
	}
	g := graph.New()
	g.Build(graph.RepoGraph{Repo: "m", Nodes: nodes})
	ix := New(g, &hashEmbedder{dims: 8}, zerolog.Nop())
	ix.SetBruteForce(true)
	if err := ix.Build(context.Background()); err != nil {
		t.Fatalf("Build: %v", err)
	}
	hits, err := ix.Search(context.Background(), "svc", nil, "", Select{Order: Order{Key: "tier"}}, 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got, want := titles(hits), []string{"s2", "s1", "s3"}; !equal(got, want) {
		t.Fatalf("string order = %v, want %v", got, want)
	}
}

// An order alone selects the whole scope: every node the tool's kinds admit.
func TestSearch_OrderAloneSelectsTheWholeScope(t *testing.T) {
	ix := studentCorpus(t)
	hits, err := ix.Search(context.Background(), "x", nil, "", Select{Order: Order{Key: "years", Desc: true}}, 100)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 10 {
		t.Fatalf("order alone should select all 10 nodes, got %d", len(hits))
	}
	if hits[0].Title != "morgan" || hits[len(hits)-1].Kind != "Answer" {
		t.Fatalf("nodes with the attribute lead, those without trail: %v", titles(hits))
	}
	if n := ix.CountMatching(nil, "", Select{Order: Order{Key: "years"}}); n != 10 {
		t.Fatalf("CountMatching under an order alone = %d, want 10", n)
	}
}

// A part's hit carries the entity that contains it; a top-level node's does not.
func TestSearch_ChildHitCarriesItsParent(t *testing.T) {
	ix := studentCorpus(t)
	hits, err := ix.Search(context.Background(), "took the course", nil, "", Select{Kinds: map[string]bool{"Answer": true}}, 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("no Answer hits")
	}
	for _, h := range hits {
		n, _ := ix.g.Get(h.QN)
		if want := "h//h.json::" + n.ParentName; h.Parent != want {
			t.Fatalf("part %q parent = %q, want %q", h.QN, h.Parent, want)
		}
	}
	hits, err = ix.Search(context.Background(), "engineer", nil, "", Select{Kinds: map[string]bool{"Student": true}}, 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, h := range hits {
		if h.Parent != "" {
			t.Fatalf("top-level node %q must carry no parent, got %q", h.QN, h.Parent)
		}
	}
}
