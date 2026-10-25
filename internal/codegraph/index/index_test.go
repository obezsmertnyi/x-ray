package index

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/rs/zerolog"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/graph"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/parser"
)

// statsStubEmbedder returns a caller-configured vector per exact node text
// (looked up by the text Index.Build/Search actually embeds), and a fixed
// query vector regardless of query text — so a test can control the semantic
// and lexical signals independently and deterministically, rather than
// relying on a real embedding model's behaviour.
type statsStubEmbedder struct {
	vecByText map[string][]float32
	queryVec  []float32
}

func (e *statsStubEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, ok := e.vecByText[t]
		if !ok {
			return nil, fmt.Errorf("statsStubEmbedder: no vector configured for text %q", t)
		}
		out[i] = v
	}
	return out, nil
}

func (e *statsStubEmbedder) EmbedQuery(_ context.Context, _ string) ([]float32, error) {
	return e.queryVec, nil
}

// TestSearchStatsPartitionInvariant drives several deliberately varied
// searches through a small, hand-built index and checks that
// SearchStats' three hit-provenance buckets (keyword promotions, semantic-only
// hits, both-channel hits) always partition the hits actually returned —
// keywordPromotions + semanticOnlyHits + bothChannelHits must equal the
// number of top-k hits handed back across every call, cumulatively. That
// identity is what makes the promotion count trustworthy: if it can drift
// from the true hit count, it can no longer be read as "this many results
// came from the keyword channel alone".
//
// The graph is built so each scenario is unambiguous rather than incidental:
//   - "C" is the worst semantic match in the whole graph (vector opposite the
//     query) but the ONLY node whose content contains the query's exact
//     (invented, so no accidental overlap) keywords — searching for them must
//     promote it purely via the keyword channel.
//   - "filler0" is the best semantic match (vector identical to the query)
//     AND, for a different query, the only lexical match — proving a
//     both-channel hit.
//   - Twenty plain "fillerN" nodes are semantically close but lexically inert
//     for every query used here, so they only ever land as semantic-only
//     hits, and there are enough of them (21 total non-"C" nodes) that the
//     top-20 semantic over-fetch (searchK, see Index.Search) genuinely
//     excludes "C" rather than happening to include it because the test
//     graph was too small.
func TestSearchStatsPartitionInvariant(t *testing.T) {
	q := []float32{1, 0, 0, 0, 0, 0, 0, 0}
	fillerVec := []float32{0.99, 0.14, 0, 0, 0, 0, 0, 0} // close to q, but strictly worse than bestVec
	bestVec := []float32{1, 0, 0, 0, 0, 0, 0, 0}         // filler0: identical to q — guaranteed best
	worstVec := []float32{-1, 0, 0, 0, 0, 0, 0, 0}       // C: opposite of q — guaranteed worst

	vecByText := map[string][]float32{}
	var nodes []parser.NodeInfo

	cContent := "zzflurbnix quorlex marker node"
	vecByText[cContent] = worstVec
	nodes = append(nodes, parser.NodeInfo{Kind: "Doc", Name: "c", FilePath: "c.md", Content: cContent})

	filler0Content := "filler node zero about widgets ubjello vexnorak"
	vecByText[filler0Content] = bestVec
	nodes = append(nodes, parser.NodeInfo{Kind: "Doc", Name: "filler0", FilePath: "filler0.md", Content: filler0Content})

	for i := 1; i <= 20; i++ {
		content := fmt.Sprintf("filler node %d about widgets", i)
		vecByText[content] = fillerVec
		nodes = append(nodes, parser.NodeInfo{
			Kind: "Doc", Name: fmt.Sprintf("filler%d", i), FilePath: fmt.Sprintf("filler%d.md", i), Content: content,
		})
	}

	g := graph.New()
	g.Build(graph.RepoGraph{Repo: "t", Nodes: nodes})

	emb := &statsStubEmbedder{vecByText: vecByText, queryVec: q}
	ix := New(g, emb, zerolog.Nop())
	if err := ix.Build(context.Background()); err != nil {
		t.Fatalf("Build: %v", err)
	}
	// Force the exact scan so the semantic ranking (hence which nodes survive
	// the top-20 over-fetch) is deterministic, not an HNSW approximation.
	ix.SetBruteForce(true)

	ctx := context.Background()
	totalHits := 0
	search := func(query string, k int) []Hit {
		t.Helper()
		hits, err := ix.Search(ctx, query, nil, "", Select{}, k)
		if err != nil {
			t.Fatalf("Search(%q): %v", query, err)
		}
		totalHits += len(hits)
		return hits
	}
	contains := func(hits []Hit, qn string) bool {
		for _, h := range hits {
			if h.QN == qn {
				return true
			}
		}
		return false
	}

	// Call 1: keyword promotion. "C"'s invented tokens have zero accidental
	// overlap with any filler's content, and its vector is the worst in the
	// graph, so it can only surface via the lexical channel.
	hits1 := search("zzflurbnix quorlex", 5)
	if len(hits1) != 5 {
		t.Fatalf("call1: want 5 hits, got %d: %+v", len(hits1), hits1)
	}
	if !contains(hits1, "t//c.md") {
		t.Errorf("call1: expected the keyword-promoted node t//c.md in results: %+v", hits1)
	}

	// Call 2: an all-stopword query — the lexical channel must abstain
	// entirely, so every returned hit is semantic-only by construction.
	hits2 := search("what is this", 5)
	if len(hits2) != 5 {
		t.Fatalf("call2: want 5 hits, got %d: %+v", len(hits2), hits2)
	}

	// Call 3: both channels agree on filler0 — it is both the best semantic
	// match (vector identical to the query) and the only lexical match for
	// these invented tokens.
	hits3 := search("ubjello vexnorak", 5)
	if len(hits3) != 5 {
		t.Fatalf("call3: want 5 hits, got %d: %+v", len(hits3), hits3)
	}
	if !contains(hits3, "t//filler0.md") {
		t.Errorf("call3: expected the both-channel node t//filler0.md in results: %+v", hits3)
	}

	snap := ix.Stats().Snapshot()
	if snap.Searches != 3 {
		t.Errorf("Searches = %d, want 3", snap.Searches)
	}
	if snap.SemanticCalls != 3 {
		t.Errorf("SemanticCalls = %d, want 3", snap.SemanticCalls)
	}
	if snap.LexicalAbstentions != 1 {
		t.Errorf("LexicalAbstentions = %d, want 1 (call2 only)", snap.LexicalAbstentions)
	}
	if snap.KeywordPromotions < 1 {
		t.Errorf("expected at least 1 keyword promotion (call1's C), got %d", snap.KeywordPromotions)
	}
	if snap.BothChannelHits < 1 {
		t.Errorf("expected at least 1 both-channel hit (call3's filler0), got %d", snap.BothChannelHits)
	}
	if snap.SemanticOnlyHits < 1 {
		t.Errorf("expected at least 1 semantic-only hit (the filler nodes), got %d", snap.SemanticOnlyHits)
	}

	// The identity that makes the promotion count trustworthy: the three
	// buckets must exactly partition every hit ever returned, cumulatively
	// across all three (differently-shaped) searches above.
	sum := snap.KeywordPromotions + snap.SemanticOnlyHits + snap.BothChannelHits
	if sum != int64(totalHits) {
		t.Errorf("partition identity broken: promotions(%d) + semanticOnly(%d) + both(%d) = %d, want totalHits=%d",
			snap.KeywordPromotions, snap.SemanticOnlyHits, snap.BothChannelHits, sum, totalHits)
	}
}

