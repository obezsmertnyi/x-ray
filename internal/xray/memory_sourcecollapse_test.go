package xray

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	mcell "github.com/triageagent-dev/x-ray/v2/api/v1"
)

// ── Source collapse: rank on facts, read the episode ──

// TestMemoryStore_SourceCollapse_OffIsByteIdentical: a store that never calls
// SetSourceCollapse, and one that turns it on and back off, return identical
// results — facts stay facts.
func TestMemoryStore_SourceCollapse_OffIsByteIdentical(t *testing.T) {
	s := newTestStore()
	seedLinkedItem(s, "ep1", "episode", "public", "Episode one", "Caroline told Melanie she adopted a dog named Biscuit in May.", "", "active", nil)
	seedLinkedItem(s, "f1", "stable_fact", "public", "Caroline adopted a dog named Biscuit", "Caroline adopted a dog named Biscuit", "", "active",
		[]mcell.ThoughtLink{{ToID: "ep1", Relation: "derived_from", Weight: 1}})

	before := s.Search(context.Background(), "adopted a dog", "", "", 10)
	s.SetSourceCollapse("derived_from")
	_ = s.Search(context.Background(), "adopted a dog", "", "", 10)
	s.SetSourceCollapse("")
	after := s.Search(context.Background(), "adopted a dog", "", "", 10)

	require.Equal(t, len(before), len(after))
	for i := range before {
		assert.Equal(t, before[i].ID, after[i].ID)
		assert.Empty(t, after[i].Via)
	}
}

// TestMemoryStore_SourceCollapse_FactHitReturnsEpisode: with collapse on, a
// query that matches only the fact returns the episode it was derived from,
// stamped Via the fact, and the fact itself is not returned twice.
func TestMemoryStore_SourceCollapse_FactHitReturnsEpisode(t *testing.T) {
	s := newTestStore()
	seedLinkedItem(s, "ep1", "episode", "public", "Episode one", "A long narrative about a weekend that mentions a pet only in passing.", "", "active", nil)
	seedLinkedItem(s, "f1", "stable_fact", "public", "Caroline adopted a dog named Biscuit", "Caroline adopted a dog named Biscuit", "", "active",
		[]mcell.ThoughtLink{{ToID: "ep1", Relation: "derived_from", Weight: 1}})
	seedLinkedItem(s, "f2", "stable_fact", "public", "Biscuit the dog likes the beach", "Biscuit the dog likes the beach", "", "active",
		[]mcell.ThoughtLink{{ToID: "ep1", Relation: "derived_from", Weight: 1}})
	seedLinkedItem(s, "other", "stable_fact", "public", "Melanie painted a dog portrait", "Melanie painted a dog portrait", "", "active", nil)

	s.SetSourceCollapse("derived_from")
	results := s.Search(context.Background(), "dog", "", "", 10)

	ids := make([]string, 0, len(results))
	for _, r := range results {
		ids = append(ids, r.ID)
	}
	require.Contains(t, ids, "ep1", "the episode both facts derive from must be returned")
	assert.NotContains(t, ids, "f1", "a collapsed fact is replaced, not duplicated")
	assert.NotContains(t, ids, "f2")
	assert.Equal(t, 1, countID(ids, "ep1"), "two facts collapsing to one episode yield it once")
	for _, r := range results {
		if r.ID == "ep1" {
			assert.Contains(t, []string{"f1", "f2"}, r.Via, "Via names the fact that surfaced the episode")
		}
	}
	// The unlinked item passes through untouched.
	assert.Contains(t, ids, "other")
}

