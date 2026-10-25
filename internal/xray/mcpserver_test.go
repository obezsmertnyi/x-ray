package xray

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testMCPToken is the Bearer token the MCP test handlers are configured with.
// Fail-closed auth requires a non-empty token, so the helpers always send it.
const testMCPToken = "test-mcp-token"

// mcpInit performs the MCP initialize handshake and returns the session ID.
func mcpInit(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      0,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"clientInfo":      map[string]any{"name": "test", "version": "1.0"},
		},
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+testMCPToken)
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	return resp.Header.Get("Mcp-Session-Id")
}

// mcpCall sends a tools/call request and returns the parsed response body.
func mcpCall(t *testing.T, srv *httptest.Server, sessionID, toolName string, args map[string]any) map[string]any {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": toolName, "arguments": args},
	})
	require.NoError(t, err)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+testMCPToken)
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	w, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer w.Body.Close()
	require.Equal(t, http.StatusOK, w.StatusCode)
	var resp map[string]any
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	return resp
}

// mcpResultText extracts the text content from an MCP tool result.
func mcpResultText(t *testing.T, resp map[string]any) string {
	t.Helper()
	result, ok := resp["result"].(map[string]any)
	require.True(t, ok, "missing result: %v", resp)
	content, ok := result["content"].([]any)
	require.True(t, ok, "missing content")
	require.NotEmpty(t, content)
	item, ok := content[0].(map[string]any)
	require.True(t, ok)
	text, _ := item["text"].(string)
	return text
}

// ── upsert_memory private-scope subject validation ────────────────────────────

func TestMCPUpsertMemory_AgentNote_RequiresSubject(t *testing.T) {
	h := NewMCPServer(newTestStore(), zerolog.Nop(), func() string { return testMCPToken }, nil, nil, "", nil, 0, 0, 0)
	srv := httptest.NewServer(h)
	defer srv.Close()

	sessionID := mcpInit(t, srv)

	// Private scope with empty subject — must be rejected.
	resp := mcpCall(t, srv, sessionID, "upsert_memory", map[string]any{
		"kind":    "note",
		"scope":   "private",
		"title":   "Private note",
		"content": "secret",
		"subject": "",
	})
	text := mcpResultText(t, resp)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	assert.Equal(t, "subject is required when scope=private", out["error"])

	// Non-empty subject — must succeed.
	resp = mcpCall(t, srv, sessionID, "upsert_memory", map[string]any{
		"kind":    "note",
		"scope":   "private",
		"title":   "Private note",
		"content": "secret",
		"subject": "abc123hash",
	})
	text = mcpResultText(t, resp)
	out = nil
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	assert.NotContains(t, out, "error")
	assert.NotEmpty(t, out["id"])
}

// ── search_memory subject scoping ──────────────────────────────────────────

func TestMCPSearchKnowledge_AgentNoteRequiresSubject(t *testing.T) {
	store := newTestStore()
	userA := stableUserID("azure:user-a-oid")
	upsertItem(t, store, "", "note", "private", "Private note", "secret meeting plan", userA, nil)
	// A blog authored under a person's name — must be reachable without a subject.
	upsertItem(t, store, "", "stable_fact", "public", "LLM-as-Judge blog", "evaluating with llm as judge", "Vinayak Talikot", nil)

	h := NewMCPServer(store, zerolog.Nop(), func() string { return testMCPToken }, nil, nil, "", nil, 0, 0, 0)
	srv := httptest.NewServer(h)
	defer srv.Close()
	sessionID := mcpInit(t, srv)

	parse := func(resp map[string]any) map[string]any {
		var out map[string]any
		require.NoError(t, json.Unmarshal([]byte(mcpResultText(t, resp)), &out))
		return out
	}

	t.Run("kind=note without subject hides the private note", func(t *testing.T) {
		out := parse(mcpCall(t, srv, sessionID, "search_memory", map[string]any{
			"query": "secret",
			"kind":  "note",
		}))
		assert.EqualValues(t, 0, out["count"], "private note must not be returned when subject is empty")
	})

	t.Run("kind=note with subject returns own note", func(t *testing.T) {
		out := parse(mcpCall(t, srv, sessionID, "search_memory", map[string]any{
			"query":   "secret",
			"kind":    "note",
			"subject": userA,
		}))
		assert.EqualValues(t, 1, out["count"])
	})

	t.Run("shared search without subject reaches per-author blog", func(t *testing.T) {
		out := parse(mcpCall(t, srv, sessionID, "search_memory", map[string]any{
			"query": "llm as judge",
		}))
		items, _ := out["items"].([]any)
		require.NotEmpty(t, items, "blog authored under a person's name must be found with empty subject")
		first, _ := items[0].(map[string]any)
		assert.Equal(t, "LLM-as-Judge blog", first["title"])
	})
}