// TestSearchStatsSharedAcrossUseStats verifies UseStats redirects an Index's
// recording to an externally-owned SearchStats — the mechanism
// engine.Registry relies on to keep counts cumulative across a project's
// evict/reload cycle (a fresh *Index is built on every reload; only an
// injected, externally-held SearchStats survives that).
func TestSearchStatsSharedAcrossUseStats(t *testing.T) {
	g := graph.New()
	g.Build(graph.RepoGraph{Repo: "t", Nodes: []parser.NodeInfo{
		{Kind: "Doc", Name: "a", FilePath: "a.md", Content: "alpha content"},
	}})
	// Dims must be a multiple of 4 — the vector library's SIMD cosine path
	// (used internally by the HNSW index this test deliberately exercises,
	// unlike TestSearchStatsPartitionInvariant which forces brute-force)
	// requires it.
	emb := &statsStubEmbedder{
		vecByText: map[string][]float32{"alpha content": {1, 0, 0, 0}},
		queryVec:  []float32{1, 0, 0, 0},
	}
	ix := New(g, emb, zerolog.Nop())
	if err := ix.Build(context.Background()); err != nil {
		t.Fatalf("Build: %v", err)
	}

	shared := NewSearchStats()
	ix.UseStats(shared)
	if ix.Stats() != shared {
		t.Fatalf("Stats() did not return the injected shared instance")
	}

	if _, err := ix.Search(context.Background(), "alpha", nil, "", Select{}, 5); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got := shared.Snapshot().Searches; got != 1 {
		t.Errorf("shared SearchStats.Searches = %d, want 1", got)
	}

	// A NEW Index (simulating a reload after eviction) given the SAME shared
	// instance must accumulate onto it rather than starting back at zero.
	ix2 := New(g, emb, zerolog.Nop())
	if err := ix2.Build(context.Background()); err != nil {
		t.Fatalf("Build (reload): %v", err)
	}
	ix2.UseStats(shared)
	if _, err := ix2.Search(context.Background(), "alpha", nil, "", Select{}, 5); err != nil {
		t.Fatalf("Search (reload): %v", err)
	}
	if got := shared.Snapshot().Searches; got != 2 {
		t.Errorf("shared SearchStats.Searches after reload = %d, want 2 (cumulative)", got)
	}
}

