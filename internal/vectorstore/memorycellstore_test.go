package vectorstore

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/fogfish/hnsw"
	"github.com/kshard/vector"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const benchDims = 128 // matches production gemini-embedding-001 MRL dims

// randVec returns a random unit vector of length dims.
func randVec(rng *rand.Rand, dims int) []float32 {
	v := make([]float32, dims)
	var norm float64
	for i := range v {
		f := rng.Float64()*2 - 1
		v[i] = float32(f)
		norm += f * f
	}
	norm = math.Sqrt(norm)
	for i := range v {
		v[i] /= float32(norm)
	}
	return v
}

// buildIndex seeds n docs with random 128-dim unit vectors into a single subject index.
func buildIndex(n int) *subjectIndex {
	rng := rand.New(rand.NewSource(42))
	idx := newSubjectIndex()
	for i := range n {
		doc := MemoryCellIndexEntry{ID: fmt.Sprintf("id-%d", i), Kind: "stable_fact", Status: "active"}
		idx.insert(doc, randVec(rng, benchDims))
	}
	return idx
}

// vec builds a float32 slice padded to the nearest multiple of 4 (required by
// the HNSW cosine kernel which uses SIMD-aligned arithmetic).
func vec(vals ...float32) []float32 {
	for len(vals)%4 != 0 {
		vals = append(vals, 0)
	}
	return vals
}

// seedDoc is a helper to insert a doc with a pre-built embedding.
func seedDoc(t *testing.T, s *MemoryCellStore, id, subject, kind, title string, embedding []float32) {
	t.Helper()
	doc := MemoryCellIndexEntry{
		ID:      id,
		Subject: subject,
		Kind:    kind,
		Title:   title,
		Status:  "active",
	}
	s.SeedFromEmbedding(doc, embedding)
}

// ── cellEmbedText ─────────────────────────────────────────────────────────────

func TestCellEmbedText(t *testing.T) {
	doc := MemoryCellIndexEntry{Kind: "stable_fact", Scope: "public", Subject: "go", Title: "Go routines", Content: "channels and goroutines"}
	assert.Equal(t, "stable_fact public go Go routines channels and goroutines", cellEmbedText(doc))
}

func TestCellEmbedText_OmitsEmptyScopeAndSubject(t *testing.T) {
	doc := MemoryCellIndexEntry{Kind: "note", Title: "T", Content: "C"}
	assert.Equal(t, "note T C", cellEmbedText(doc))
}

func TestCellEmbedText_CapsLongTextOnARuneBoundary(t *testing.T) {
	long := strings.Repeat("é", maxCellEmbedChars) // two bytes a rune
	got := cellEmbedText(MemoryCellIndexEntry{Kind: "note", Title: "T", Content: long})
	assert.LessOrEqual(t, len(got), maxCellEmbedChars)
	assert.True(t, utf8.ValidString(got), "the cut must not split a rune")
	assert.True(t, strings.HasPrefix(got, "note T "), "the head is what is kept")
}

// ── newSubjectIndex / basic insert ────────────────────────────────────────────

func TestSubjectIndex_InsertAndSearch(t *testing.T) {
	idx := newSubjectIndex()

	e1 := vec(1, 0, 0)
	e2 := vec(0, 1, 0)

	idx.insert(MemoryCellIndexEntry{ID: "a", Kind: "stable_fact"}, e1)
	idx.insert(MemoryCellIndexEntry{ID: "b", Kind: "recent_change"}, e2)

	// Query close to e1 — "a" should rank first.
	results := idx.search(vec(0.99, 0.01, 0), 5, "")
	require.NotEmpty(t, results)
	assert.Equal(t, "a", results[0].Entry.ID)
}

func TestApplyScoreGap(t *testing.T) {
	// applyScoreGap trims results that fall more than scoreGap (absolute cosine)
	// below the top. Pruning lives at the store level now (measured against the
	// global top), not per-subject — see TestSubjectIndex_NoPruning.
	s := NewMemoryCellStore(nil, zerolog.Nop()) // default gap = 0.02
	in := []CellSearchResult{
		{Entry: MemoryCellIndexEntry{ID: "top"}, Score: 0.92},
		{Entry: MemoryCellIndexEntry{ID: "near"}, Score: 0.905}, // 0.015 below → kept
		{Entry: MemoryCellIndexEntry{ID: "far"}, Score: 0.88},   // 0.04 below → cut
		{Entry: MemoryCellIndexEntry{ID: "outlier"}, Score: 0.5},
	}
	out := s.applyScoreGap(in)
	ids := make([]string, len(out))
	for i, r := range out {
		ids[i] = r.Entry.ID
	}
	assert.Equal(t, []string{"top", "near"}, ids, "keep only items within the absolute gap of the top")

	// gap=0 disables the cutoff (raw top-K passes through).
	s.SetScoreGap(0)
	assert.Len(t, s.applyScoreGap(in), 4, "gap=0 returns all results")

	// A single result is always returned unchanged.
	s.SetScoreGap(0.02)
	one := []CellSearchResult{{Entry: MemoryCellIndexEntry{ID: "only"}, Score: 0.4}}
	assert.Len(t, s.applyScoreGap(one), 1)
}