// ── event_time annotation (docs/todo/graphiti-comparison-refinement.md #1) ──

func TestMCPSearchMemory_EventTimeAnnotation(t *testing.T) {
	store := newTestStore()
	upsertEventItem(t, store, "", "event", "public", "Deploy widget-api", "content", "", "2023-05-30T00:00:00Z")
	upsertItem(t, store, "", "stable_fact", "public", "widget-api owns billing", "content", "", nil)

	h := NewMCPServer(store, zerolog.Nop(), func() string { return testMCPToken }, nil, nil, "", nil, 0, 0, 0)
	srv := httptest.NewServer(h)
	defer srv.Close()
	sessionID := mcpInit(t, srv)

	var out map[string]any
	resp := mcpCall(t, srv, sessionID, "search_memory", map[string]any{"query": "widget-api"})
	require.NoError(t, json.Unmarshal([]byte(mcpResultText(t, resp)), &out))
	items, _ := out["items"].([]any)
	require.Len(t, items, 2)

	var titles []string
	for _, raw := range items {
		it, _ := raw.(map[string]any)
		titles = append(titles, fmt.Sprintf("%v", it["title"]))
	}
	assert.Contains(t, titles, "Deploy widget-api (event_time: 2023-05-30)", "item with EventDate must carry the annotation")
	assert.Contains(t, titles, "widget-api owns billing", "item without EventDate must render its title unchanged")
}

func TestMCPUpsertMemory_EventDateValidation(t *testing.T) {
	store := newTestStore()
	h := NewMCPServer(store, zerolog.Nop(), func() string { return testMCPToken }, nil, nil, "", nil, 0, 0, 0)
	srv := httptest.NewServer(h)
	defer srv.Close()
	sessionID := mcpInit(t, srv)

	t.Run("invalid event_date rejected with a clear error", func(t *testing.T) {
		resp := mcpCall(t, srv, sessionID, "upsert_memory", map[string]any{
			"kind": "event", "scope": "public", "title": "Bad date", "content": "x",
			"event_date": "not-a-date",
		})
		var out map[string]any
		require.NoError(t, json.Unmarshal([]byte(mcpResultText(t, resp)), &out))
		errStr, _ := out["error"].(string)
		assert.Contains(t, errStr, "event_date")
		assert.NotContains(t, out, "id", "no item should be created on a rejected upsert")
	})

	t.Run("RFC3339 event_date accepted", func(t *testing.T) {
		resp := mcpCall(t, srv, sessionID, "upsert_memory", map[string]any{
			"kind": "event", "scope": "public", "title": "Good RFC3339", "content": "x",
			"event_date": "2026-06-15T09:00:00Z",
		})
		var out map[string]any
		require.NoError(t, json.Unmarshal([]byte(mcpResultText(t, resp)), &out))
		assert.NotContains(t, out, "error")
		assert.NotEmpty(t, out["id"])
	})

	t.Run("bare YYYY-MM-DD event_date accepted", func(t *testing.T) {
		resp := mcpCall(t, srv, sessionID, "upsert_memory", map[string]any{
			"kind": "event", "scope": "public", "title": "Good bare date", "content": "x",
			"event_date": "2026-06-15",
		})
		var out map[string]any
		require.NoError(t, json.Unmarshal([]byte(mcpResultText(t, resp)), &out))
		assert.NotContains(t, out, "error")
		assert.NotEmpty(t, out["id"])
	})

	t.Run("omitted event_date still accepted", func(t *testing.T) {
		resp := mcpCall(t, srv, sessionID, "upsert_memory", map[string]any{
			"kind": "note", "scope": "public", "title": "No date", "content": "x",
		})
		var out map[string]any
		require.NoError(t, json.Unmarshal([]byte(mcpResultText(t, resp)), &out))
		assert.NotContains(t, out, "error")
	})
}