// TestParseAttrFilter covers the three shapes a caller can send: absent (match
// everything), well-formed, and malformed. Malformed must ERROR rather than
// degrade to the zero filter — a typo'd filter that silently matched every node
// would widen a search the caller asked to narrow, and the caller would read the
// extra hits as real.
func TestParseAttrFilter(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    AttrFilter
		wantErr bool
	}{
		{in: "", want: AttrFilter{}},
		{in: "team=infra", want: AttrFilter{Key: "team", Value: "infra"}},
		{in: "tier=", want: AttrFilter{Key: "tier"}}, // empty value is a real filter: matches only nodes whose attr is ""
		{in: "team", wantErr: true},
		{in: "=infra", wantErr: true},
	} {
		got, err := ParseAttrFilter(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseAttrFilter(%q): want error, got %+v", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseAttrFilter(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseAttrFilter(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
	if !(AttrFilter{}).Empty() {
		t.Error("zero AttrFilter must report Empty")
	}
	if (AttrFilter{Key: "k"}).Empty() {
		t.Error("filter with a key must not report Empty")
	}
}

// TestSearchAttrFilter proves the filter narrows results to nodes carrying the
// exact attribute, and that a node missing the key is excluded rather than
// treated as a wildcard.
func TestSearchAttrFilter(t *testing.T) {
	q := []float32{1, 0, 0, 0, 0, 0, 0, 0}
	vecByText := map[string][]float32{}
	var nodes []parser.NodeInfo

	// Distinct vectors, all positively aligned with q, so every node is a
	// plausible semantic hit and only the attr filter decides who survives.
	add := func(i int, name string, attrs map[string]string) {
		content := fmt.Sprintf("person %s in the roster", name)
		vecByText[content] = []float32{1, float32(i) / 100, 0, 0, 0, 0, 0, 0}
		nodes = append(nodes, parser.NodeInfo{
			Kind: "Doc", Name: name, FilePath: name + ".md", Content: content, Attrs: attrs,
		})
	}
	add(1, "ann", map[string]string{"team": "infra"})
	add(2, "bob", map[string]string{"team": "infra"})
	add(3, "cat", map[string]string{"team": "apps"})
	add(4, "dan", nil) // carries no attributes at all

	g := graph.New()
	g.Build(graph.RepoGraph{Repo: "t", Nodes: nodes})
	ix := New(g, &statsStubEmbedder{vecByText: vecByText, queryVec: q}, zerolog.Nop())
	if err := ix.Build(context.Background()); err != nil {
		t.Fatalf("Build: %v", err)
	}
	ix.SetBruteForce(true)

	names := func(f AttrFilter) []string {
		t.Helper()
		hits, err := ix.Search(context.Background(), "person roster", nil, "", Select{Attr: f}, 10)
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		out := make([]string, 0, len(hits))
		for _, h := range hits {
			out = append(out, h.Title)
		}
		sort.Strings(out)
		return out
	}

	if got := names(AttrFilter{}); len(got) != 4 {
		t.Errorf("no filter: got %v, want all 4", got)
	}
	if got := names(AttrFilter{Key: "team", Value: "infra"}); len(got) != 2 || got[0] != "ann" || got[1] != "bob" {
		t.Errorf("team=infra: got %v, want [ann bob]", got)
	}
	if got := names(AttrFilter{Key: "team", Value: "apps"}); len(got) != 1 || got[0] != "cat" {
		t.Errorf("team=apps: got %v, want [cat]", got)
	}
	// "dan" has no attrs: a missing key reads as "", so only an explicitly empty
	// value matches it — a filter on any other value must exclude it.
	if got := names(AttrFilter{Key: "team", Value: ""}); len(got) != 1 || got[0] != "dan" {
		t.Errorf(`team="" should match only the attribute-less node, got %v`, got)
	}
	if got := names(AttrFilter{Key: "nosuch", Value: "x"}); len(got) != 0 {
		t.Errorf("unknown key: got %v, want none", got)
	}
}

// TestCutAtLargestGap covers the shapes a ranked list arrives in: a clear
// single winner, a plateau of equals, and lists too short to have a gap.
func TestCutAtLargestGap(t *testing.T) {
	mk := func(scores ...float32) []Hit {
		hs := make([]Hit, len(scores))
		for i, s := range scores {
			hs[i] = Hit{QN: fmt.Sprintf("n%d", i), Score: s}
		}
		return hs
	}
	for _, tc := range []struct {
		name string
		in   []Hit
		want int
	}{
		{"nil", nil, 0},
		{"single hit has no gap", mk(0.6), 1},
		// One right answer then a cliff: the classic verbatim shape.
		{"one winner", mk(0.64, 0.52, 0.51, 0.50, 0.49), 1},
		// A plateau then a cliff: the attribute shape, where several answers are
		// genuinely right and the cut must keep all of them.
		{"plateau then cliff", mk(0.68, 0.67, 0.67, 0.66, 0.40, 0.39), 4},
		// Steepest drop last — nothing is cut off the front.
		{"gap at the end", mk(0.60, 0.59, 0.58, 0.20), 3},
		// A uniform list has no cliff. Every gap is equal, so the earliest wins
		// and the answer does not wobble with float32 rounding.
		{"even decline", mk(0.50, 0.40, 0.30, 0.20), 1},
	} {
		if got := len(CutAtLargestGap(tc.in)); got != tc.want {
			t.Errorf("%s: kept %d, want %d", tc.name, got, tc.want)
		}
	}
	// The kept prefix must stay in rank order and keep its scores intact.
	in := mk(0.9, 0.88, 0.3)
	got := CutAtLargestGap(in)
	if len(got) != 2 || got[0].Score != 0.9 || got[1].Score != 0.88 {
		t.Errorf("cut altered the surviving prefix: %+v", got)
	}
}
