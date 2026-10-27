package xray

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func hasID(items []*MemoryItem, id string) bool {
	for _, it := range items {
		if it.ID == id {
			return true
		}
	}
	return false
}

// The person's own private notes are not crowded out of a search by shared items
// that fill the limit first.
func TestSearch_PrivateNotesAreNotStarvedBySharedHits(t *testing.T) {
	s := newTestStore()
	s.SetWriteDedup(false)
	for i := range 12 {
		upsertItem(t, s, fmt.Sprintf("shared-%d", i), "stable_fact", "public", fmt.Sprintf("Gizmo fact %d", i), "the gizmo does a thing", "alice", nil)
	}
	upsertItem(t, s, "mine", "note", "private", "Scratch", "a passing note about the gizmo", "alice", nil)

	got := s.Search(context.Background(), "gizmo", "", "alice", 5)
	assert.True(t, hasID(got, "mine"), "the private note is missing from %v", idsOf(got))
	assert.LessOrEqual(t, len(got), 5)
}

// Shared items of other subjects, which the search never returns, do not use up
// the limit that ranks the person's private notes.
func TestSearch_PrivateNotesAreNotRankedAgainstOtherSubjectsItems(t *testing.T) {
	s := newTestStore()
	s.SetWriteDedup(false)
	for i := range 12 {
		upsertItem(t, s, fmt.Sprintf("bob-%d", i), "stable_fact", "public", "gizmo gizmo gizmo", "gizmo gizmo gizmo gizmo everything", "bob", nil)
	}
	upsertItem(t, s, "mine", "note", "private", "Scratch", "a passing note about the gizmo", "alice", nil)

	got := s.Search(context.Background(), "gizmo", "", "alice", 3)
	require.Len(t, got, 1)
	assert.Equal(t, "mine", got[0].ID)
}

// A list that names a subject and no scope returns the newest items across
// shared and private, so a recent private note is not cut off at the limit.
func TestList_PrivateNotesAreOrderedWithSharedOnes(t *testing.T) {
	s := newTestStore()
	s.SetWriteDedup(false)
	for i := range 5 {
		upsertItem(t, s, fmt.Sprintf("shared-%d", i), "stable_fact", "public", fmt.Sprintf("Fact %d", i), fmt.Sprintf("content %d", i), "alice", nil)
	}
	time.Sleep(5 * time.Millisecond)
	upsertItem(t, s, "mine", "note", "private", "Newest", "written last", "alice", nil)

	got := s.List("", "", "alice", 3)
	require.Len(t, got, 3)
	assert.Equal(t, "mine", got[0].ID, "newest first")
}

// Refuting does not copy the whole store to find the few items it cascades to.
func TestRefute_DoesNotSnapshotTheWholeStore(t *testing.T) {
	s := newTestStore()
	s.SetWriteDedup(false)
	for i := range 3000 {
		upsertItem(t, s, fmt.Sprintf("n-%d", i), "note", "public", fmt.Sprintf("Note %d", i), fmt.Sprintf("body of note %d", i), "", nil)
	}
	res, msg := s.Upsert(context.Background(), "wrong", "hypothesis", "public", "Wrong theory", "the moon causes outages", "", "", "", nil, nil, withStaleAfter(time.Now().Add(time.Hour)))
	require.Empty(t, msg)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	changed, found := s.Refute(context.Background(), res.ID, "no evidence", nil)
	runtime.ReadMemStats(&after)

	require.True(t, found)
	require.NotEmpty(t, changed)
	assert.Less(t, after.Mallocs-before.Mallocs, uint64(1000), "refuting one item allocated as if it cloned the store")
}