// ── ADR-051: hard result caps ──────────────────────────────────────────────

func TestMCPListMemory_LimitHardCapped(t *testing.T) {
	const cap = 5
	store := newTestStore()
	store.SetWriteDedup(false)
	for i := 0; i < cap+20; i++ {
		upsertItem(t, store, "", "stable_fact", "public", fmt.Sprintf("Fact %d", i), "content", "", nil)
	}
	h := NewMCPServer(store, zerolog.Nop(), func() string { return testMCPToken }, nil, nil, "", nil, cap, 0, 0)
	srv := httptest.NewServer(h)
	defer srv.Close()
	sessionID := mcpInit(t, srv)

	var out map[string]any
	resp := mcpCall(t, srv, sessionID, "list_memory", map[string]any{
		"kind":  "stable_fact",
		"limit": float64(cap + 1000),
	})
	require.NoError(t, json.Unmarshal([]byte(mcpResultText(t, resp)), &out))
	assert.EqualValues(t, cap, out["count"], "limit must be capped to the configured ceiling regardless of what the caller requests")
}

func TestMCPListMemory_LimitDefaultsToConstantWhenUnconfigured(t *testing.T) {
	store := newTestStore()
	store.SetWriteDedup(false)
	for i := 0; i < DefaultMCPListMemoryMaxLimit+20; i++ {
		upsertItem(t, store, "", "stable_fact", "public", fmt.Sprintf("Fact %d", i), "content", "", nil)
	}
	h := NewMCPServer(store, zerolog.Nop(), func() string { return testMCPToken }, nil, nil, "", nil, 0, 0, 0)
	srv := httptest.NewServer(h)
	defer srv.Close()
	sessionID := mcpInit(t, srv)

	var out map[string]any
	resp := mcpCall(t, srv, sessionID, "list_memory", map[string]any{"kind": "stable_fact"})
	require.NoError(t, json.Unmarshal([]byte(mcpResultText(t, resp)), &out))
	assert.EqualValues(t, DefaultMCPListMemoryMaxLimit, out["count"], "0 passed to NewMCPServer must fall back to the default ceiling")
}

func TestMCPSearchMemory_LimitHardCapped(t *testing.T) {
	const cap = 3
	store := newTestStore()
	store.SetWriteDedup(false)    // the fixture is twenty restatements of one fact
	store.SetRecallShaping(false) // and this test pins the limit, not the collapse
	for i := 0; i < cap+20; i++ {
		upsertItem(t, store, "", "stable_fact", "public", fmt.Sprintf("Widget fact %d", i), "widget content", "", nil)
	}
	h := NewMCPServer(store, zerolog.Nop(), func() string { return testMCPToken }, nil, nil, "", nil, 0, cap, 0)
	srv := httptest.NewServer(h)
	defer srv.Close()
	sessionID := mcpInit(t, srv)

	var out map[string]any
	resp := mcpCall(t, srv, sessionID, "search_memory", map[string]any{
		"query": "widget",
		"limit": float64(cap + 1000),
	})
	require.NoError(t, json.Unmarshal([]byte(mcpResultText(t, resp)), &out))
	assert.EqualValues(t, cap, out["count"], "limit must be capped to the configured ceiling regardless of what the caller requests")
}

// ── ADR-051: get_memory orientation snapshot ───────────────────────────────

func TestMCPGetMemory_ReturnsCounts(t *testing.T) {
	store := newTestStore()
	upsertItem(t, store, "", "stable_fact", "public", "Fact one", "content", "", nil)
	upsertItem(t, store, "", "stable_fact", "public", "Fact two", "content", "", nil)
	upsertItem(t, store, "", "event", "public", "Event one", "content", "", nil)

	h := NewMCPServer(store, zerolog.Nop(), func() string { return testMCPToken }, nil, nil, "", nil, 0, 0, 0)
	srv := httptest.NewServer(h)
	defer srv.Close()
	sessionID := mcpInit(t, srv)

	var out map[string]any
	resp := mcpCall(t, srv, sessionID, "get_memory", map[string]any{})
	require.NoError(t, json.Unmarshal([]byte(mcpResultText(t, resp)), &out))
	assert.EqualValues(t, 3, out["total"])
	byKind, ok := out["by_kind"].(map[string]any)
	require.True(t, ok)
	assert.EqualValues(t, 2, byKind["stable_fact"])
	assert.EqualValues(t, 1, byKind["event"])
	assert.NotContains(t, out, "summary")
}

