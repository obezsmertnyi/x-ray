package mcpserver

import (
	"testing"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/index"
)

// A keyword-promoted hit tops its map's fused list while carrying a LOW cosine —
// that is the point of promoting it. Re-sorting the merged page by cosine buries
// exactly those hits.
func TestSortHitsByFused_KeepsAKeywordPromotionOnTop(t *testing.T) {
	hits := []index.Hit{
		// Worst cosine on the page, best fused score.
		{QN: "r//c.json::ravenlock", Title: "ravenlock", Score: 0.3342, Fused: 0.0333},
		// A neighbour the embedding liked.
		{QN: "r//c.json::RavDP1987", Title: "RavDP1987", Score: 0.5447, Fused: 0.0167},
		{QN: "r//c.json::filler7", Title: "filler7", Score: 0.3665, Fused: 0.0164},
	}
	sortHitsByFused(hits)
	if hits[0].Title != "ravenlock" {
		got := make([]string, len(hits))
		for i, h := range hits {
			got[i] = h.Title
		}
		t.Errorf("merge reordered a fused list by cosine: got %v, want ravenlock first", got)
	}
}

// Equal fused scores keep the order the map delivered them in. A tiebreak on
// cosine would reorder a map's own list — the very thing the merge must not do —
// and the input order is already deterministic, because it is the order that
// map's Search produced.
func TestSortHitsByFused_TiesKeepDeliveredOrder(t *testing.T) {
	mk := func() []index.Hit {
		return []index.Hit{
			{QN: "b//x::n", Title: "b", Score: 0.5, Fused: 0.02},
			{QN: "a//x::n", Title: "a", Score: 0.5, Fused: 0.02},
			{QN: "c//x::n", Title: "c", Score: 0.9, Fused: 0.02},
		}
	}
	first := mk()
	sortHitsByFused(first)
	for i, want := range []string{"b", "a", "c"} {
		if first[i].Title != want {
			t.Fatalf("tie at %d reordered the delivered list: got %q, want %q", i, first[i].Title, want)
		}
	}
	second := mk()
	sortHitsByFused(second)
	for i := range first {
		if first[i].QN != second[i].QN {
			t.Fatalf("sort is not deterministic at %d: %q vs %q", i, first[i].QN, second[i].QN)
		}
	}
}

// Every map ranks its own hits, so each map's rank-1 hit carries the same fused
// score. Merging on that alone round-robins the maps: an unrelated map's best
// hit lands above a well-matched map's second. weighByRoute makes the scores
// comparable by scaling each map's list by how well that map matched the route.
func TestWeighByRoute_KeepsAWellMatchedMapAhead(t *testing.T) {
	// The named map: rank scores as Index.Search stamps them, 1/(60+rank).
	named := []index.Hit{
		{QN: "team//c.json::a1", Title: "a1", Score: 0.742, Fused: 1.0 / 60},
		{QN: "team//c.json::a2", Title: "a2", Score: 0.731, Fused: 1.0 / 61},
		{QN: "team//c.json::a3", Title: "a3", Score: 0.668, Fused: 1.0 / 62},
	}
	// A map the router merely reached for, with the SAME internal rank scores.
	other := []index.Hit{
		{QN: "mobile//x.ts::b1", Title: "b1", Score: 0.529, Fused: 1.0 / 60},
		{QN: "mobile//x.ts::b2", Title: "b2", Score: 0.519, Fused: 1.0 / 61},
	}

	weighByRoute(named, 1.0)
	weighByRoute(other, 0.52)
	merged := append(append([]index.Hit{}, named...), other...)
	sortHitsByFused(merged)

	got := make([]string, len(merged))
	for i, h := range merged {
		got[i] = h.Title
	}
	for i, want := range []string{"a1", "a2", "a3", "b1", "b2"} {
		if got[i] != want {
			t.Fatalf("merge interleaved the maps: got %v, want the well-matched map first", got)
		}
	}
}

// Scaling must not reorder a map's own list, including a keyword-promoted hit
// that sits on top with the worst cosine on the page.
func TestWeighByRoute_PreservesAMapsOwnOrder(t *testing.T) {
	hits := []index.Hit{
		{QN: "r//c.json::ravenlock", Title: "ravenlock", Score: 0.3342, Fused: 0.0333},
		{QN: "r//c.json::RavDP1987", Title: "RavDP1987", Score: 0.5447, Fused: 0.0167},
		{QN: "r//c.json::filler7", Title: "filler7", Score: 0.3665, Fused: 0.0164},
	}
	weighByRoute(hits, 0.41)
	sortHitsByFused(hits)
	for i, want := range []string{"ravenlock", "RavDP1987", "filler7"} {
		if hits[i].Title != want {
			t.Fatalf("weighting reordered a map's own list at %d: got %q, want %q", i, hits[i].Title, want)
		}
	}
}
