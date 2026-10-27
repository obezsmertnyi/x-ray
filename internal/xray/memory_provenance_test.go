package xray

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	mcell "github.com/triageagent-dev/x-ray/v2/api/v1"
	"github.com/triageagent-dev/x-ray/v2/pkg/memstore"
)

func TestUpsert_OriginFromContext(t *testing.T) {
	s := newTestStore()
	ctx := context.Background()

	res, errMsg := s.Upsert(ctx, "n1", "note", "public", "T", "body one", "", "", "", nil, nil)
	require.Empty(t, errMsg)
	assert.Equal(t, OriginDerived, res.Item.Origin, "a write that names no person is derived")

	res, errMsg = s.Upsert(withWriteOrigin(ctx, OriginHuman), "n2", "note", "public", "T", "body two", "", "", "", nil, nil)
	require.Empty(t, errMsg)
	assert.Equal(t, OriginHuman, res.Item.Origin)

	_, errMsg = s.Upsert(ctx, "n2", "note", "public", "T", "rewritten by a model", "", "", "", nil, nil)
	assert.Equal(t, errDerivedReplacesHuman, errMsg, "a derived write must not replace a human cell")
	assert.Equal(t, "body two", s.GetItem("n2").Content)

	res, errMsg = s.Upsert(withWriteOrigin(ctx, OriginHuman), "n1", "note", "public", "T", "corrected", "", "", "", nil, nil)
	require.Empty(t, errMsg)
	assert.Equal(t, OriginHuman, res.Item.Origin, "a person may replace a derived cell")
}

func seedOrigin(s *MemoryStore, id, origin string, evidence []string, links []mcell.ThoughtLink) {
	s.Restore(context.Background(), &MemoryItem{
		ID: id, Kind: "stable_fact", Scope: "public", Title: id, Content: "content of " + id,
		Status: "active", Origin: origin, EvidenceRefs: evidence, Links: links,
	})
}

func TestRefute_RetiresCellAndCascadesToDerivedCitations(t *testing.T) {
	s := newTestStore()
	seedOrigin(s, "root", OriginDerived, nil, nil)
	seedOrigin(s, "cites-ref", OriginDerived, []string{"cell:root"}, nil)
	seedOrigin(s, "cites-link", OriginDerived, nil, []mcell.ThoughtLink{{ToID: "root", Relation: "causes"}})
	seedOrigin(s, "human-cites", OriginHuman, []string{"root"}, nil)
	seedOrigin(s, "observed-cites", OriginObserved, []string{"root"}, nil)
	seedOrigin(s, "two-hops", OriginDerived, []string{"cites-ref"}, nil)
	seedOrigin(s, "unrelated", OriginDerived, nil, nil)

	changed, found := s.Refute(context.Background(), "root", "measured otherwise", []string{"event-9"})
	require.True(t, found)

	var ids []string
	for _, c := range changed {
		ids = append(ids, c.ID)
		assert.Equal(t, "stale", c.Status)
	}
	assert.Equal(t, []string{"root", "cites-link", "cites-ref"}, ids, "the cell first, then the derived cells that cite it, one hop")

	for _, id := range []string{"human-cites", "observed-cites", "two-hops", "unrelated"} {
		assert.Equal(t, "active", s.GetItem(id).Status, id)
	}

	rec := s.GetItem("root").Data[memstore.RefutedDataKey]
	assert.Equal(t, "application/json", rec.Mime)
	raw, err := base64.StdEncoding.DecodeString(rec.Bytes)
	require.NoError(t, err)
	var got memstore.RefutedRecord
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, "measured otherwise", got.Reason)
	assert.Equal(t, []string{"event-9"}, got.Evidence)

	for _, hit := range s.Search(context.Background(), "content of root", "", "", 10) {
		assert.NotEqual(t, "root", hit.ID, "a refuted cell is not searchable")
	}
}