// ── ADR-051: delete_memory ownership check ─────────────────────────────────

func TestMCPDeleteMemory_PrivateItemRequiresMatchingSubject(t *testing.T) {
	store := newTestStore()
	id := upsertItem(t, store, "", "note", "private", "Private note", "secret", "owner-a", nil)

	h := NewMCPServer(store, zerolog.Nop(), func() string { return testMCPToken }, nil, nil, "", nil, 0, 0, 0)
	srv := httptest.NewServer(h)
	defer srv.Close()
	sessionID := mcpInit(t, srv)

	// Wrong/missing subject — must be rejected, item stays.
	var out map[string]any
	resp := mcpCall(t, srv, sessionID, "delete_memory", map[string]any{"id": id})
	require.NoError(t, json.Unmarshal([]byte(mcpResultText(t, resp)), &out))
	assert.Equal(t, "forbidden", out["error"])
	assert.NotNil(t, store.GetItem(id), "item must not be deleted when subject doesn't match")

	// Matching subject — succeeds.
	resp = mcpCall(t, srv, sessionID, "delete_memory", map[string]any{"id": id, "subject": "owner-a"})
	out = nil
	require.NoError(t, json.Unmarshal([]byte(mcpResultText(t, resp)), &out))
	assert.Equal(t, "deleted", out["status"])
	assert.Nil(t, store.GetItem(id))
}

func TestMCPDeleteMemory_PublicItemDeletableWithoutSubject(t *testing.T) {
	store := newTestStore()
	id := upsertItem(t, store, "", "stable_fact", "public", "Public fact", "content", "", nil)

	h := NewMCPServer(store, zerolog.Nop(), func() string { return testMCPToken }, nil, nil, "", nil, 0, 0, 0)
	srv := httptest.NewServer(h)
	defer srv.Close()
	sessionID := mcpInit(t, srv)

	var out map[string]any
	resp := mcpCall(t, srv, sessionID, "delete_memory", map[string]any{"id": id})
	require.NoError(t, json.Unmarshal([]byte(mcpResultText(t, resp)), &out))
	assert.Equal(t, "deleted", out["status"], "a public item should be deletable without a subject — unlike voice, MCP doesn't restrict delete to private-only")
}

// ── ADR-051: fetch_memory ──────────────────────────────────────────────────

func TestMCPFetchMemory_ReturnsRequestedItemsInOrder(t *testing.T) {
	store := newTestStore()
	id1 := upsertItem(t, store, "", "stable_fact", "public", "First", "content", "", nil)
	id2 := upsertItem(t, store, "", "stable_fact", "public", "Second", "content", "", nil)

	h := NewMCPServer(store, zerolog.Nop(), func() string { return testMCPToken }, nil, nil, "", nil, 0, 0, 0)
	srv := httptest.NewServer(h)
	defer srv.Close()
	sessionID := mcpInit(t, srv)

	var out map[string]any
	// Request in reverse order — result must preserve the requested order.
	resp := mcpCall(t, srv, sessionID, "fetch_memory", map[string]any{"ids": []any{id2, id1}})
	require.NoError(t, json.Unmarshal([]byte(mcpResultText(t, resp)), &out))
	assert.EqualValues(t, 2, out["count"])
	items, _ := out["items"].([]any)
	require.Len(t, items, 2)
	first, _ := items[0].(map[string]any)
	second, _ := items[1].(map[string]any)
	assert.Equal(t, "Second", first["title"])
	assert.Equal(t, "First", second["title"])
}

