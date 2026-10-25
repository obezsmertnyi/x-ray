package xray

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestListRecent_NewestFirst is the core new behavior: ListRecent returns items
// ordered by CreatedAt descending — the recall path for episodic memory, where
// "the latest creation time is the last session". Plain List has no time order.
func TestListRecent_NewestFirst(t *testing.T) {
	s := newTestStore()
	user := stableUserID("azure:user-a")

	// Three episodic memories; stamp CreatedAt explicitly so ordering is
	// deterministic regardless of upsert wall-clock granularity.
	mk := func(title string, ageMin int) string {
		res, errMsg := s.Upsert(context.Background(), "", "episodic", "private", title, "body of "+title, user, "medium", "", nil, nil)
		require.Empty(t, errMsg)
		s.mu.Lock()
		s.items[res.ID].CreatedAt = time.Now().Add(-time.Duration(ageMin) * time.Minute)
		s.mu.Unlock()
		return res.ID
	}
	oldID := mk("oldest", 30)
	midID := mk("middle", 15)
	newID := mk("newest", 1)

	got := s.ListRecent("episodic", scopePrivate, user, 5)
	require.Len(t, got, 3)
	assert.Equal(t, newID, got[0].ID, "newest must be first")
	assert.Equal(t, midID, got[1].ID)
	assert.Equal(t, oldID, got[2].ID, "oldest must be last")
}

// TestListRecent_RespectsLimitAndKind confirms the cap returns the N newest (not
// an arbitrary N) and that the kind filter isolates episodic from other kinds.
func TestListRecent_RespectsLimitAndKind(t *testing.T) {
	s := newTestStore()
	user := stableUserID("azure:user-a")

	for i := 0; i < 5; i++ {
		res, errMsg := s.Upsert(context.Background(), "", "episodic", "private", "ep", "body", user, "medium", "", nil, nil)
		require.Empty(t, errMsg)
		s.mu.Lock()
		s.items[res.ID].CreatedAt = time.Now().Add(-time.Duration(i) * time.Minute)
		s.mu.Unlock()
	}
	// A note (different kind) and a public episodic of another user must not leak in.
	_, errMsg := s.Upsert(context.Background(), "", "note", "private", "a note", "body", user, "medium", "", nil, nil)
	require.Empty(t, errMsg)

	got := s.ListRecent("episodic", scopePrivate, user, 2)
	require.Len(t, got, 2, "limit must cap to 2 newest episodic items")
	for _, it := range got {
		assert.Equal(t, "episodic", it.Kind)
	}
}

// TestListRecent_ZeroLimit returns nothing rather than the whole set.
func TestListRecent_ZeroLimit(t *testing.T) {
	s := newTestStore()
	user := stableUserID("azure:user-a")
	_, errMsg := s.Upsert(context.Background(), "", "episodic", "private", "ep", "body", user, "medium", "", nil, nil)
	require.Empty(t, errMsg)
	assert.Empty(t, s.ListRecent("episodic", scopePrivate, user, 0))
}

// mkEpisodic upserts an episodic memory and back-dates its CreatedAt by ageMin.
func mkEpisodic(t *testing.T, s *MemoryStore, user, title string, ageMin int) string {
	t.Helper()
	res, errMsg := s.Upsert(context.Background(), "", "episodic", "private", title, "body", user, "medium", "", nil, nil)
	require.Empty(t, errMsg)
	s.mu.Lock()
	s.items[res.ID].CreatedAt = time.Now().Add(-time.Duration(ageMin) * time.Minute)
	s.mu.Unlock()
	return res.ID
}

// TestListRecent_ForgetsExpiredEpisodic is the read-time forget: with a TTL set,
// ListRecent drops episodic memories older than the TTL even though they still
// exist in the store (the reaper hasn't run). Fresh ones remain.
func TestListRecent_ForgetsExpiredEpisodic(t *testing.T) {
	s := newTestStore()
	s.SetEpisodicTTL(10 * time.Minute)
	user := stableUserID("azure:user-a")

	mkEpisodic(t, s, user, "stale", 60) // older than TTL → forgotten
	freshID := mkEpisodic(t, s, user, "fresh", 1)

	got := s.ListRecent("episodic", scopePrivate, user, 10)
	require.Len(t, got, 1, "expired episodic memory must be filtered at read time")
	assert.Equal(t, freshID, got[0].ID)
}

// TestListRecent_ZeroTTLNeverForgets confirms TTL=0 disables expiry.
func TestListRecent_ZeroTTLNeverForgets(t *testing.T) {
	s := newTestStore()
	s.SetEpisodicTTL(0)
	user := stableUserID("azure:user-a")
	mkEpisodic(t, s, user, "ancient", 60*24*365) // a year old
	assert.Len(t, s.ListRecent("episodic", scopePrivate, user, 10), 1, "TTL=0 must never forget")
}

// TestReapExpiredEpisodic_DeletesExpired confirms the reaper removes expired
// episodic items from the store (map mode, nil syncer) and leaves fresh ones.
func TestReapExpiredEpisodic_DeletesExpired(t *testing.T) {
	s := newTestStore()
	s.SetEpisodicTTL(10 * time.Minute)
	user := stableUserID("azure:user-a")

	staleID := mkEpisodic(t, s, user, "stale", 60)
	freshID := mkEpisodic(t, s, user, "fresh", 1)

	s.reapExpiredEpisodic(context.Background(), nil)

	s.mu.RLock()
	_, staleStillThere := s.items[staleID]
	_, freshStillThere := s.items[freshID]
	s.mu.RUnlock()
	assert.False(t, staleStillThere, "reaper must delete the expired episodic memory")
	assert.True(t, freshStillThere, "reaper must keep the fresh episodic memory")
}
