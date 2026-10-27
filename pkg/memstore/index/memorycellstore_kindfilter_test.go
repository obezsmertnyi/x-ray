package index

import (
	"fmt"
	"testing"
)

// crowdedTestVec returns a dim-length vector near the shared query axis
// (axis 0), perturbed on a private axis so distinct calls produce distinct
// but near-identical-to-query vectors — the "populous kind" side of the
// crowding-out regression below.
func crowdedTestVec(dim, i int) []float32 {
	v := make([]float32, dim)
	v[0] = 1
	v[(i%(dim-1))+1] = 0.01
	return v
}

// farTestVec returns a dim-length vector orthogonal to the query axis,
// distinguished by index i — the "rare kind" side of the regression: cos ≈ 0
// to the query, so it never surfaces in a global top-K ranked by similarity
// to the query.
func farTestVec(dim, i int) []float32 {
	v := make([]float32, dim)
	v[(i%(dim-4))+4] = 1
	return v
}

// TestSearchByEmbedding_KindFilterExactScan is the regression for the "dark
// kinds" bug: CellStore.searchByEmbedding used to fetch topK*3 global
// HNSW neighbors and filter by kind afterward, so a kind that is a small
// minority of the corpus never appeared in the global top-fetchK for a
// typical query and a kind-filtered search for it returned nothing even
// though matching cells existed. Here 300 analytics_counter cells crowd
// every position near the query vector; 3 event cells sit far from it
// (orthogonal). A kindFilter="event" search must still return all 3 —
// exact-scanning the "event" population directly, never touching the HNSW
// index's global ranking.
func TestSearchByEmbedding_KindFilterExactScan(t *testing.T) {
	const dim = 256
	store := newRawStore(nil)

	for i := 0; i < 300; i++ {
		seedVec(t, store, fmt.Sprintf("analytics-%d", i), "analytics_counter", crowdedTestVec(dim, i))
	}
	wantIDs := map[string]bool{}
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("event-%d", i)
		seedVec(t, store, id, "event", farTestVec(dim, i))
		wantIDs[id] = true
	}

	query := make([]float32, dim)
	query[0] = 1

	results := store.searchAll(query, 5, "event")
	if len(results) != 3 {
		t.Fatalf("expected all 3 event cells, got %d: %+v", len(results), results)
	}
	got := map[string]bool{}
	for _, r := range results {
		if r.Document.Kind != "event" {
			t.Fatalf("kind-filtered search returned a non-event cell: %+v", r.Document)
		}
		got[r.Document.ID] = true
	}
	for id := range wantIDs {
		if !got[id] {
			t.Fatalf("expected event cell %s missing from results: %+v", id, results)
		}
	}
}

// TestSearchByEmbedding_ScanScoreMatchesIndexScore pins score compatibility
// between the two searchByEmbedding paths: the exact-scan branch (kindFilter
// != "") must report the IDENTICAL Score the HNSW branch (kindFilter == "")
// would have reported for the same (query, cell) pair, since downstream RRF
// fusion and eval score comparisons assume one consistent scale regardless
// of which branch answered.
func TestSearchByEmbedding_ScanScoreMatchesIndexScore(t *testing.T) {
	store := newRawStore(nil)

	seedVec(t, store, "top-event", "event", []float32{0.99, 0.14, 0, 0})
	seedVec(t, store, "other-fact", "stable_fact", []float32{0.5, 0.86, 0, 0})
	seedVec(t, store, "other-note", "note", []float32{0, 1, 0, 0})

	query := []float32{1, 0, 0, 0}

	unfiltered := store.searchAll(query, 5, "")
	if len(unfiltered) == 0 || unfiltered[0].Document.ID != "top-event" {
		t.Fatalf("expected top-event as the unfiltered (HNSW) top hit, got %+v", unfiltered)
	}

	filtered := store.searchAll(query, 5, "event")
	if len(filtered) != 1 || filtered[0].Document.ID != "top-event" {
		t.Fatalf("expected exactly top-event from the kind-filtered scan, got %+v", filtered)
	}

	if unfiltered[0].Score != filtered[0].Score {
		t.Fatalf("index-path score %v != scan-path score %v for the same cell — kind-filtered scan must match the HNSW score exactly",
			unfiltered[0].Score, filtered[0].Score)
	}
}
