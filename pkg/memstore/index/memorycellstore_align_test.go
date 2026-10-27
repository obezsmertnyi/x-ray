package index

import (
	"context"
	"fmt"
	"math/rand"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A delete storm costs one rebuild, and a deleted id is never returned before
// or after it.
func TestSubjectIndex_DeleteIsLazyAndNeverReturned(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	idx := newSubjectIndex()
	vecs := map[string][]float32{}
	for i := range 40 {
		id := fmt.Sprintf("id-%d", i)
		vecs[id] = randVec(rng, 8)
		idx.insert(IndexEntry{ID: id, Kind: "stable_fact"}, vecs[id])
	}
	before := idx.index
	for i := range 30 {
		require.True(t, idx.remove(fmt.Sprintf("id-%d", i)))
	}
	assert.True(t, idx.dirty)
	assert.Same(t, before, idx.index, "a delete does not touch the graph")

	hits := idx.search(vecs["id-35"], 40, "")
	assert.NotSame(t, before, idx.index, "the first read rebuilt the graph")
	assert.False(t, idx.dirty)
	require.NotEmpty(t, hits)
	assert.Equal(t, "id-35", hits[0].Document.ID)
	for _, h := range hits {
		var n int
		_, _ = fmt.Sscanf(h.Document.ID, "id-%d", &n)
		assert.GreaterOrEqual(t, n, 30, "a deleted id came back: %s", h.Document.ID)
	}

	rebuilt := idx.index
	idx.search(vecs["id-35"], 5, "")
	assert.Same(t, rebuilt, idx.index, "no further rebuild without another delete")
}

func TestSubjectIndex_ReseedingAnUnchangedCellDoesNotDirtyTheGraph(t *testing.T) {
	idx := newSubjectIndex()
	v := vec(1, 0, 0, 0)
	doc := IndexEntry{ID: "a", Kind: "stable_fact"}
	idx.insert(doc, v)
	idx.insert(doc, v)
	assert.False(t, idx.dirty)

	idx.insert(doc, vec(0, 1, 0, 0))
	assert.True(t, idx.dirty, "a replaced embedding leaves a stale node behind")
}

// A minority kind is found even when a populous kind fills every nearby slot.
func TestSubjectIndex_KindFilterIsNotStarvedByAPopulousKind(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	idx := newSubjectIndex()
	query := randVec(rng, 8)
	for i := range 200 {
		// Every common cell sits closer to the query than the rare one.
		v := make([]float32, len(query))
		copy(v, query)
		v[i%8] += float32(i%5) * 0.01
		idx.insert(IndexEntry{ID: fmt.Sprintf("common-%d", i), Kind: "event"}, v)
	}
	far := make([]float32, len(query))
	for i := range far {
		far[i] = -query[i]
	}
	idx.insert(IndexEntry{ID: "rare", Kind: "hypothesis"}, far)

	hits := idx.search(query, 3, "hypothesis")
	require.Len(t, hits, 1)
	assert.Equal(t, "rare", hits[0].Document.ID)
}

func TestMemoryCellStore_DimensionGuard(t *testing.T) {
	s := NewCellStore(nil, zerolog.Nop())
	assert.True(t, s.SeedFromEmbedding(IndexEntry{ID: "a", Kind: "note"}, vec(1, 0, 0, 0)))
	assert.False(t, s.SeedFromEmbedding(IndexEntry{ID: "b", Kind: "note"}, vec(1, 0, 0, 0, 0, 0, 0, 0)),
		"a vector of another length is refused so the caller can re-embed it")
	assert.Equal(t, int64(1), s.CollectionSize())
	assert.True(t, s.SeedFromEmbedding(IndexEntry{ID: "c", Kind: "note"}, nil), "nothing to seed is not a failure")

	assert.True(t, s.dimMismatch(vec(1, 0, 0, 0, 0, 0, 0, 0)))
	assert.False(t, s.dimMismatch(vec(0, 1, 0, 0)))
}

func TestMemoryCellStore_DeleteFindsTheSubjectAndLeavesTheRest(t *testing.T) {
	s := NewCellStore(nil, zerolog.Nop())
	seedDoc(t, s, "a", "s1", "note", "A", vec(1, 0, 0, 0))
	seedDoc(t, s, "b", "s2", "note", "B", vec(0, 1, 0, 0))
	require.NoError(t, s.Delete(context.Background(), "a"))
	assert.Equal(t, int64(1), s.CollectionSize())
	require.NoError(t, s.Delete(context.Background(), "missing"))
	assert.Equal(t, int64(1), s.CollectionSize())
}

func TestClusterEdges_KeepsOnlyPairsTighterToEachOtherThanToTheCentre(t *testing.T) {
	centre := vec(1, 0, 0, 0)
	entry := func(id string, v []float32) cellEntry {
		return cellEntry{Cell: IndexEntry{ID: id}, Embedding: v}
	}
	// a and b lean the same way, away from the centre; c leans another way.
	picked := []cellEntry{
		entry("a", vec(0.6, 0.8, 0, 0)),
		entry("b", vec(0.6, 0.78, 0.1, 0)),
		entry("c", vec(0.6, 0, 0.8, 0)),
	}
	edges := clusterEdges(centre, picked, 3)
	require.Len(t, edges, 1)
	assert.ElementsMatch(t, []string{"a", "b"}, []string{edges[0].A, edges[0].B})

	assert.Empty(t, clusterEdges(centre, picked[:1], 3), "one neighbour has nobody to link to")
	// Neighbours that all sit between the centre and each other form a star.
	star := []cellEntry{entry("x", vec(0.99, 0.1, 0, 0)), entry("y", vec(0.99, 0, 0.1, 0))}
	assert.Empty(t, clusterEdges(centre, star, 3))
}

func TestMemoryCellStore_NeighborhoodUnknownIDIsNotIndexed(t *testing.T) {
	s := NewCellStore(nil, zerolog.Nop())
	seedDoc(t, s, "a", "", "note", "A", vec(1, 0, 0, 0))
	_, ok := s.Neighborhood("missing", 5, 3, nil)
	assert.False(t, ok)
	nh, ok := s.Neighborhood("a", 5, 3, nil)
	require.True(t, ok)
	assert.Empty(t, nh.Neighbors)
}

func TestMemoryCellStore_SearchExhaustiveRanksEveryEntry(t *testing.T) {
	s := NewCellStore(nil, zerolog.Nop())
	s.queryCache = newQueryEmbedCache(&fakeQueryBackend{vecFn: func(string) []float32 { return vec(1, 0, 0, 0) }}, 0, 0)
	seedDoc(t, s, "near", "s1", "note", "N", vec(1, 0.1, 0, 0))
	seedDoc(t, s, "global", "", "stable_fact", "G", vec(0.8, 0.5, 0, 0))
	seedDoc(t, s, "other-subject", "s2", "note", "O", vec(0.9, 0.2, 0, 0))
	seedDoc(t, s, "far", "s1", "note", "F", vec(0, 0, 0, 1))

	got, err := s.SearchExhaustive(context.Background(), "q", "s1", "")
	require.NoError(t, err)
	var ids []string
	for _, r := range got {
		ids = append(ids, r.Document.ID)
	}
	assert.Equal(t, []string{"near", "global", "far"}, ids, "the subject's cells and the global ones, every one, best first")

	all, err := s.SearchExhaustive(context.Background(), "q", "", "note")
	require.NoError(t, err)
	assert.Len(t, all, 3, "no subject: every subject, filtered to the kind")
}

func TestMemoryCellStore_SearchReportsItsPhaseSplit(t *testing.T) {
	s := NewCellStore(nil, zerolog.Nop())
	s.queryCache = newQueryEmbedCache(&fakeQueryBackend{vecFn: func(string) []float32 { return vec(1, 0, 0, 0) }}, 0, 0)
	seedDoc(t, s, "a", "", "note", "A", vec(1, 0, 0, 0))

	var got []SearchTiming
	ctx := WithTimingSink(context.Background(), func(tm SearchTiming) { got = append(got, tm) })
	_, err := s.Search(ctx, "q", 3, "")
	require.NoError(t, err)
	_, err = s.SearchSubject(ctx, "q", "", 3, "")
	require.NoError(t, err)
	assert.Len(t, got, 2, "each search reports once")

	_, err = s.Search(context.Background(), "q", 3, "")
	require.NoError(t, err, "no sink is fine")
}
