package index

import (
	"context"
	"hash/fnv"
	"math"
	"testing"

	"github.com/rs/zerolog"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/graph"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/parser"
)

// hashEmbedder derives a deterministic unit vector from the text itself, so a
// fixture needs no per-text vector table — these tests care which nodes are
// SELECTED, not which vector each one gets.
type hashEmbedder struct{ dims int }

func (e *hashEmbedder) vec(text string) []float32 {
	v := make([]float32, e.dims)
	var norm float64
	for i := range v {
		h := fnv.New32a()
		_, _ = h.Write([]byte(text))
		_, _ = h.Write([]byte{byte(i)})
		v[i] = float32(h.Sum32()%1000)/1000 - 0.5
		norm += float64(v[i]) * float64(v[i])
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		v[0] = 1
		return v
	}
	for i := range v {
		v[i] = float32(float64(v[i]) / norm)
	}
	return v
}

func (e *hashEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = e.vec(t)
	}
	return out, nil
}

func (e *hashEmbedder) EmbedQuery(_ context.Context, text string) ([]float32, error) {
	return e.vec(text), nil
}

// attrCorpus is a catalog map's shape: entries carrying properties, each with a
// child that carries none, so attribute-less nodes outnumber the ones that match
// — the condition under which a post-filter under-returns.
func attrCorpus(t *testing.T, entries int) *Index {
	t.Helper()
	var nodes []parser.NodeInfo
	for i := 0; i < entries; i++ {
		tag := "no"
		if i%3 == 0 {
			tag = "yes"
		}
		nodes = append(nodes,
			parser.NodeInfo{Kind: "Entry", Name: nameOf(i), FilePath: "c.json",
				Content: "entry " + nameOf(i), Attrs: map[string]string{"gitops": tag}},
			parser.NodeInfo{Kind: "Field", Name: "note", ParentName: nameOf(i), FilePath: "c.json",
				Content: "a shared preamble that every field repeats, for " + nameOf(i)},
			parser.NodeInfo{Kind: "Field", Name: "other", ParentName: nameOf(i), FilePath: "c.json",
				Content: "a shared preamble that every field repeats, for " + nameOf(i)},
		)
	}
	g := graph.New()
	g.Build(graph.RepoGraph{Repo: "m", Nodes: nodes, Edges: nil})

	ix := New(g, &hashEmbedder{dims: 8}, zerolog.Nop())
	ix.SetBruteForce(true)
	if err := ix.Build(context.Background()); err != nil {
		t.Fatalf("Build: %v", err)
	}
	return ix
}

func nameOf(i int) string { return string(rune('a'+i/26)) + string(rune('a'+i%26)) }

// An attribute names a set, so every member of it must be reachable — not only
// the members the query's own similarity happened to retrieve.
func TestSearch_AttrSelectsEveryMatchingEntry(t *testing.T) {
	const entries = 30
	ix := attrCorpus(t, entries)
	want := 0
	for i := 0; i < entries; i++ {
		if i%3 == 0 {
			want++
		}
	}

	hits, err := ix.Search(context.Background(), "entry", nil, "", Select{Attr: AttrFilter{Key: "gitops", Value: "yes"}}, want)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != want {
		t.Fatalf("attr filter returned %d of %d matching entries", len(hits), want)
	}
	for _, h := range hits {
		n, ok := ix.g.Get(h.QN)
		if !ok || n.Attrs["gitops"] != "yes" {
			t.Fatalf("hit %q does not carry the selected attribute", h.QN)
		}
	}
}

// A selected set is still ranked, and still capped by k.
func TestSearch_AttrSelectionIsRankedAndCapped(t *testing.T) {
	ix := attrCorpus(t, 30)
	hits, err := ix.Search(context.Background(), "entry ab", nil, "", Select{Attr: AttrFilter{Key: "gitops", Value: "yes"}}, 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("k must cap a selection: got %d hits, want 3", len(hits))
	}
	for i := 1; i < len(hits); i++ {
		if hits[i-1].Score < hits[i].Score {
			t.Fatalf("selection is not ranked by similarity: %v", []float32{hits[i-1].Score, hits[i].Score})
		}
		if hits[i-1].Fused <= hits[i].Fused {
			t.Fatalf("selection must stamp a descending fused score for cross-map merging")
		}
	}
}

