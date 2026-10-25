package index

import (
	"context"
	"fmt"
	"testing"

	"github.com/rs/zerolog"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/graph"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/parser"
)

// A query that is exactly a node's name must return that node first, even when
// a prefix-sharing neighbour wins on similarity. Without the exact channel the
// two tie at rank 0 (one lexical, one semantic) and the tie breaks on QN, where
// the neighbour's leading capital sorts first.
//
// Fixture: "ravenlock" is named by the query and given the WORST vector;
// "RavDP1987" shares its prefix, gets the best vector, and sorts first on QN.
func TestSearchNamedNodeWinsOverSemanticNeighbour(t *testing.T) {
	q := []float32{1, 0, 0, 0}
	neighbourVec := []float32{1, 0, 0, 0}   // identical to the query: best possible cosine
	namedVec := []float32{0, 0, 0, 1}       // orthogonal: worst possible cosine
	fillerVec := []float32{0.9, 0.44, 0, 0} // between the two

	vecByText := map[string][]float32{}
	var nodes []parser.NodeInfo

	namedContent := "ravenlock. field engineer. 10 years. widgets, sprockets, flanges."
	vecByText[namedContent] = namedVec
	nodes = append(nodes, parser.NodeInfo{Kind: "Entry", Name: "ravenlock", FilePath: "catalog.json", Content: namedContent})

	neighbourContent := "RavDP1987. field engineer. 4 years. gaskets, couplings."
	vecByText[neighbourContent] = neighbourVec
	nodes = append(nodes, parser.NodeInfo{Kind: "Entry", Name: "RavDP1987", FilePath: "catalog.json", Content: neighbourContent})

	for i := 0; i < 20; i++ {
		c := fmt.Sprintf("filler%d. field engineer. gaskets, couplings, flanges.", i)
		vecByText[c] = fillerVec
		nodes = append(nodes, parser.NodeInfo{
			Kind: "Entry", Name: fmt.Sprintf("filler%d", i), FilePath: "catalog.json", Content: c,
		})
	}

	g := graph.New()
	g.Build(graph.RepoGraph{Repo: "r", Nodes: nodes})
	ix := New(g, &statsStubEmbedder{vecByText: vecByText, queryVec: q}, zerolog.Nop())
	if err := ix.Build(context.Background()); err != nil {
		t.Fatalf("Build: %v", err)
	}
	ix.SetBruteForce(true) // deterministic semantic order

	hits, err := ix.Search(context.Background(), "ravenlock", nil, "", Select{}, 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	if hits[0].Title != "ravenlock" {
		got := make([]string, len(hits))
		for i, h := range hits {
			got[i] = fmt.Sprintf("%s(score=%.3f fused=%.5f)", h.Title, h.Score, h.Fused)
		}
		t.Errorf("exact-identifier query must return the named node first, got %v", got)
	}
	// The named node has the worst cosine here, so Fused must carry the order.
	if hits[0].Fused <= hits[1].Fused {
		t.Errorf("named node Fused=%v must exceed runner-up Fused=%v", hits[0].Fused, hits[1].Fused)
	}
}

// A query with no keyword signal still needs a fused score, or merging it
// against a fused map's hits sorts every one of them to the bottom.
func TestSearchStampsFusedOnSemanticOnlyPath(t *testing.T) {
	q := []float32{1, 0, 0, 0, 0, 0, 0, 0}
	vecByText := map[string][]float32{}
	var nodes []parser.NodeInfo
	for i := 0; i < 5; i++ {
		c := fmt.Sprintf("doc %d", i)
		vecByText[c] = []float32{1 - float32(i)/10, float32(i) / 10, 0, 0, 0, 0, 0, 0}
		nodes = append(nodes, parser.NodeInfo{Kind: "Doc", Name: fmt.Sprintf("d%d", i), FilePath: "d.md", Content: c})
	}
	g := graph.New()
	g.Build(graph.RepoGraph{Repo: "t", Nodes: nodes})
	ix := New(g, &statsStubEmbedder{vecByText: vecByText, queryVec: q}, zerolog.Nop())
	if err := ix.Build(context.Background()); err != nil {
		t.Fatalf("Build: %v", err)
	}
	ix.SetBruteForce(true)
	// "what is it" is entirely stopwords, so the lexical channel abstains.
	hits, err := ix.Search(context.Background(), "what is it", nil, "", Select{}, 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for i, h := range hits {
		want := 1.0 / float64(rrfK+i)
		if h.Fused != want {
			t.Errorf("hit %d: Fused=%v, want %v", i, h.Fused, want)
		}
	}
}

// A renamed node must not stay reachable under its old title.
func TestTitleIndexRetractsOnReindex(t *testing.T) {
	idx := newTrigramIndex()
	idx.index("n1", "ravenlock", "ravenlock identifier record")
	if got := idx.exactTitle(normalizeQuery("ravenlock")); len(got) != 1 || got[0] != "n1" {
		t.Fatalf("exactTitle before reindex = %v, want [n1]", got)
	}
	idx.index("n1", "ravenlock2", "ravenlock identifier record")
	if got := idx.exactTitle(normalizeQuery("ravenlock")); len(got) != 0 {
		t.Errorf("old title still resolves after reindex: %v", got)
	}
	if got := idx.exactTitle(normalizeQuery("ravenlock2")); len(got) != 1 || got[0] != "n1" {
		t.Errorf("exactTitle after reindex = %v, want [n1]", got)
	}
	idx.reset()
	if got := idx.exactTitle(normalizeQuery("ravenlock2")); len(got) != 0 {
		t.Errorf("reset did not clear the title index: %v", got)
	}
}
