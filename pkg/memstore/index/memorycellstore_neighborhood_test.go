package index

import (
	"testing"
)

// seedVec inserts a cell with a hand-crafted embedding so neighborhood
// geometry is fully deterministic (no embedder involved).
func seedVec(t *testing.T, store *CellStore, id, kind string, vec []float32) {
	t.Helper()
	store.SeedFromEmbedding(IndexEntry{ID: id, Kind: kind, Title: id, Status: "active"}, vec)
}

// TestNeighborhood_BasicGeometry pins the semantic-neighborhood contract:
// the center never appears among its own neighbors, neighbors are sorted
// most-similar-first, the keep filter excludes cells, an unknown id answers
// ok=false, and only a genuine sub-cluster produces a neighbor↔neighbor edge.
func TestNeighborhood_BasicGeometry(t *testing.T) {
	store := newRawStore(nil)
	seedVec(t, store, "center", "case", []float32{1, 0, 0, 0})
	seedVec(t, store, "near", "event", []float32{0.99, 0.14, 0, 0})
	seedVec(t, store, "cluster-a1", "event", []float32{0.9, 0.43, 0, 0})
	seedVec(t, store, "cluster-a2", "event", []float32{0.9, 0.44, 0, 0})
	seedVec(t, store, "far", "note", []float32{0, 1, 0, 0})

	nh, ok := store.Neighborhood("center", 10, 3, nil)
	if !ok {
		t.Fatal("Neighborhood must find an indexed id")
	}
	if nh.Center.ID != "center" {
		t.Fatalf("center = %q", nh.Center.ID)
	}
	if len(nh.Neighbors) != 4 {
		t.Fatalf("neighbors = %d, want 4", len(nh.Neighbors))
	}
	for i, n := range nh.Neighbors {
		if n.Document.ID == "center" {
			t.Fatal("center leaked into its own neighbors")
		}
		if i > 0 && n.Score > nh.Neighbors[i-1].Score {
			t.Fatalf("neighbors not sorted best-first: %+v", nh.Neighbors)
		}
	}
	if nh.Neighbors[0].Document.ID != "near" {
		t.Fatalf("best neighbor = %q, want near", nh.Neighbors[0].Document.ID)
	}

	// cluster-a1↔cluster-a2 are nearly identical (cos ≈ 1.0) and tighter to
	// each other than either is to the center (≈0.90), so they form the one
	// real sub-cluster. Every other pair is dominated by its own similarity
	// to the center and must not produce an edge.
	foundCluster := false
	for _, e := range nh.Edges {
		pair := e.A + "|" + e.B
		if pair == "cluster-a1|cluster-a2" || pair == "cluster-a2|cluster-a1" {
			foundCluster = true
			continue
		}
		t.Fatalf("unexpected edge above threshold: %+v", e)
	}
	if !foundCluster {
		t.Fatal("cluster-a1↔cluster-a2 edge missing")
	}

	// keep filter drops cells (grants/kind filtering hook).
	nh, ok = store.Neighborhood("center", 10, 3, func(c IndexEntry) bool { return c.Kind != "note" })
	if !ok || len(nh.Neighbors) != 3 {
		t.Fatalf("filtered neighbors = %d, want 3", len(nh.Neighbors))
	}

	// Unknown id → not indexed.
	if _, ok := store.Neighborhood("nope", 10, 3, nil); ok {
		t.Fatal("unknown id must answer ok=false")
	}
}

// unitAxis builds a dim-length vector that is `w` along axis 0 and `off`
// along a private axis, so a family of them share a controlled cosine to the
// center (axis 0) while staying a controlled distance from each other.
func unitAxis(dim, axis int, w, off float32) []float32 {
	v := make([]float32, dim)
	v[0] = w
	v[axis] = off
	return v
}

