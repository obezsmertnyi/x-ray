package memstore

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// An item that goes stale by its deadline, noticed lazily on a read, leaves the
// lexical indexes as one retired by Expire does.
func TestStore_LazilyExpiredItemLeavesTheIndexes(t *testing.T) {
	s := NewStore()
	soon := metav1.NewTime(time.Now().Add(time.Hour))
	it := mk("old", "note", "Retiring batch", "the nightly batch retires soon")
	it.StaleAfter = &soon
	s.Put(it)
	require.True(t, s.Indexed("old"))
	require.Equal(t, 1, s.vocab.wordDocFreq("batch"))
	// The deadline passes while the item sits in the store.
	past := metav1.NewTime(time.Now().Add(-time.Minute))
	held := s.cells["old"]
	held.StaleAfter = &past
	s.cells["old"] = held

	_ = s.Items() // a read that notices the deadline

	assert.False(t, s.Indexed("old"), "the trigram index still holds a stale item")
	assert.Equal(t, 0, s.vocab.wordDocFreq("batch"), "the vocabulary still counts a stale item")
	assert.Empty(t, s.KeywordSearch("nightly batch", "", "", 5))
}

// A clone is searchable: its indexes are built from the cloned items, and it
// inherits the vocabulary of the store it was cloned from.
func TestStore_CloneIsSearchableAndKeepsTheVocabulary(t *testing.T) {
	voc := Vocabulary{NonSearchable: []string{"prompt"}}
	src := NewStore(WithConfig(Config{Vocabulary: voc}))
	upsert(t, src, mk("a", "note", "Ingest queue", "the ingest queue drains hourly"))
	upsert(t, src, mk("p", "prompt", "System prompt", "the ingest queue prompt"))

	clone := src.CloneInto(&sync.RWMutex{}, Config{})

	assert.Equal(t, []string{"a"}, ids(clone.KeywordSearch("ingest queue", "", "", 5)), "a clone finds its items, and keeps the host's hidden kinds hidden")
	assert.True(t, clone.Indexed("a"))
	assert.True(t, clone.cfg.Vocabulary.IsNonSearchable("prompt"))

	c2 := src.CloneInto(&sync.RWMutex{}, Config{Vocabulary: Vocabulary{}})
	assert.True(t, c2.cfg.Vocabulary.IsNonSearchable("prompt"), "an empty vocabulary given to CloneInto means inherit")
}

// GetFold passes what it returns through the Read hook, like Get.
func TestStore_GetFoldRunsTheReadHook(t *testing.T) {
	s := NewStore(WithConfig(Config{Read: func(items []Item) {
		for i := range items {
			items[i].Content = "corrected"
		}
	}}))
	upsert(t, s, mk("Status-Note", "note", "Status", "stale words"))

	exact, ok := s.GetFold("Status-Note", false)
	require.True(t, ok)
	assert.Equal(t, "corrected", exact.Content, "the exact branch")
	folded, ok := s.GetFold("status-note", true)
	require.True(t, ok)
	assert.Equal(t, "corrected", folded.Content, "the case-folded branch")
}

// KeywordSearchOwn ranks only the person's own private items, so shared items
// that score higher cannot push them out of the limit.
func TestStore_KeywordSearchOwnRanksOnlyThePersonsPrivateItems(t *testing.T) {
	s := NewStore()
	for i := 0; i < 6; i++ {
		upsert(t, s, mk("shared-"+string(rune('a'+i)), "note", "gizmo gizmo gizmo", "gizmo gizmo is everything here"))
	}
	priv := mk("mine", "note", "Scratch", "a passing mention of a gizmo")
	priv.Scope = ScopePrivate
	priv.Tags = []string{PrivateOwnerTagPrefix + "alice"}
	upsert(t, s, priv)
	other := mk("theirs", "note", "Their scratch", "gizmo in someone else's note")
	other.Scope = ScopePrivate
	other.Tags = []string{PrivateOwnerTagPrefix + "bob"}
	upsert(t, s, other)

	got := s.KeywordSearchOwn("alice", "gizmo", "", 3)
	assert.Equal(t, []string{"mine"}, ids(got))
	assert.Empty(t, s.KeywordSearchOwn("", "gizmo", "", 3), "no person, no private items")
}