func TestSubjectIndex_NoPruning(t *testing.T) {
	// subjectIndex.search no longer prunes — it returns the raw top-K by score so
	// the cross-subject merge can apply the relevance cutoff against the global top.
	idx := newSubjectIndex()
	for i := range 10 {
		idx.insert(MemoryCellIndexEntry{ID: string(rune('a' + i))}, vec(1, float32(i)*0.001, 0, 0))
	}
	idx.insert(MemoryCellIndexEntry{ID: "outlier"}, vec(0, 1, 0, 0)) // orthogonal, low score

	results := idx.search(vec(1, 0, 0, 0), 20, "")

	// The outlier is NOT pruned here (pruning moved to applyScoreGap); it ranks last.
	require.NotEmpty(t, results)
	var hasOutlier bool
	for _, r := range results {
		if r.Entry.ID == "outlier" {
			hasOutlier = true
		}
	}
	assert.True(t, hasOutlier, "subjectIndex.search returns the outlier unpruned (cutoff is applied at the merge)")
	assert.Equal(t, "outlier", results[len(results)-1].Entry.ID, "outlier ranks last by score")
}

func TestSubjectIndex_SearchEmpty(t *testing.T) {
	idx := newSubjectIndex()
	results := idx.search(vec(1, 0, 0), 5, "")
	assert.Empty(t, results)
}

func TestSubjectIndex_KindFilter(t *testing.T) {
	idx := newSubjectIndex()
	// Insert many stable_fact docs close to the query so at least one survives
	// HNSW retrieval even with the 97%-score filter. recent_change docs are mixed in.
	for i := range 8 {
		kind := "stable_fact"
		if i%2 == 1 {
			kind = "recent_change"
		}
		v := vec(1, float32(i)*0.001, 0, 0)
		idx.insert(MemoryCellIndexEntry{ID: string(rune('a' + i)), Kind: kind}, v)
	}

	results := idx.search(vec(1, 0, 0, 0), 10, "stable_fact")
	// All returned results must match the requested kind.
	for _, r := range results {
		assert.Equal(t, "stable_fact", r.Entry.Kind)
	}
	// At least one stable_fact doc should be returned.
	assert.NotEmpty(t, results)
}

// ── NewMemoryCellStore ────────────────────────────────────────────────────────

func TestNewMemoryCellStore(t *testing.T) {
	s := NewMemoryCellStore(nil, zerolog.Nop())
	assert.NotNil(t, s)
	assert.Equal(t, int64(0), s.CollectionSize())
}

// ── SeedFromEmbedding ────────────────────────────────────────────────────────

func TestMemoryCellStore_SeedFromEmbedding(t *testing.T) {
	s := NewMemoryCellStore(nil, zerolog.Nop())

	seedDoc(t, s, "doc1", "alice", "stable_fact", "Title A", vec(1, 0, 0))
	seedDoc(t, s, "doc2", "alice", "stable_fact", "Title B", vec(0, 1, 0))
	seedDoc(t, s, "doc3", "bob", "stable_fact", "Title C", vec(0, 0, 1))

	assert.Equal(t, int64(3), s.CollectionSize())
}

func TestMemoryCellStore_SeedFromEmbedding_EmptyVecSkipped(t *testing.T) {
	s := NewMemoryCellStore(nil, zerolog.Nop())
	doc := MemoryCellIndexEntry{ID: "empty", Subject: "alice"}
	s.SeedFromEmbedding(doc, nil)
	assert.Equal(t, int64(0), s.CollectionSize())

	s.SeedFromEmbedding(doc, []float32{})
	assert.Equal(t, int64(0), s.CollectionSize())
}

// ── CollectionSize ────────────────────────────────────────────────────────────