// TestMemoryStore_SourceCollapse_DanglingSourceKeepsFact: a fact whose source
// is missing or superseded is returned as itself rather than dropped.
func TestMemoryStore_SourceCollapse_DanglingSourceKeepsFact(t *testing.T) {
	s := newTestStore()
	seedLinkedItem(s, "gone", "episode", "public", "Superseded episode", "old", "", "superseded", nil)
	seedLinkedItem(s, "f1", "stable_fact", "public", "gizmo fact one", "gizmo fact one", "", "active",
		[]mcell.ThoughtLink{{ToID: "gone", Relation: "derived_from"}})
	seedLinkedItem(s, "f2", "stable_fact", "public", "gizmo fact two", "gizmo fact two", "", "active",
		[]mcell.ThoughtLink{{ToID: "missing", Relation: "derived_from"}})

	s.SetSourceCollapse("derived_from")
	results := s.Search(context.Background(), "gizmo", "", "", 10)
	ids := make([]string, 0, len(results))
	for _, r := range results {
		ids = append(ids, r.ID)
	}
	assert.ElementsMatch(t, []string{"f1", "f2"}, ids)
}

// TestMemoryStore_SourceCollapse_RespectsSubjectVisibility: an episode owned
// by another subject is never surfaced through a fact's link.
func TestMemoryStore_SourceCollapse_RespectsSubjectVisibility(t *testing.T) {
	s := newTestStore()
	seedLinkedItem(s, "ep-b", "episode", "public", "Episode of B", "narrative", "conv-b", "active", nil)
	seedLinkedItem(s, "f1", "stable_fact", "public", "gizmo fact", "gizmo fact", "conv-a", "active",
		[]mcell.ThoughtLink{{ToID: "ep-b", Relation: "derived_from"}})

	s.SetSourceCollapse("derived_from")
	results := s.Search(context.Background(), "gizmo", "", "conv-a", 10)
	require.Len(t, results, 1)
	assert.Equal(t, "f1", results[0].ID, "the cross-subject source is hidden; the fact stands in")
}

// TestMemoryStore_SourceCollapse_FanoutControlsSourceDiversity: with the
// candidate pool sized to exactly `limit` (fanout=1), every top-ranked
// candidate can come from the same source episode, so source-collapse
// returns only that one episode; a wider pool (fanout large enough) is
// needed to let a lower-ranked second episode's facts survive into the
// collapsed result. Ranking is pinned deterministically by the lexical
// scorer (ADR-030): episode A's facts match the query token in the title,
// which lexScore rewards twice — once for the whole-query title-contains
// bonus, once for the per-token title match — while episode B's facts match
// the token only in content, a single, much smaller bonus. That gap holds
// regardless of map iteration order, so A's facts always outrank B's.
func TestMemoryStore_SourceCollapse_FanoutControlsSourceDiversity(t *testing.T) {
	s := newTestStore()
	seedLinkedItem(s, "epA", "episode", "public", "Episode A", "narrative A", "", "active", nil)
	seedLinkedItem(s, "epB", "episode", "public", "Episode B", "narrative B", "", "active", nil)
	for _, id := range []string{"a1", "a2", "a3"} {
		seedLinkedItem(s, id, "stable_fact", "public", "gizmo "+id, "gizmo "+id, "", "active",
			[]mcell.ThoughtLink{{ToID: "epA", Relation: "derived_from"}})
	}
	for _, id := range []string{"b1", "b2"} {
		seedLinkedItem(s, id, "stable_fact", "public", "topic "+id, "mentions gizmo in "+id, "", "active",
			[]mcell.ThoughtLink{{ToID: "epB", Relation: "derived_from"}})
	}

	s.SetSourceCollapse("derived_from")
	const limit = 3

	s.SetCandidateFanout(1)
	narrow := s.Search(context.Background(), "gizmo", "", "", limit)
	narrowIDs := make([]string, 0, len(narrow))
	for _, r := range narrow {
		narrowIDs = append(narrowIDs, r.ID)
	}
	assert.Equal(t, []string{"epA"}, narrowIDs, "fanout=1: the top-limit pool is all epA facts, so only epA surfaces")

	s.SetCandidateFanout(10)
	wide := s.Search(context.Background(), "gizmo", "", "", limit)
	wideIDs := make([]string, 0, len(wide))
	for _, r := range wide {
		wideIDs = append(wideIDs, r.ID)
	}
	assert.ElementsMatch(t, []string{"epA", "epB"}, wideIDs, "a wider pool lets epB's lower-ranked facts survive the collapse")
}