func TestRefute_UnknownID(t *testing.T) {
	_, found := newTestStore().Refute(context.Background(), "nope", "r", nil)
	assert.False(t, found)
}

func TestUpsert_WriteDedupMergesARestatedFact(t *testing.T) {
	s := newTestStore()
	ctx := context.Background()
	first, errMsg := s.Upsert(ctx, "", "stable_fact", "public", "Cache TTL",
		"The edge cache keeps responses for sixty seconds before revalidating against the origin server", "svc", "high", "", nil, []string{"cache"})
	require.Empty(t, errMsg)
	require.Equal(t, "created", first.Status)

	again, errMsg := s.Upsert(ctx, "", "stable_fact", "public", "Edge cache TTL",
		"The edge cache keeps responses for sixty seconds before revalidating against the origin", "svc", "high", "", nil, []string{"edge"})
	require.Empty(t, errMsg)
	assert.Equal(t, "merged", again.Status)
	assert.Equal(t, first.ID, again.ID, "the restatement lands on the cell it repeats")
	assert.ElementsMatch(t, []string{"cache", "edge"}, again.Item.Tags)
	assert.Equal(t, first.Item.CreatedAt, again.Item.CreatedAt)
	assert.Len(t, s.FilterItems("svc", "stable_fact"), 1)
}

func TestUpsert_WriteDedupBoundaries(t *testing.T) {
	ctx := context.Background()
	text := "The edge cache keeps responses for sixty seconds before revalidating against the origin server"

	t.Run("another subject is another fact", func(t *testing.T) {
		s := newTestStore()
		_, _ = s.Upsert(ctx, "", "stable_fact", "public", "T", text, "a", "", "", nil, nil)
		res, _ := s.Upsert(ctx, "", "stable_fact", "public", "T", text, "b", "", "", nil, nil)
		assert.Equal(t, "created", res.Status)
	})
	t.Run("another kind is another fact", func(t *testing.T) {
		s := newTestStore()
		_, _ = s.Upsert(ctx, "", "stable_fact", "public", "T", text, "", "", "", nil, nil)
		res, _ := s.Upsert(ctx, "", "hypothesis", "public", "T", text, "", "", "", nil, nil)
		assert.Equal(t, "created", res.Status)
	})
	t.Run("an explicit id of an existing cell is an update, never a merge", func(t *testing.T) {
		s := newTestStore()
		first, _ := s.Upsert(ctx, "", "stable_fact", "public", "T", text, "", "", "", nil, nil)
		other, _ := s.Upsert(ctx, "", "stable_fact", "public", "Deploys", "Deploys freeze on fridays after noon for every region we operate", "", "", "", nil, nil)
		res, _ := s.Upsert(ctx, other.ID, "stable_fact", "public", "Deploys", text, "", "", "", nil, nil)
		assert.Equal(t, "updated", res.Status, "the id names a cell, so it is rewritten even into a restatement of another")
		assert.Equal(t, other.ID, res.ID)
		assert.NotEqual(t, first.ID, res.ID)
	})
	t.Run("events and notes are never merged", func(t *testing.T) {
		s := newTestStore()
		_, _ = s.Upsert(ctx, "", "note", "public", "T", text, "", "", "", nil, nil)
		res, _ := s.Upsert(ctx, "", "note", "public", "T", text, "", "", "", nil, nil)
		assert.Equal(t, "created", res.Status)
	})
	t.Run("a model restatement does not overwrite a human fact", func(t *testing.T) {
		s := newTestStore()
		human, _ := s.Upsert(withWriteOrigin(ctx, OriginHuman), "", "stable_fact", "public", "T", text, "", "", "", nil, nil)
		res, _ := s.Upsert(ctx, "", "stable_fact", "public", "T", text, "", "", "", nil, nil)
		assert.Equal(t, "created", res.Status)
		assert.NotEqual(t, human.ID, res.ID)
		assert.Equal(t, OriginHuman, s.GetItem(human.ID).Origin)
	})
	t.Run("two plans that differ by a number stay two", func(t *testing.T) {
		s := newTestStore()
		_, _ = s.Upsert(ctx, "", "stable_fact", "public", "Q3 plan", "launch", "", "", "", nil, nil)
		res, _ := s.Upsert(ctx, "", "stable_fact", "public", "Q4 plan", "launch", "", "", "", nil, nil)
		assert.Equal(t, "created", res.Status)
	})
	t.Run("switched off", func(t *testing.T) {
		s := newTestStore()
		s.SetWriteDedup(false)
		_, _ = s.Upsert(ctx, "", "stable_fact", "public", "T", text, "", "", "", nil, nil)
		res, _ := s.Upsert(ctx, "", "stable_fact", "public", "T", text, "", "", "", nil, nil)
		assert.Equal(t, "created", res.Status)
	})
}