// An attribute that nothing carries selects nothing — it must not fall back to
// returning whatever the query matched.
func TestSearch_AttrWithNoMatchesReturnsNothing(t *testing.T) {
	ix := attrCorpus(t, 10)
	hits, err := ix.Search(context.Background(), "entry", nil, "", Select{Attr: AttrFilter{Key: "gitops", Value: "maybe"}}, 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("an unmatched attribute must select nothing, got %d hits", len(hits))
	}
}

// Kinds narrow a selection; they do not widen it.
func TestSearch_AttrAndKindCompose(t *testing.T) {
	ix := attrCorpus(t, 30)
	hits, err := ix.Search(context.Background(), "entry", map[string]bool{"Field": true}, "",
		Select{Attr: AttrFilter{Key: "gitops", Value: "yes"}}, 20)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("Field nodes carry no attributes, so the composition selects nothing; got %d", len(hits))
	}
}

// A caller that names a set needs the set's real size, independent of the reply
// cap — otherwise "these are all of them" and "these are the first few" look
// identical and it reasons over a partial set as if it were whole.
func TestCountMatching_ReportsTheWholeSetRegardlessOfLimit(t *testing.T) {
	const entries = 30
	ix := attrCorpus(t, entries)
	want := 0
	for i := 0; i < entries; i++ {
		if i%3 == 0 {
			want++
		}
	}
	attr := Select{Attr: AttrFilter{Key: "gitops", Value: "yes"}}

	if got := ix.CountMatching(nil, "", attr); got != want {
		t.Fatalf("CountMatching = %d, want %d", got, want)
	}
	// The count must not move with the reply cap.
	hits, err := ix.Search(context.Background(), "entry", nil, "", attr, 2)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("limit not applied: got %d hits", len(hits))
	}
	if got := ix.CountMatching(nil, "", attr); got != want {
		t.Fatalf("CountMatching changed with limit: %d, want %d", got, want)
	}
	// And it must respect the same predicate the search used.
	if got := ix.CountMatching(map[string]bool{"Field": true}, "", attr); got != 0 {
		t.Fatalf("CountMatching ignored kinds: got %d, want 0", got)
	}
}

// A caller naming a KIND names a set the same way an attribute does: the map's
// own vocabulary, not the code/docs grouping a tool searches by default. Without
// it, a kind that outnumbers the one asked for wins every generic query and the
// wanted entries never surface.
func TestSearch_KindSelectsTheMapsOwnVocabulary(t *testing.T) {
	const entries = 30
	ix := attrCorpus(t, entries) // entries carry attrs; each has 2 attribute-less fields

	hits, err := ix.Search(context.Background(), "a shared preamble that every field repeats",
		nil, "", Select{Kinds: map[string]bool{"Entry": true}}, entries)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != entries {
		t.Fatalf("kind selection returned %d of %d entries", len(hits), entries)
	}
	for _, h := range hits {
		if h.Kind != "Entry" {
			t.Fatalf("selection leaked a %s node: %s", h.Kind, h.QN)
		}
	}
	// The query deliberately matches the FIELD text, which outnumbers entries
	// 2:1 — without selection those fields would fill the whole reply.
	plain, err := ix.Search(context.Background(), "a shared preamble that every field repeats",
		nil, "", Select{}, 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	fields := 0
	for _, h := range plain {
		if h.Kind == "Field" {
			fields++
		}
	}
	if fields == 0 {
		t.Fatalf("fixture is wrong: the unselected query should be dominated by Field nodes")
	}
}

// Kind and attribute compose: both must hold.
func TestSearch_KindAndAttrSelectTheIntersection(t *testing.T) {
	ix := attrCorpus(t, 30)
	sel := Select{Kinds: map[string]bool{"Entry": true}, Attr: AttrFilter{Key: "gitops", Value: "yes"}}

	hits, err := ix.Search(context.Background(), "entry", nil, "", sel, 100)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	want := ix.CountMatching(nil, "", sel)
	if want == 0 || len(hits) != want {
		t.Fatalf("intersection returned %d, CountMatching says %d", len(hits), want)
	}
	for _, h := range hits {
		n, _ := ix.g.Get(h.QN)
		if n.Kind != "Entry" || n.Attrs["gitops"] != "yes" {
			t.Fatalf("hit %q is outside the intersection", h.QN)
		}
	}
	// A kind that exists with an attribute that does not selects nothing.
	none, err := ix.Search(context.Background(), "entry", nil, "",
		Select{Kinds: map[string]bool{"Field": true}, Attr: AttrFilter{Key: "gitops", Value: "yes"}}, 100)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("Field nodes carry no attributes, so the intersection is empty; got %d", len(none))
	}
}