// clusterVec adds a component on a SHARED off-center axis (axis 1), which is
// what makes a family mutually tighter than any of them is to the center.
func clusterVec(dim, axis int, toCenter, shared, private float32) []float32 {
	v := make([]float32, dim)
	v[0] = toCenter
	v[1] = shared
	v[axis] = private
	return v
}

// TestNeighborhood_EdgesAreRelativeNotAbsolute is the regression for the
// miscalibration this replaced: an absolute cosine threshold.
// Real embeddings of same-kind cells crowd into a narrow high band — here
// every neighbor sits at cos 0.98 to the center and 0.96 to every other
// neighbor, which cleared the old 0.60 constant by a mile and rendered the
// COMPLETE graph (measured live: 190 of 190 pairs qualified). None of these
// cells is tighter to another than to the center, so the honest answer is a
// star: zero edges, at any absolute scale.
func TestNeighborhood_EdgesAreRelativeNotAbsolute(t *testing.T) {
	const dim = 8
	store := newRawStore(nil)
	center := make([]float32, dim)
	center[0] = 1
	seedVec(t, store, "center", "case", center)
	// cos(n_i, center) = 0.98; cos(n_i, n_j) = 0.98² ≈ 0.96 — dense, but no
	// pair beats its own pull toward the center.
	for i := 1; i <= 5; i++ {
		seedVec(t, store, "dense-"+string(rune('a'+i-1)), "event", unitAxis(dim, i, 0.98, 0.19899749))
	}

	nh, ok := store.Neighborhood("center", 10, 3, nil)
	if !ok {
		t.Fatal("center must be indexed")
	}
	if len(nh.Neighbors) != 5 {
		t.Fatalf("neighbors = %d, want 5", len(nh.Neighbors))
	}
	if len(nh.Edges) != 0 {
		t.Fatalf("uniformly dense neighborhood must yield no edges, got %d: %+v", len(nh.Edges), nh.Edges)
	}
}

// TestNeighborhood_PerNeighborEdgeCap pins the density bound: when a whole
// family DOES qualify (mutually far tighter than any of them is to the
// center), each neighbor contributes at most edgesPerNeighbor links rather
// than the complete graph, and edges come back strongest-first on the same
// [0,1] scale as neighbor scores.
func TestNeighborhood_PerNeighborEdgeCap(t *testing.T) {
	const dim = 8
	store := newRawStore(nil)
	center := make([]float32, dim)
	center[0] = 1
	seedVec(t, store, "center", "case", center)
	// cos(n_i, center) ≈ 0.45; cos(n_i, n_j) ≈ 0.99 — one tight cluster
	// hanging off a distant center, so every pair qualifies.
	const n = 5
	for i := 1; i <= n; i++ {
		seedVec(t, store, "tight-"+string(rune('a'+i-1)), "event", clusterVec(dim, i+1, 0.5, 1.0, 0.1))
	}

	nh, ok := store.Neighborhood("center", 10, 1, nil)
	if !ok {
		t.Fatal("center must be indexed")
	}
	if len(nh.Neighbors) != n {
		t.Fatalf("neighbors = %d, want %d", len(nh.Neighbors), n)
	}
	if len(nh.Edges) == 0 {
		t.Fatal("a genuine tight cluster must produce edges")
	}
	// Union of each node's single strongest link — never the complete graph.
	if maxEdges := n * (n - 1) / 2; len(nh.Edges) >= maxEdges {
		t.Fatalf("edges = %d, want fewer than the complete graph (%d)", len(nh.Edges), maxEdges)
	}
	if len(nh.Edges) > n {
		t.Fatalf("edges = %d, want at most perNode*n = %d", len(nh.Edges), n)
	}
	for i, e := range nh.Edges {
		if e.Score < 0 || e.Score > 1 {
			t.Fatalf("edge score %v outside the [0,1] neighbor-score scale: %+v", e.Score, e)
		}
		if i > 0 && e.Score > nh.Edges[i-1].Score {
			t.Fatalf("edges not sorted strongest-first: %+v", nh.Edges)
		}
	}
}
