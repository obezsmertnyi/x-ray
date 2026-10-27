package xray

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	mcell "github.com/triageagent-dev/x-ray/v2/api/v1"
)

func TestValidateWrite(t *testing.T) {
	assert.Empty(t, ValidateWrite("note", ""))
	assert.Empty(t, ValidateWrite("stable_fact", "high"))
	assert.Contains(t, ValidateWrite("stable_fact", "medium"), "confidence=high")
	assert.Contains(t, ValidateWrite("agent_note", ""), "not one of")
	assert.Contains(t, ValidateWrite("episodic", ""), "not one of", "server-written kinds are not writable through upsert")
	assert.Contains(t, ValidateWrite("", ""), "required")

	assert.Empty(t, ValidateSearchKind(""))
	assert.Empty(t, ValidateSearchKind("episodic"))
	assert.Contains(t, ValidateSearchKind("system_configuration"), "not a searchable kind")
	assert.Contains(t, ValidateSearchKind("fact"), "not a searchable kind")
}

func TestExpire(t *testing.T) {
	s := newTestStore()
	upsertItem(t, s, "old-note", "note", "public", "Old", "an out of date note", "", nil)
	got := s.Expire("old-note")
	require.NotNil(t, got)
	assert.Equal(t, "stale", got.Status)
	assert.Nil(t, s.Get("old-note"), "a stale item is not returned as active")
	assert.Empty(t, s.KeywordSearch("out of date note", "", "", 5))
	assert.Nil(t, s.Expire("missing"))
}

func TestSupersede(t *testing.T) {
	s := newTestStore()
	s.SetWriteDedup(false)
	ctx := context.Background()
	upsertItem(t, s, "ttl-v1", "stable_fact", "public", "Cache TTL", "sixty seconds", "", nil)

	changed, errMsg := s.Supersede(ctx, "ttl-v1", "ttl-v2", "stable_fact", "public", "Cache TTL", "ninety seconds", "", "high", nil)
	require.Empty(t, errMsg)
	require.Len(t, changed, 2)
	assert.Equal(t, "ttl-v2", changed[0].ID)
	assert.Equal(t, []string{"ttl-v1"}, changed[0].Supersedes)
	assert.Equal(t, "superseded", s.GetItem("ttl-v1").Status)
	assert.Equal(t, "active", s.GetItem("ttl-v2").Status)

	_, errMsg = s.Supersede(ctx, "missing", "x", "note", "public", "T", "c", "", "", nil)
	assert.Equal(t, "not_found", errMsg)
	_, errMsg = s.Supersede(ctx, "ttl-v2", "ttl-v2", "note", "public", "T", "c", "", "", nil)
	assert.Contains(t, errMsg, "different id")
}

func TestSupersede_DerivedCannotReplaceHuman(t *testing.T) {
	s := newTestStore()
	ctx := context.Background()
	_, errMsg := s.Upsert(withWriteOrigin(ctx, OriginHuman), "mine", "note", "public", "T", "a person wrote this", "", "", "", nil, nil)
	require.Empty(t, errMsg)
	_, errMsg = s.Supersede(ctx, "mine", "theirs", "note", "public", "T", "a model rewrote it", "", "", nil)
	assert.Equal(t, errDerivedReplacesHuman, errMsg)
	assert.Equal(t, "active", s.GetItem("mine").Status, "nothing was changed")
	assert.Nil(t, s.GetItem("theirs"))

	changed, errMsg := s.Supersede(withWriteOrigin(ctx, OriginHuman), "mine", "mine-v2", "note", "public", "T", "a person revised it", "", "", nil)
	require.Empty(t, errMsg)
	assert.Equal(t, OriginHuman, changed[0].Origin)
}