func TestMemoryCellStore_CollectionSize_MultiSubject(t *testing.T) {
	s := NewMemoryCellStore(nil, zerolog.Nop())
	seedDoc(t, s, "a1", "alice", "stable_fact", "T", vec(1, 0))
	seedDoc(t, s, "b1", "bob", "stable_fact", "T", vec(0, 1))
	seedDoc(t, s, "b2", "bob", "stable_fact", "T2", vec(1, 1))
	assert.Equal(t, int64(3), s.CollectionSize())
}

// ── Delete ────────────────────────────────────────────────────────────────────

func TestMemoryCellStore_Delete(t *testing.T) {
	s := NewMemoryCellStore(nil, zerolog.Nop())
	seedDoc(t, s, "doc1", "alice", "stable_fact", "Title", vec(1, 0, 0))
	seedDoc(t, s, "doc2", "alice", "stable_fact", "Title2", vec(0, 1, 0))

	require.Equal(t, int64(2), s.CollectionSize())

	err := s.Delete(context.Background(), "doc1")
	require.NoError(t, err)
	assert.Equal(t, int64(1), s.CollectionSize())
}

func TestMemoryCellStore_Delete_NotFound(t *testing.T) {
	s := NewMemoryCellStore(nil, zerolog.Nop())
	// Delete of non-existent ID should not error.
	err := s.Delete(context.Background(), "ghost")
	assert.NoError(t, err)
}

func TestMemoryCellStore_Delete_AcrossSubjects(t *testing.T) {
	s := NewMemoryCellStore(nil, zerolog.Nop())
	seedDoc(t, s, "shared-id", "alice", "stable_fact", "T", vec(1, 0))
	seedDoc(t, s, "other-id", "bob", "stable_fact", "T", vec(0, 1))

	err := s.Delete(context.Background(), "shared-id")
	require.NoError(t, err)
	assert.Equal(t, int64(1), s.CollectionSize())
}

// ── SearchSubject (no embedder — use pre-seeded vecs via a fake) ──────────────

func TestMemoryCellStore_SearchSubject_ViaFakeEmbedder(t *testing.T) {
	// Verify routing logic by accessing subjectIndex directly (no embedder needed).
	// Use similar embeddings so the 97%-of-top-score filter keeps both results.
	s := NewMemoryCellStore(nil, zerolog.Nop())
	// alice1 and alice2 share nearly the same direction — both survive the score filter.
	seedDoc(t, s, "alice1", "alice", "stable_fact", "Go concurrency", vec(1, 0.01, 0, 0))
	seedDoc(t, s, "alice2", "alice", "recent_change", "Python async", vec(1, 0.02, 0, 0))
	seedDoc(t, s, "global1", "", "stable_fact", "Global fact", vec(1, 0, 0, 0))

	s.mu.RLock()
	aliceIdx := s.subjects["alice"]
	globalIdx := s.subjects[""]
	s.mu.RUnlock()

	require.NotNil(t, aliceIdx, "alice subject index should exist")
	require.NotNil(t, globalIdx, "global subject index should exist")

	aliceResults := aliceIdx.search(vec(1, 0, 0, 0), 10, "")
	assert.Len(t, aliceResults, 2)

	globalResults := globalIdx.search(vec(1, 0, 0, 0), 10, "")
	assert.Len(t, globalResults, 1)
	assert.Equal(t, "global1", globalResults[0].Entry.ID)
}

// ── getOrCreate ───────────────────────────────────────────────────────────────

func TestMemoryCellStore_GetOrCreate(t *testing.T) {
	s := NewMemoryCellStore(nil, zerolog.Nop())
	idx1 := s.getOrCreate("alice")
	idx2 := s.getOrCreate("alice")
	assert.Same(t, idx1, idx2, "same pointer for same subject")

	idx3 := s.getOrCreate("bob")
	assert.NotSame(t, idx1, idx3)
}

// ── benchmarks ───────────────────────────────────────────────────────────────

var nodeCounts = []int{100, 500, 1000, 5000, 10000}

func BenchmarkSubjectIndex_Insert(b *testing.B) {
	for _, n := range nodeCounts {
		b.Run(fmt.Sprintf("nodes=%d", n), func(b *testing.B) {
			rng := rand.New(rand.NewSource(42))
			idx := buildIndex(n)
			b.ResetTimer()
			for range b.N {
				v := randVec(rng, benchDims)
				doc := MemoryCellIndexEntry{ID: fmt.Sprintf("new-%d", b.N), Kind: "stable_fact"}
				idx.insert(doc, v)
			}
		})
	}
}

