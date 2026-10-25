package vectorstore

import (
	"math"
	"sort"

	"github.com/kshard/vector"
)

// CellNeighborhood is the semantic neighbourhood of one cell: its nearest
// neighbours and the links among them that mark sub-clusters.
type CellNeighborhood struct {
	Center    MemoryCellIndexEntry
	Neighbors []CellSearchResult
	Edges     []NeighborEdge
}

// NeighborEdge is one neighbour-to-neighbour similarity link. Score uses the
// same [0,1] mapping as CellSearchResult.Score, so an edge and a neighbour score
// compare directly.
type NeighborEdge struct {
	A, B  string
	Score float32
}

// simScore maps raw cosine [-1,1] onto the [0,1] scale searches report
// (1 - dist/2, i.e. (1+cos)/2), so every Score in a CellNeighborhood is on one
// scale.
func simScore(cos float32) float32 { return (1 + cos) / 2 }

// cosineSim is plain cosine similarity. Stored embeddings are not guaranteed
// unit-norm, so both norms are computed.
func cosineSim(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(na) * math.Sqrt(nb)))
}

// Neighborhood returns the semantic neighbourhood of the cell with the given id,
// searched in the index of the subject that holds it: subjects never share a
// graph. The second result is false when the id has no embedding in any index
// (never seeded, private, or since deleted), so a caller can tell "not indexed"
// from "no neighbours". keep is an optional per-cell filter; nil keeps all.
//
// Neighbour-to-neighbour edges follow a scale-free rule, not an absolute
// similarity threshold: an edge survives only when the two cells are more
// similar to each other than either is to the centre, and then only if it is
// among each endpoint's edgesPerNeighbor strongest such links (symmetrised, the
// union, so a hub may end up with a higher degree). Embeddings of same-kind
// cells crowd into a narrow high band, so any fixed threshold either admits the
// complete graph or is retuned per corpus and per embedder. "Tighter to each
// other than to the centre" is what sub-cluster means and holds at any scale. A
// neighbourhood with no sub-structure returns no edges: a star.
func (s *MemoryCellStore) Neighborhood(id string, topK, edgesPerNeighbor int, keep func(MemoryCellIndexEntry) bool) (*CellNeighborhood, bool) {
	if topK <= 0 {
		topK = 20
	}
	if edgesPerNeighbor <= 0 {
		edgesPerNeighbor = 3
	}
	for _, idx := range s.snapshotSubjects() {
		idx.freshen()
		idx.mu.RLock()
		center, ok := idx.entries[id]
		if !ok {
			idx.mu.RUnlock()
			continue
		}
		if len(center.Embedding) == 0 {
			idx.mu.RUnlock()
			return nil, false
		}
		// Over-fetch: the centre itself and any filtered candidates must not
		// leave fewer than topK visible neighbours.
		q := cellNodeEmbedding{Vec: vector.F32(center.Embedding)}
		found := idx.index.Search(q, topK*2+1, 100)
		nh := &CellNeighborhood{Center: center.Cell}
		picked := make([]cellEntry, 0, topK)
		for _, nb := range found {
			if nb.Key == id {
				continue
			}
			entry, ok := idx.entries[nb.Key]
			if !ok {
				continue
			}
			if keep != nil && !keep(entry.Cell) {
				continue
			}
			dist := idx.index.Distance(q, nb)
			nh.Neighbors = append(nh.Neighbors, CellSearchResult{Entry: entry.Cell, Score: 1.0 - dist/2.0})
			picked = append(picked, entry)
			if len(picked) >= topK {
				break
			}
		}
		idx.mu.RUnlock()
		sort.Slice(nh.Neighbors, func(i, j int) bool { return nh.Neighbors[i].Score > nh.Neighbors[j].Score })
		nh.Edges = clusterEdges(center.Embedding, picked, edgesPerNeighbor)
		return nh, true
	}
	return nil, false
}

// clusterEdges keeps a neighbour pair only when the two are tighter to each
// other than to the centre, then each endpoint's strongest perNode such links,
// and returns their union strongest first. O(K²·dim), trivial at the caps the
// caller uses.
func clusterEdges(centerVec []float32, picked []cellEntry, perNode int) []NeighborEdge {
	n := len(picked)
	if n < 2 {
		return nil
	}
	// Each neighbour's own similarity to the centre is the bar its pairs must
	// clear, computed directly so the comparison never depends on the graph's
	// distance mapping.
	centerCos := make([]float32, n)
	for i := range picked {
		centerCos[i] = cosineSim(centerVec, picked[i].Embedding)
	}

	type cand struct {
		i, j int
		cos  float32
	}
	best := make([][]cand, n)
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			cos := cosineSim(picked[i].Embedding, picked[j].Embedding)
			bar := centerCos[i]
			if centerCos[j] > bar {
				bar = centerCos[j]
			}
			if cos <= bar {
				continue
			}
			best[i] = append(best[i], cand{i, j, cos})
			best[j] = append(best[j], cand{i, j, cos})
		}
	}

	seen := make(map[[2]int]bool)
	var edges []NeighborEdge
	for i := range best {
		sort.Slice(best[i], func(a, b int) bool { return best[i][a].cos > best[i][b].cos })
		for k, c := range best[i] {
			if k >= perNode {
				break
			}
			key := [2]int{c.i, c.j}
			if seen[key] {
				continue
			}
			seen[key] = true
			edges = append(edges, NeighborEdge{A: picked[c.i].Cell.ID, B: picked[c.j].Cell.ID, Score: simScore(c.cos)})
		}
	}
	sort.Slice(edges, func(a, b int) bool { return edges[a].Score > edges[b].Score })
	return edges
}