func TestMCPFetchMemory_MissingIDOmittedNotErrored(t *testing.T) {
	store := newTestStore()
	id1 := upsertItem(t, store, "", "stable_fact", "public", "Exists", "content", "", nil)

	h := NewMCPServer(store, zerolog.Nop(), func() string { return testMCPToken }, nil, nil, "", nil, 0, 0, 0)
	srv := httptest.NewServer(h)
	defer srv.Close()
	sessionID := mcpInit(t, srv)

	var out map[string]any
	resp := mcpCall(t, srv, sessionID, "fetch_memory", map[string]any{"ids": []any{id1, "does-not-exist"}})
	require.NoError(t, json.Unmarshal([]byte(mcpResultText(t, resp)), &out))
	assert.EqualValues(t, 1, out["count"], "a missing id must be silently omitted, not an error")
	assert.NotContains(t, out, "error")
}

func TestMCPFetchMemory_CrossSubjectPrivateItemOmitted(t *testing.T) {
	store := newTestStore()
	id := upsertItem(t, store, "", "note", "private", "Secret", "content", "owner-a", nil)

	h := NewMCPServer(store, zerolog.Nop(), func() string { return testMCPToken }, nil, nil, "", nil, 0, 0, 0)
	srv := httptest.NewServer(h)
	defer srv.Close()
	sessionID := mcpInit(t, srv)

	// No subject / wrong subject — item omitted, not an error that would confirm existence.
	var out map[string]any
	resp := mcpCall(t, srv, sessionID, "fetch_memory", map[string]any{"ids": []any{id}})
	require.NoError(t, json.Unmarshal([]byte(mcpResultText(t, resp)), &out))
	assert.EqualValues(t, 0, out["count"])

	resp = mcpCall(t, srv, sessionID, "fetch_memory", map[string]any{"ids": []any{id}, "subject": "owner-b"})
	out = nil
	require.NoError(t, json.Unmarshal([]byte(mcpResultText(t, resp)), &out))
	assert.EqualValues(t, 0, out["count"])

	// Matching subject — item returned.
	resp = mcpCall(t, srv, sessionID, "fetch_memory", map[string]any{"ids": []any{id}, "subject": "owner-a"})
	out = nil
	require.NoError(t, json.Unmarshal([]byte(mcpResultText(t, resp)), &out))
	assert.EqualValues(t, 1, out["count"])
}

// ── ADR-051: recent_memory ──────────────────────────────────────────────────

func TestMCPRecentMemory_NewestFirst(t *testing.T) {
	store := newTestStore()
	upsertItem(t, store, "older", "episodic", "private", "Older session", "content", "user-a", nil)
	time.Sleep(2 * time.Millisecond)
	upsertItem(t, store, "newer", "episodic", "private", "Newer session", "content", "user-a", nil)

	h := NewMCPServer(store, zerolog.Nop(), func() string { return testMCPToken }, nil, nil, "", nil, 0, 0, 0)
	srv := httptest.NewServer(h)
	defer srv.Close()
	sessionID := mcpInit(t, srv)

	var out map[string]any
	resp := mcpCall(t, srv, sessionID, "recent_memory", map[string]any{"subject": "user-a"})
	require.NoError(t, json.Unmarshal([]byte(mcpResultText(t, resp)), &out))
	items, _ := out["items"].([]any)
	require.Len(t, items, 2)
	first, _ := items[0].(map[string]any)
	assert.Equal(t, "Newer session", first["title"], "newest-first ordering")
}

func TestMCPRecentMemory_LimitHardCapped(t *testing.T) {
	const cap = 4
	store := newTestStore()
	for i := 0; i < cap+10; i++ {
		upsertItem(t, store, "", "episodic", "private", fmt.Sprintf("Session %d", i), "content", "user-a", nil)
	}

	h := NewMCPServer(store, zerolog.Nop(), func() string { return testMCPToken }, nil, nil, "", nil, 0, 0, cap)
	srv := httptest.NewServer(h)
	defer srv.Close()
	sessionID := mcpInit(t, srv)

	var out map[string]any
	resp := mcpCall(t, srv, sessionID, "recent_memory", map[string]any{"subject": "user-a", "limit": float64(cap + 1000)})
	require.NoError(t, json.Unmarshal([]byte(mcpResultText(t, resp)), &out))
	assert.EqualValues(t, cap, out["count"])
}
