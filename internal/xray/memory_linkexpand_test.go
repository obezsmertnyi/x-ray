package xray

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	mcell "github.com/triageagent-dev/x-ray/v2/api/v1"
)

// ── ThoughtLink expansion (docs/todo/graphiti-comparison-refinement.md #4) ──

// TestMemoryStore_LinkExpansion_DefaultOffIsByteIdentical is the golden
// comparison the spec calls for: a store that never touches
// SetLinkExpansion, and one that flips it on and back off, must return
// identical Search results — production behavior is unchanged until an
// operator opts in.
func TestMemoryStore_LinkExpansion_DefaultOffIsByteIdentical(t *testing.T) {
	s := newTestStore()
	seedLinkedItem(s, "a", "stable_fact", "public", "gizmo", "top match content", "", "active",
		[]mcell.ThoughtLink{{ToID: "c", Relation: "correlates_with", Weight: 0.9}})
	seedLinkedItem(s, "b", "stable_fact", "public", "widget gizmo two", "second match content", "", "active", nil)
	seedLinkedItem(s, "c", "stable_fact", "public", "Linked neighbor", "reached only via a's link", "", "active", nil)

	baseline := s.Search(context.Background(), "gizmo", "", "", 10)
	require.Len(t, baseline, 2, "only a and b lexically match 'gizmo'; c is reachable only through a's link")

	// Touch the switch (on, then back off) before re-running — the OFF path
	// must still behave as if SetLinkExpansion had never been called.
	s.SetLinkExpansion(true)
	_ = s.Search(context.Background(), "gizmo", "", "", 10)
	s.SetLinkExpansion(false)
	after := s.Search(context.Background(), "gizmo", "", "", 10)

	require.Len(t, after, len(baseline))
	for i := range baseline {
		assert.Equal(t, baseline[i].ID, after[i].ID)
		assert.Empty(t, after[i].Via, "expansion off must never stamp Via")
	}
}

// TestMemoryStore_LinkExpansion_OnAppendsNeighborAfterDirectHits covers the
// core ON behavior: a linked neighbor that does not itself match the query
// is appended after the direct hits, stamped with Via, while a neighbor
// reachable only through a superseded item's link is skipped.
func TestMemoryStore_LinkExpansion_OnAppendsNeighborAfterDirectHits(t *testing.T) {
	s := newTestStore()
	seedLinkedItem(s, "a", "stable_fact", "public", "gizmo", "top match content", "", "active",
		[]mcell.ThoughtLink{{ToID: "c", Relation: "correlates_with", Weight: 0.9}})
	seedLinkedItem(s, "b", "stable_fact", "public", "widget gizmo two", "second match content", "", "active",
		[]mcell.ThoughtLink{{ToID: "d", Relation: "correlates_with"}})
	seedLinkedItem(s, "c", "stable_fact", "public", "Linked neighbor", "reached only via a's link", "", "active", nil)
	seedLinkedItem(s, "d", "stable_fact", "public", "Superseded neighbor", "must never surface", "", "superseded", nil)

	s.SetLinkExpansion(true)
	results := s.Search(context.Background(), "gizmo", "", "", 10)

	require.Len(t, results, 3, "a, b direct hits plus c expanded in; d is superseded and must be skipped")
	assert.Equal(t, "a", results[0].ID, "exact-title match ranks first")
	assert.Equal(t, "b", results[1].ID)
	assert.Equal(t, "c", results[2].ID, "expanded neighbor is appended after every direct hit")
	assert.Equal(t, "a", results[2].Via, "Via records the id of the result whose link surfaced it")
	assert.Empty(t, results[0].Via, "a direct hit never carries Via")
	assert.Empty(t, results[1].Via)
}

// TestMemoryStore_LinkExpansion_RespectsKCapAndRankOrder covers both the k/2
// cap and the ranking rule together: two direct hits each link to a distinct,
// otherwise-unreachable neighbor, but the k cap only has room for one
// addition — it must be the higher-ranked result's neighbor, not the lower-
// ranked one's.
func TestMemoryStore_LinkExpansion_RespectsKCapAndRankOrder(t *testing.T) {
	s := newTestStore()
	seedLinkedItem(s, "a", "stable_fact", "public", "gizmo", "top match content", "", "active",
		[]mcell.ThoughtLink{{ToID: "x", Relation: "correlates_with"}})
	seedLinkedItem(s, "b", "stable_fact", "public", "widget gizmo two", "second match content", "", "active",
		[]mcell.ThoughtLink{{ToID: "y", Relation: "correlates_with"}})
	seedLinkedItem(s, "x", "stable_fact", "public", "Neighbor of a", "reached via a", "", "active", nil)
	seedLinkedItem(s, "y", "stable_fact", "public", "Neighbor of b", "reached via b", "", "active", nil)

	s.SetLinkExpansion(true)
	// limit=3: base is 2 direct hits (a ranks above b), leaving room=1; maxExtra
	// = min(limit/2=1, room=1) = 1 — exactly one neighbor may be admitted.
	results := s.Search(context.Background(), "gizmo", "", "", 3)

	require.Len(t, results, 3, "total must never exceed the requested limit")
	assert.Equal(t, "a", results[0].ID)
	assert.Equal(t, "b", results[1].ID)
	assert.Equal(t, "x", results[2].ID, "the higher-ranked result's neighbor (a's) wins the single admitted slot over b's")
	assert.Equal(t, "a", results[2].Via)
}

// TestMemoryStore_LinkExpansion_SkipsCrossSubjectPrivateTarget verifies
// expansion applies the same subject-scoped visibility rule the base search
// itself already enforces: a private neighbor belonging to a different
// subject than the caller searched with must not leak in.
func TestMemoryStore_LinkExpansion_SkipsCrossSubjectPrivateTarget(t *testing.T) {
	s := newTestStore()
	seedLinkedItem(s, "a", "stable_fact", "public", "gizmo", "top match content", "", "active",
		[]mcell.ThoughtLink{{ToID: "secret", Relation: "correlates_with"}})
	seedLinkedItem(s, "secret", "note", "private", "Someone else's note", "private content", "owner-b", "active", nil)

	s.SetLinkExpansion(true)
	results := s.Search(context.Background(), "gizmo", "", "", 10)

	require.Len(t, results, 1, "the private neighbor must not be admitted for an unscoped (subject=\"\") search")
	assert.Equal(t, "a", results[0].ID)
}