func TestConsolidateMemory(t *testing.T) {
	s := newTestStore()
	s.SetWriteDedup(false)
	ctx := context.Background()
	text := "The edge cache keeps responses for sixty seconds before revalidating against the origin server"
	a, _ := s.Upsert(ctx, "", "stable_fact", "public", "TTL", text, "", "medium", "", nil, nil)
	b, _ := s.Upsert(ctx, "", "stable_fact", "public", "TTL again", text, "", "high", "", nil, nil)
	other, _ := s.Upsert(ctx, "", "stable_fact", "public", "Other", "Deploys freeze on fridays after noon for every region we operate", "", "high", "", nil, nil)

	report, changed := s.ConsolidateMemory("", false)
	require.Len(t, report.Groups, 1)
	assert.Equal(t, 3, report.Scanned)
	assert.Equal(t, b.ID, report.Groups[0].Keep, "higher confidence is kept")
	assert.Equal(t, []string{a.ID}, report.Groups[0].Superseded)
	assert.False(t, report.Applied)
	assert.Empty(t, changed)
	assert.Equal(t, "active", s.GetItem(a.ID).Status, "a report writes nothing")

	report, changed = s.ConsolidateMemory("", true)
	assert.True(t, report.Applied)
	assert.Equal(t, 1, report.Superseded)
	assert.Equal(t, "superseded", s.GetItem(a.ID).Status)
	assert.Equal(t, []string{a.ID}, s.GetItem(b.ID).Supersedes)
	assert.Equal(t, "active", s.GetItem(other.ID).Status)
	assert.Len(t, changed, 2)

	again, _ := s.ConsolidateMemory("", true)
	assert.Empty(t, again.Groups, "a second sweep finds nothing")
}

func TestSearch_CollapsesNearDuplicatesAndPrefersActiveWatchItems(t *testing.T) {
	ctx := context.Background()
	text := "the edge cache keeps responses for sixty seconds before revalidating against the origin server"

	t.Run("near-duplicates fold into one hit", func(t *testing.T) {
		s := newTestStore()
		s.SetWriteDedup(false)
		_, _ = s.Upsert(ctx, "d1", "stable_fact", "public", "cache ttl", text, "", "", "", nil, nil)
		_, _ = s.Upsert(ctx, "d2", "stable_fact", "public", "cache ttl", text, "", "", "", nil, nil)
		_, _ = s.Upsert(ctx, "d3", "stable_fact", "public", "cache ttl", text, "", "", "", nil, nil)
		got := s.Search(ctx, "cache ttl", "", "", 10)
		require.Len(t, got, 1)
		assert.Equal(t, 2, got[0].Collapsed)

		s.SetRecallShaping(false)
		assert.Len(t, s.Search(ctx, "cache ttl", "", "", 10), 3)
	})

	t.Run("an active watch item outranks a plainly better match", func(t *testing.T) {
		s := newTestStore()
		_, _ = s.Upsert(ctx, "w", "watch_item", "public", "queue depth", "queue depth is climbing", "", "", "", nil, nil)
		_, _ = s.Upsert(ctx, "f", "operational_pattern", "public", "queue depth", "the queue depth alarm fires at five hundred messages", "", "", "", nil, nil)
		_, _ = s.Upsert(ctx, "g", "operational_pattern", "public", "queue", "depth of the queue is capped by the broker", "", "", "", nil, nil)
		got := s.Search(ctx, "queue depth", "", "", 10)
		require.NotEmpty(t, got)
		assert.Equal(t, "w", got[0].ID)
	})
}