// TestMemoryStore_SetLinks writes links onto an ephemeral item and reports
// false for an unknown id.
func TestMemoryStore_SetLinks(t *testing.T) {
	s := newTestStore()
	id := upsertItem(t, s, "f1", "stable_fact", "public", "fact", "fact body", "", nil)
	require.True(t, s.SetLinks(id, []mcell.ThoughtLink{{ToID: "ep1", Relation: "derived_from"}}))
	got := s.GetItem(id)
	require.Len(t, got.Links, 1)
	assert.Equal(t, "ep1", got.Links[0].ToID)
	assert.False(t, s.SetLinks("nope", nil))
}

// ── Reranker hook ──

type fakeReranker struct {
	calls   int
	order   []int
	err     error
	lastLen int // number of docs the most recent call received
}

func (f *fakeReranker) Rerank(_ context.Context, _ string, docs []string, topN int) ([]int, error) {
	f.calls++
	f.lastLen = len(docs)
	if f.err != nil {
		return nil, f.err
	}
	out := f.order
	if len(out) > topN {
		out = out[:topN]
	}
	return out, nil
}

// TestMemoryStore_Rerank_NotCalledOnLexicalFastPath: a keyword-only store (no
// embedder) never reaches the fused path, so the reranker is never consulted.
func TestMemoryStore_Rerank_NotCalledOnLexicalFastPath(t *testing.T) {
	s := newTestStore()
	upsertItem(t, s, "a", "stable_fact", "public", "gizmo", "gizmo body", "", nil)
	upsertItem(t, s, "b", "stable_fact", "public", "gizmo two", "another gizmo", "", nil)
	fr := &fakeReranker{order: []int{1, 0}}
	s.SetReranker(fr)
	res := s.Search(context.Background(), "gizmo", "", "", 10)
	require.Len(t, res, 2)
	assert.Equal(t, 0, fr.calls, "the lexical short-circuit must never pay for a rerank")
}

// TestMemoryStore_rerank_ReordersAndFallsBack exercises the step directly:
// the reranker's order wins, and an error keeps the incoming order.
func TestMemoryStore_rerank_ReordersAndFallsBack(t *testing.T) {
	s := newTestStore()
	items := []*MemoryItem{{ID: "a", Title: "a"}, {ID: "b", Title: "b"}, {ID: "c", Title: "c"}}

	s.SetReranker(&fakeReranker{order: []int{2, 0, 1}})
	got := s.rerank(context.Background(), "q", items, 2)
	require.Len(t, got, 2)
	assert.Equal(t, "c", got[0].ID)
	assert.Equal(t, "a", got[1].ID)

	s.SetReranker(&fakeReranker{err: errors.New("sidecar down")})
	got = s.rerank(context.Background(), "q", items, 2)
	require.Len(t, got, 3, "on error the fused order is returned untouched")
	assert.Equal(t, "a", got[0].ID)
}

// TestMemoryStore_rerank_WindowsToLimitTimesFanout: rerank must not hand the
// cross-encoder the whole fused pool — only the first limit*rerankFanout
// items — even though a fallback (no reranker/error/empty order) still
// returns the untouched, unwindowed pool.
func TestMemoryStore_rerank_WindowsToLimitTimesFanout(t *testing.T) {
	s := newTestStore()
	wide := make([]*MemoryItem, 10)
	for i := range wide {
		id := string(rune('a' + i))
		wide[i] = &MemoryItem{ID: id, Title: id}
	}

	fr := &fakeReranker{order: []int{0, 1}}
	s.SetReranker(fr)
	got := s.rerank(context.Background(), "q", wide, 2)
	require.Len(t, got, 2)
	assert.Equal(t, 6, fr.lastLen, "with limit=2 and rerankFanout=3, the reranker must see exactly 6 documents")
}

func countID(ids []string, id string) int {
	n := 0
	for _, x := range ids {
		if x == id {
			n++
		}
	}
	return n
}