// rebuildHoldingLock rebuilds idx's graph in place; the caller holds idx.mu.
func rebuildHoldingLock(idx *subjectIndex) {
	g := newGraph()
	for id, e := range idx.entries {
		g.Insert(cellNodeEmbedding{Key: id, Vec: vector.F32(e.Embedding)})
	}
	idx.index = g
}

func BenchmarkSubjectIndex_Search(b *testing.B) {
	for _, n := range nodeCounts {
		b.Run(fmt.Sprintf("nodes=%d", n), func(b *testing.B) {
			rng := rand.New(rand.NewSource(99))
			idx := buildIndex(n)
			query := randVec(rng, benchDims)
			b.ResetTimer()
			for range b.N {
				idx.search(query, 10, "")
			}
		})
	}
}

func BenchmarkSubjectIndex_Delete(b *testing.B) {
	for _, n := range nodeCounts {
		b.Run(fmt.Sprintf("nodes=%d", n), func(b *testing.B) {
			b.ResetTimer()
			for range b.N {
				b.StopTimer()
				idx := buildIndex(n)
				b.StartTimer()
				// A delete marks the graph dirty; the rebuild runs on the next
				// read — measure that cost.
				idx.remove("id-0")
				idx.freshen()
			}
		})
	}
}

// BenchmarkSubjectIndex_ReaderBlockedUnderLock measures how long a concurrent
// reader is blocked while a rebuild holds the write lock.
func BenchmarkSubjectIndex_ReaderBlockedUnderLock(b *testing.B) {
	for _, n := range nodeCounts {
		b.Run(fmt.Sprintf("nodes=%d", n), func(b *testing.B) {
			idx := buildIndex(n)
			rng := rand.New(rand.NewSource(7))
			query := randVec(rng, benchDims)
			b.ResetTimer()
			for range b.N {
				started := make(chan struct{})
				done := make(chan struct{})
				go func() {
					idx.mu.Lock()
					close(started)
					rebuildHoldingLock(idx) // holds write lock for full rebuild
					idx.mu.Unlock()
					close(done)
				}()

				<-started
				b.StartTimer()
				idx.mu.RLock() // blocked until rebuild releases write lock
				_ = idx.index.Search(cellNodeEmbedding{Vec: vector.F32(query)}, 10, 50)
				idx.mu.RUnlock()
				b.StopTimer()

				<-done
			}
		})
	}
}

// BenchmarkSubjectIndex_ReaderBlockedSwap measures how long a concurrent
// reader is blocked when rebuild happens outside the lock and only the
// pointer swap holds the write lock.
func BenchmarkSubjectIndex_ReaderBlockedSwap(b *testing.B) {
	for _, n := range nodeCounts {
		b.Run(fmt.Sprintf("nodes=%d", n), func(b *testing.B) {
			idx := buildIndex(n)
			rng := rand.New(rand.NewSource(7))
			query := randVec(rng, benchDims)
			b.ResetTimer()
			for range b.N {
				// snapshot entries under brief read lock
				idx.mu.RLock()
				snapshot := make(map[string]cellEntry, len(idx.entries))
				for id, e := range idx.entries {
					snapshot[id] = e
				}
				idx.mu.RUnlock()

				// build new graph outside any lock
				newGraph := hnsw.New(cellSurface,
					hnsw.WithEfConstruction(200),
					hnsw.WithM(16),
					hnsw.WithM0(32),
				)
				for id, e := range snapshot {
					if len(e.Embedding) > 0 {
						newGraph.Insert(cellNodeEmbedding{Key: id, Vec: vector.F32(e.Embedding)})
					}
				}

				// swap: write lock held only for pointer assignment
				started := make(chan struct{})
				done := make(chan struct{})
				go func() {
					idx.mu.Lock()
					close(started)
					idx.index = newGraph
					idx.mu.Unlock()
					close(done)
				}()

				<-started
				b.StartTimer()
				idx.mu.RLock()
				_ = idx.index.Search(cellNodeEmbedding{Vec: vector.F32(query)}, 10, 50)
				idx.mu.RUnlock()
				b.StopTimer()

				<-done
			}
		})
	}
}

// ── concurrent seed ───────────────────────────────────────────────────────────

func TestMemoryCellStore_ConcurrentSeed(t *testing.T) {
	s := NewMemoryCellStore(nil, zerolog.Nop())
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := string(rune('a' + i%26))
			seedDoc(t, s, id+string(rune('0'+i)), "subject", "stable_fact", "T", vec(float32(i), 0))
		}(i)
	}
	wg.Wait()
	assert.Equal(t, int64(50), s.CollectionSize())
}
