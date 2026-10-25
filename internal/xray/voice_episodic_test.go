package xray

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSaveMemory_EpisodicKind confirms the kind arg is honored: kind=episodic
// stores an episodic memory, anything else (including absent/mishears) falls back
// to a plain note.
func TestSaveMemory_EpisodicKind(t *testing.T) {
	s := newTestStore()
	user := stableUserID("azure:user-a")
	tool := defSaveMemory(s, nil, "", user)

	_, err := tool.handler(context.Background(), map[string]any{
		"title": "session memory", "content": "we discussed the roadmap", "kind": "episodic",
	})
	require.NoError(t, err)
	_, err = tool.handler(context.Background(), map[string]any{
		"title": "standing note", "content": "a hypothesis", "kind": "garbage-mishear",
	})
	require.NoError(t, err)

	episodic := s.ListRecent("episodic", scopePrivate, user, 10)
	require.Len(t, episodic, 1, "kind=episodic should store exactly one episodic item")
	notes := s.List("note", scopePrivate, user, 10)
	require.Len(t, notes, 1, "an unrecognized kind must fall back to note")
}

// TestGetRecentMemory_ReturnsNewestWithTimestamp is the read side: get_recent_memory
// returns episodic items newest-first, each carrying created_at, and respects limit.
func TestGetRecentMemory_ReturnsNewestWithTimestamp(t *testing.T) {
	s := newTestStore()
	user := stableUserID("azure:user-a")
	note := defSaveMemory(s, nil, "", user)
	for _, title := range []string{"first", "second", "third"} {
		_, err := note.handler(context.Background(), map[string]any{"title": title, "content": "body " + title, "kind": "episodic"})
		require.NoError(t, err)
	}

	tool := defRecentMemory(s, user, 500, zerolog.Nop())
	out, err := tool.handler(context.Background(), map[string]any{"limit": float64(2)})
	require.NoError(t, err)

	var got struct {
		Count int `json:"count"`
		Items []struct {
			Title     string `json:"title"`
			Content   string `json:"content"`
			CreatedAt string `json:"created_at"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Equal(t, 2, got.Count, "limit=2 should cap the result")
	for _, it := range got.Items {
		assert.NotEmpty(t, it.CreatedAt, "each item must carry created_at (recency is the point)")
		assert.True(t, strings.Contains(it.CreatedAt, "T") && strings.HasSuffix(it.CreatedAt, "Z"), "created_at should be RFC3339 UTC: %q", it.CreatedAt)
	}
}

// TestGetRecentMemory_OnlyEpisodicNotNotes confirms get_recent_memory is scoped to
// episodic and does not surface plain notes (which get_agent_notes owns).
func TestGetRecentMemory_OnlyEpisodicNotNotes(t *testing.T) {
	s := newTestStore()
	user := stableUserID("azure:user-a")
	note := defSaveMemory(s, nil, "", user)
	_, err := note.handler(context.Background(), map[string]any{"title": "a note", "content": "standing observation"})
	require.NoError(t, err) // kind omitted → note
	_, err = note.handler(context.Background(), map[string]any{"title": "a session", "content": "episodic body", "kind": "episodic"})
	require.NoError(t, err)

	out, err := defRecentMemory(s, user, 0, zerolog.Nop()).handler(context.Background(), nil)
	require.NoError(t, err)
	assert.Contains(t, out, "episodic body", "episodic memory should be returned")
	assert.NotContains(t, out, "standing observation", "plain notes must not appear in get_recent_memory")
}