func TestContext_SpreadsActivationOverLinks(t *testing.T) {
	s := newTestStore()
	seed := func(id, subject string, links ...mcell.ThoughtLink) {
		seedLinkedItem(s, id, "stable_fact", "public", id+" title", "content "+id, subject, "active", links)
	}
	seed("a", "checkout", mcell.ThoughtLink{ToID: "b", Weight: 1}, mcell.ThoughtLink{ToID: "weak", Weight: 0.1})
	seed("b", "", mcell.ThoughtLink{ToID: "c", Weight: 1})
	seed("c", "", mcell.ThoughtLink{ToID: "d", Weight: 1})
	seed("d", "")
	seed("weak", "")
	seed("island", "other")
	seedLinkedItem(s, "secret", "note", "private", "secret", "x", "checkout", "active", nil)

	anchors, activated := s.Context("checkout", 2)
	require.Len(t, anchors, 1, "the private cell never resolves as an anchor")
	assert.Equal(t, "a", anchors[0].ID)
	var ids []string
	for _, a := range activated {
		ids = append(ids, a.Item.ID)
	}
	assert.Equal(t, []string{"b", "c", "weak"}, ids, "depth 2 stops before d; best score first")
	assert.InDelta(t, 0.6, activated[0].Score, 1e-6)
	assert.InDelta(t, 0.36, activated[1].Score, 1e-6)

	_, deeper := s.Context("a", 8)
	assert.Len(t, deeper, 4, "an exact id anchors; depth 8 reaches d")

	none, _ := s.Context("no-such-anchor", 3)
	assert.Empty(t, none)
}

func TestMCP_ExpireSupersedeContextAndKindValidation(t *testing.T) {
	store := newTestStore()
	store.SetWriteDedup(false)
	h := NewMCPServer(store, zerolog.Nop(), func() string { return testMCPToken }, nil, nil, "", nil, 0, 0, 0)
	srv := httptest.NewServer(h)
	defer srv.Close()
	session := mcpInit(t, srv)
	call := func(tool string, args map[string]any) string {
		return mcpResultText(t, mcpCall(t, srv, session, tool, args))
	}

	assert.Contains(t, call("upsert_memory", map[string]any{"kind": "fact", "scope": "public", "title": "T", "content": "c"}), "not one of")
	assert.Contains(t, call("upsert_memory", map[string]any{"kind": "stable_fact", "scope": "public", "title": "T", "content": "c", "confidence": "low"}), "confidence=high")
	assert.Contains(t, call("search_memory", map[string]any{"query": "x", "kind": "fact"}), "not a searchable kind")
	assert.Contains(t, call("list_memory", map[string]any{"kind": "fact"}), "not a searchable kind")

	call("upsert_memory", map[string]any{"id": "a", "kind": "note", "scope": "public", "title": "A", "content": "first"})
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(call("supersede_memory", map[string]any{
		"old_id": "a", "new_id": "a2", "kind": "note", "scope": "public", "title": "A", "content": "second",
	})), &out))
	assert.Equal(t, "a2", out["id"])
	assert.Equal(t, "superseded", store.GetItem("a").Status)
	assert.Equal(t, []string{"a"}, store.GetItem("a2").Supersedes)

	require.NoError(t, json.Unmarshal([]byte(call("expire_memory", map[string]any{"id": "a2"})), &out))
	assert.Equal(t, "stale", out["status"])
	assert.Contains(t, call("expire_memory", map[string]any{"id": "nope"}), "not found")

	seedLinkedItem(store, "hub", "stable_fact", "public", "Hub", "centre", "payments", "active", []mcell.ThoughtLink{{ToID: "leaf", Weight: 1}})
	seedLinkedItem(store, "leaf", "stable_fact", "public", "Leaf", "edge", "", "active", nil)
	require.NoError(t, json.Unmarshal([]byte(call("get_memory_context", map[string]any{"anchor": "payments"})), &out))
	assert.EqualValues(t, 2, out["count"])
	assert.Contains(t, call("get_memory_context", map[string]any{"anchor": "nothing-here"}), "no cells matched")
}