// mcpCallBearer is mcpCall with the credential named by the caller.
func mcpCallBearer(t *testing.T, srv *httptest.Server, bearer, sessionID, tool string, args map[string]any) map[string]any {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
	require.NoError(t, err)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out
}

func TestMCP_OriginFollowsTheCredential(t *testing.T) {
	store := newTestStore()
	auth := newTestAuthorizer(t, true, nil)
	h := NewMCPServer(store, zerolog.Nop(), func() string { return testMCPToken }, auth, nil, "", nil, 0, 0, 0)
	srv := httptest.NewServer(h)
	defer srv.Close()

	person, err := auth.signToken("github:alice", []string{scopeRead, scopeWrite})
	require.NoError(t, err)

	write := func(bearer, id, content string) string {
		_, session := mcpInitWith(t, srv, bearer)
		resp := mcpCallBearer(t, srv, bearer, session, "upsert_memory", map[string]any{
			"id": id, "kind": "note", "scope": "public", "title": "T", "content": content,
		})
		return mcpResultText(t, resp)
	}

	write(person, "by-person", "written by a person")
	write(testMCPToken, "by-secret", "written with the shared secret")
	assert.Equal(t, OriginHuman, store.GetItem("by-person").Origin, "an OAuth token names a person")
	assert.Equal(t, OriginDerived, store.GetItem("by-secret").Origin, "the shared secret names nobody")

	out := write(testMCPToken, "by-person", "a model overwrites the person")
	assert.Contains(t, out, errDerivedReplacesHuman)
	assert.Equal(t, "written by a person", store.GetItem("by-person").Content)
}

func TestMCP_RefuteAndConsolidateTools(t *testing.T) {
	store := newTestStore()
	store.SetWriteDedup(false)
	h := NewMCPServer(store, zerolog.Nop(), func() string { return testMCPToken }, nil, nil, "", nil, 0, 0, 0)
	srv := httptest.NewServer(h)
	defer srv.Close()
	session := mcpInit(t, srv)

	mcpCall(t, srv, session, "upsert_memory", map[string]any{
		"id": "base", "kind": "stable_fact", "scope": "public", "title": "Base", "content": "the original claim", "confidence": "high",
	})
	mcpCall(t, srv, session, "upsert_memory", map[string]any{
		"id": "child", "kind": "hypothesis", "scope": "public", "title": "Child", "content": "drawn from the original",
		"evidence_refs": []any{"base"},
	})
	assert.Equal(t, []string{"base"}, store.GetItem("child").EvidenceRefs)

	var out map[string]any
	resp := mcpCall(t, srv, session, "refute_memory", map[string]any{"id": "base", "reason": "wrong"})
	require.NoError(t, json.Unmarshal([]byte(mcpResultText(t, resp)), &out))
	assert.Equal(t, "stale", out["status"])
	assert.Equal(t, []any{"child"}, out["cascaded"])
	assert.Equal(t, "stale", store.GetItem("child").Status)

	resp = mcpCall(t, srv, session, "refute_memory", map[string]any{"id": "base", "reason": " "})
	assert.Contains(t, mcpResultText(t, resp), "reason is required")
	resp = mcpCall(t, srv, session, "refute_memory", map[string]any{"id": "missing", "reason": "x"})
	assert.Contains(t, mcpResultText(t, resp), "not found")

	resp = mcpCall(t, srv, session, "consolidate_memory", map[string]any{})
	require.NoError(t, json.Unmarshal([]byte(mcpResultText(t, resp)), &out))
	assert.Equal(t, false, out["applied"])
}
