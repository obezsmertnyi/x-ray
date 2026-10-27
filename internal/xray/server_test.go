package xray

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	mcell "github.com/triageagent-dev/x-ray/v2/api/v1"
	"github.com/triageagent-dev/x-ray/v2/pkg/memstore/index"
)

// cancelAfter returns a context that cancels itself after a short delay,
// used to unblock SSE handlers in tests without hanging.
func cancelAfter(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, 50*time.Millisecond)
}

// ── test helpers ──────────────────────────────────────────────────────────────

func newTestServer(t *testing.T) *Server {
	t.Helper()
	mem := NewMemoryStore()
	return NewServer(zerolog.Nop(), mem, nil, GitHubOAuthConfig{}, AzureOAuthConfig{}, "", 0, nil)
}

// authedDo injects a valid session cookie so requests pass requireSession.
func authedDo(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	token := sessions.create("test-user", "github:1")
	var req *http.Request
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		req = httptest.NewRequest(method, path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.AddCookie(&http.Cookie{Name: "xray_session", Value: token})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func do(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		req = httptest.NewRequest(method, path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// ── /healthz ──────────────────────────────────────────────────────────────────

func TestServer_Healthz(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s.Handler(), "GET", "/healthz", nil)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "ok", w.Body.String())
}

// ── /v1/memory/metrics quota block ─────────────────────────────────────────────

func TestServer_Metrics_IncludesUserQuota(t *testing.T) {
	mem := NewMemoryStore()
	s := NewServer(zerolog.Nop(), mem, nil, GitHubOAuthConfig{}, AzureOAuthConfig{}, "", 0, nil)
	vm := &VoiceManager{memory: mem, config: &ConfigWatcher{}, userDailyAudioTokenLimit: 1000}
	s.SetVoiceManager(vm)

	rawUserID := "github:42"
	mem.AddUserTokens(stableUserID(rawUserID), 250)
	token := sessions.create("alice", rawUserID)
	t.Cleanup(func() { sessions.invalidate(token) })

	req := httptest.NewRequest("GET", "/v1/memory/metrics", nil)
	req.AddCookie(&http.Cookie{Name: "xray_session", Value: token})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Quota *struct {
			Used      int64 `json:"used"`
			Limit     int64 `json:"limit"`
			Remaining int64 `json:"remaining"`
		} `json:"quota"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.NotNil(t, resp.Quota, "metrics must include a quota block for the authenticated user")
	assert.Equal(t, int64(250), resp.Quota.Used)
	assert.Equal(t, int64(1000), resp.Quota.Limit)
	assert.Equal(t, int64(750), resp.Quota.Remaining)
}

func TestServer_Metrics_UnlimitedQuota(t *testing.T) {
	mem := NewMemoryStore()
	s := NewServer(zerolog.Nop(), mem, nil, GitHubOAuthConfig{}, AzureOAuthConfig{}, "", 0, nil)
	vm := &VoiceManager{memory: mem, config: &ConfigWatcher{}, userDailyAudioTokenLimit: 0} // unlimited
	s.SetVoiceManager(vm)

	token := sessions.create("bob", "github:7")
	t.Cleanup(func() { sessions.invalidate(token) })
	req := httptest.NewRequest("GET", "/v1/memory/metrics", nil)
	req.AddCookie(&http.Cookie{Name: "xray_session", Value: token})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Quota *struct {
			Limit int64 `json:"limit"`
		} `json:"quota"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.NotNil(t, resp.Quota)
	assert.Equal(t, int64(0), resp.Quota.Limit, "limit 0 signals unlimited to the UI")
}

// ── /v1/version ───────────────────────────────────────────────────────────────

func TestServer_Version(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s.Handler(), "GET", "/v1/version", nil)
	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Contains(t, resp, "version")
	// /v1/version is unauthenticated, so it must NOT leak build or auth-topology
	// recon data. Build commit/date and the OAuth org/team/tenant fields are
	// deliberately omitted; only the human-readable version string is exposed.
	for _, leaked := range []string{"commit", "buildDate", "github", "github_org", "github_team", "azure", "azure_tenant"} {
		assert.NotContains(t, resp, leaked, "%s must not be exposed on the unauthenticated /v1/version", leaked)
	}
}

func TestServer_Categories_Empty(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s.Handler(), "GET", "/v1/categories", nil)
	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Contains(t, resp, "categories")
	assert.Nil(t, resp["categories"])
}

func TestServer_Categories_FromConfig(t *testing.T) {
	mem := NewMemoryStore()
	cw := NewConfigWatcher(mem)
	s := NewServer(zerolog.Nop(), mem, nil, GitHubOAuthConfig{}, AzureOAuthConfig{}, "", 0, cw)
	cw.update(&mcell.MemoryCell{
		ObjectMeta: metav1.ObjectMeta{Name: SystemConfigCRName},
		Spec: mcell.MemoryCellSpec{
			Kind: "system_configuration",
			UI: &mcell.UIConfig{
				Categories: []mcell.CategoryConfig{
					{ID: "active-concerns", Label: "Active Concerns", Color: "#f87171", Kind: "active_concern", Tier: 0.6, Angle: 0},
				},
			},
		},
	})
	w := do(t, s.Handler(), "GET", "/v1/categories", nil)
	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	cats, ok := resp["categories"].([]any)
	require.True(t, ok)
	require.Len(t, cats, 1)
	cat := cats[0].(map[string]any)
	assert.Equal(t, "active-concerns", cat["id"])
	assert.Equal(t, "active_concern", cat["kind"])
}

func TestServer_AuthProviders(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s.Handler(), "GET", "/v1/auth/providers", nil)
	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	// Only booleans for the login UI — no org/team/tenant recon data.
	assert.Contains(t, resp, "github")
	assert.Contains(t, resp, "azure")
	for _, leaked := range []string{"github_org", "github_team", "azure_tenant"} {
		assert.NotContains(t, resp, leaked, "%s must not be exposed on the unauthenticated /v1/auth/providers", leaked)
	}
}

// ── /manifest.json ────────────────────────────────────────────────────────────

func TestServer_Manifest(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s.Handler(), "GET", "/manifest.json", nil)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "manifest")
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "X-Ray", resp["name"])
	assert.Equal(t, "X-Ray", resp["short_name"])
}

// ── /v1/memory (public) ───────────────────────────────────────────────────────

func TestServer_MemoryList_Empty(t *testing.T) {
	s := newTestServer(t)
	w := authedDo(t, s.Handler(), "GET", "/v1/memory", nil)
	assert.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Items []*MemoryItem `json:"items"`
		Total int           `json:"total"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 0, resp.Total)
}

func TestServer_MemoryList_WithItems(t *testing.T) {
	s := newTestServer(t)
	s.memory.Upsert(t.Context(), "", "stable_fact", "public", "Alpha", "content", "xray", "", "", nil, nil) //nolint:errcheck
	s.memory.Upsert(t.Context(), "", "stable_fact", "public", "Beta", "content", "xray", "", "", nil, nil)  //nolint:errcheck

	w := authedDo(t, s.Handler(), "GET", "/v1/memory", nil)
	assert.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Items []*MemoryItem `json:"items"`
		Total int           `json:"total"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 2, resp.Total)
}

func TestServer_MemoryList_KindFilter(t *testing.T) {
	s := newTestServer(t)
	s.memory.Upsert(t.Context(), "", "stable_fact", "public", "Fact", "c", "", "", "", nil, nil)     //nolint:errcheck
	s.memory.Upsert(t.Context(), "", "recent_change", "public", "Change", "c", "", "", "", nil, nil) //nolint:errcheck

	w := authedDo(t, s.Handler(), "GET", "/v1/memory?kind=stable_fact", nil)
	assert.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Items []*MemoryItem `json:"items"`
		Total int           `json:"total"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.Total)
	assert.Equal(t, "stable_fact", resp.Items[0].Kind)
}

func TestServer_MemoryGet_Found(t *testing.T) {
	s := newTestServer(t)
	res, _ := s.memory.Upsert(t.Context(), "", "stable_fact", "public", "Hello", "world", "", "", "", nil, nil)

	w := authedDo(t, s.Handler(), "GET", "/v1/memory/"+res.ID, nil)
	assert.Equal(t, http.StatusOK, w.Code)
	var item MemoryItem
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &item))
	assert.Equal(t, res.ID, item.ID)
	assert.Equal(t, "Hello", item.Title)
}

func TestServer_MemoryGet_NotFound(t *testing.T) {
	s := newTestServer(t)
	w := authedDo(t, s.Handler(), "GET", "/v1/memory/nonexistent", nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestServer_MemoryGet_ImageURL(t *testing.T) {
	s := newTestServer(t)
	imgData := map[string]mcell.DataEntry{"image": {Bytes: "aW1nZGF0YQ==", Mime: "image/jpeg"}}
	res, _ := s.memory.Upsert(t.Context(), "", "stable_fact", "public", "With Image", "body", "", "", "", imgData, nil)

	w := authedDo(t, s.Handler(), "GET", "/v1/memory/"+res.ID, nil)
	assert.Equal(t, http.StatusOK, w.Code)
	var item MemoryItem
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &item))
	assert.Equal(t, "/v1/memory/"+res.ID+"/image", item.ImageURL)
}

// ── /v1/memory/item?id=... ──────────────────────────────────────────────────
//
// GET /v1/memory/{id} cannot reliably carry an id containing a literal '/'
// (it splits the request into more path segments than the {id} pattern
// expects) — this is the query-parameter alternative, which round-trips any
// id since '/' inside a query VALUE is never treated as a path separator.

func TestServer_MemoryGetItem_SlashInID(t *testing.T) {
	s := newTestServer(t)
	res, _ := s.memory.Upsert(t.Context(), "inc-claim|gcp|events/gcp-status/compute-engine", "stable_fact", "public", "Slashy", "world", "", "", "", nil, nil)

	w := authedDo(t, s.Handler(), "GET", "/v1/memory/item?id="+url.QueryEscape(res.ID), nil)
	assert.Equal(t, http.StatusOK, w.Code)
	var item MemoryItem
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &item))
	assert.Equal(t, res.ID, item.ID)
	assert.Equal(t, "Slashy", item.Title)
}

func TestServer_MemoryGetItem_PipeInID(t *testing.T) {
	s := newTestServer(t)
	res, _ := s.memory.Upsert(t.Context(), "a|b|c", "stable_fact", "public", "Piped", "world", "", "", "", nil, nil)

	w := authedDo(t, s.Handler(), "GET", "/v1/memory/item?id="+url.QueryEscape(res.ID), nil)
	assert.Equal(t, http.StatusOK, w.Code)
	var item MemoryItem
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &item))
	assert.Equal(t, res.ID, item.ID)
}

func TestServer_MemoryGetItem_SpaceInID(t *testing.T) {
	s := newTestServer(t)
	res, _ := s.memory.Upsert(t.Context(), "an id with spaces", "stable_fact", "public", "Spacey", "world", "", "", "", nil, nil)

	w := authedDo(t, s.Handler(), "GET", "/v1/memory/item?id="+url.QueryEscape(res.ID), nil)
	assert.Equal(t, http.StatusOK, w.Code)
	var item MemoryItem
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &item))
	assert.Equal(t, res.ID, item.ID)
}

func TestServer_MemoryGetItem_UnicodeInID(t *testing.T) {
	s := newTestServer(t)
	res, _ := s.memory.Upsert(t.Context(), "café-日本語-🎉", "stable_fact", "public", "Unicode", "world", "", "", "", nil, nil)

	w := authedDo(t, s.Handler(), "GET", "/v1/memory/item?id="+url.QueryEscape(res.ID), nil)
	assert.Equal(t, http.StatusOK, w.Code)
	var item MemoryItem
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &item))
	assert.Equal(t, res.ID, item.ID)
}

func TestServer_MemoryGetItem_NotFound(t *testing.T) {
	s := newTestServer(t)
	w := authedDo(t, s.Handler(), "GET", "/v1/memory/item?id=nonexistent", nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestServer_MemoryGetItem_PrivateNotFound(t *testing.T) {
	s := newTestServer(t)
	res, _ := s.memory.Upsert(t.Context(), "", "note", "private", "Secret", "shh", "alice", "", "", nil, nil)

	w := authedDo(t, s.Handler(), "GET", "/v1/memory/item?id="+url.QueryEscape(res.ID), nil)
	assert.Equal(t, http.StatusNotFound, w.Code, "private note must return 404 on /v1/memory/item too")
}

// ── /v1/memory/{id}/image ────────────────────────────────────────────────────

func TestServer_MemoryImage_NotFound(t *testing.T) {
	s := newTestServer(t)
	w := authedDo(t, s.Handler(), "GET", "/v1/memory/missing/image", nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// ── requireSession middleware ─────────────────────────────────────────────────

func TestServer_RequireSession_PublicPaths(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()

	// Non-streaming public paths — no auth needed.
	staticPaths := []string{
		"/v1/mobile/auth",
		"/v1/mobile/auth/validate",
		"/v1/version",
		"/healthz",
		"/manifest.json",
		"/sw.js",
	}
	for _, path := range staticPaths {
		t.Run(path, func(t *testing.T) {
			w := do(t, h, "GET", path, nil)
			assert.NotEqual(t, http.StatusUnauthorized, w.Code, "public path should not return 401")
		})
	}
}

func TestServer_MemoryEvents_RequiresAuth(t *testing.T) {
	// Auth must be enabled (non-empty ClientID) for requireSession to enforce 401.
	mem := NewMemoryStore()
	s := NewServer(zerolog.Nop(), mem, nil,
		GitHubOAuthConfig{ClientID: "test-client"},
		AzureOAuthConfig{}, "", 0, nil)

	req := httptest.NewRequest("GET", "/v1/memory/events", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestServer_MemoryEvents_WithAuth(t *testing.T) {
	// Auth must be enabled so requireSession is active; cookie lets the request through.
	mem := NewMemoryStore()
	s := NewServer(zerolog.Nop(), mem, nil,
		GitHubOAuthConfig{ClientID: "test-client"},
		AzureOAuthConfig{}, "", 0, nil)
	token := sessions.create("test-user", "github:1")

	ctx, cancel := cancelAfter(t.Context())
	defer cancel()

	req := httptest.NewRequest("GET", "/v1/memory/events", nil).WithContext(ctx)
	req.AddCookie(&http.Cookie{Name: "xray_session", Value: token})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	assert.NotEqual(t, http.StatusUnauthorized, w.Code)
}

func TestServer_RequireSession_ProtectedPath(t *testing.T) {
	mem := NewMemoryStore()
	s := NewServer(zerolog.Nop(), mem, nil,
		GitHubOAuthConfig{ClientID: "test-client"},
		AzureOAuthConfig{}, "", 0, nil)
	w := do(t, s.Handler(), "GET", "/v1/memory", nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestServer_RequireSession_TokenViaCookie(t *testing.T) {
	s := newTestServer(t)
	token := sessions.create("alice", "github:2")

	req := httptest.NewRequest("GET", "/v1/memory", nil)
	req.AddCookie(&http.Cookie{Name: "xray_session", Value: token})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestServer_RequireSession_TokenViaBearer(t *testing.T) {
	s := newTestServer(t)
	token := sessions.create("alice", "github:2")

	req := httptest.NewRequest("GET", "/v1/memory", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

// ── embedded UI ────────────────────────────────────────────────────────────────

func TestServer_ServesTheEmbeddedUIAndKeepsTheAPI(t *testing.T) {
	ui := fstest.MapFS{
		"index.html": {Data: []byte(`<title>home</title><script>boot()</script>`)},
		"login.html": {Data: []byte(`<title>login</title>`)},
	}
	s := NewServer(zerolog.Nop(), NewMemoryStore(), ui, GitHubOAuthConfig{ClientID: "test-client"}, AzureOAuthConfig{}, "", 0, nil)
	h := s.Handler()
	do := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}

	home := do("/")
	assert.Equal(t, http.StatusOK, home.Code)
	assert.Contains(t, home.Body.String(), "<title>home</title>")
	// A document's script is allowed by hash, never by 'unsafe-inline'.
	assert.Contains(t, home.Header().Get("Content-Security-Policy"), "script-src 'self' blob: 'sha256-")
	assert.NotContains(t, home.Header().Get("Content-Security-Policy"), "script-src 'self' 'unsafe-inline'")

	assert.Contains(t, do("/login").Body.String(), "<title>login</title>")
	assert.Contains(t, do("/k/events").Body.String(), "<title>home</title>", "a client-side route gets the shell")

	// The API, health and PWA routes are not the UI's.
	assert.Equal(t, http.StatusOK, do("/healthz").Code)
	assert.NotContains(t, do("/v1/version").Body.String(), "<title>")
	assert.Equal(t, http.StatusUnauthorized, do("/v1/memory").Code, "the UI is public, the API is not")
}

func TestServer_WithoutAUIServesNoneAtRoot(t *testing.T) {
	w := httptest.NewRecorder()
	newTestServer(t).Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// ── /sw.js ────────────────────────────────────────────────────────────────────

func TestServer_ServiceWorker(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s.Handler(), "GET", "/sw.js", nil)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "javascript")
	assert.True(t, strings.Contains(w.Body.String(), "install"))
}

// ── private-note REST isolation ───────────────────────────────────────────────

func TestServer_MemoryList_NoAgentNote(t *testing.T) {
	s := newTestServer(t)
	s.memory.Upsert(t.Context(), "", "note", "private", "Private", "secret", "abc123", "", "", nil, nil) //nolint:errcheck
	s.memory.Upsert(t.Context(), "", "stable_fact", "public", "Public", "open", "", "", "", nil, nil)    //nolint:errcheck

	w := authedDo(t, s.Handler(), "GET", "/v1/memory", nil)
	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Items []*MemoryItem `json:"items"`
		Total int           `json:"total"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.Total, "private note must be excluded from /v1/memory list")
	for _, it := range resp.Items {
		assert.NotEqual(t, "private", it.Scope)
	}
}

func TestServer_MemorySummary_NoAgentNote(t *testing.T) {
	s := newTestServer(t)
	s.memory.Upsert(t.Context(), "", "note", "private", "Private", "secret", "abc123", "", "", nil, nil) //nolint:errcheck
	s.memory.Upsert(t.Context(), "", "stable_fact", "public", "Public", "open", "", "", "", nil, nil)    //nolint:errcheck

	w := authedDo(t, s.Handler(), "GET", "/v1/memory/summary", nil)
	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Items []MemoryItemSummary `json:"items"`
		Total int                 `json:"total"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.Total, "total must reflect filtered count")
	for _, it := range resp.Items {
		assert.NotEqual(t, "private", it.Scope)
	}
}

func TestServer_MemoryGet_AgentNote_Returns404(t *testing.T) {
	s := newTestServer(t)
	// Subject belongs to a different user — shared endpoint must deny.
	res, _ := s.memory.Upsert(t.Context(), "", "note", "private", "Private", "secret", "other-user", "", "", nil, nil)

	w := authedDo(t, s.Handler(), "GET", "/v1/memory/"+res.ID, nil)
	assert.Equal(t, http.StatusNotFound, w.Code, "private note must return 404 on /v1/memory/{id}")
}

func TestServer_UserNotes_GetByID_UnauthenticatedReturns404(t *testing.T) {
	s := newTestServer(t)
	// A signed-out caller reads no one else's note. (With auth off, which only
	// --dev allows, the caller is the anonymous user and reads that user's own
	// notes: TestServer_UserNotes_AuthDisabled_ShowsTheAnonymousUsersNotes.)
	res, _ := s.memory.Upsert(t.Context(), "", "note", "private", "Ghost", "content", stableUserID("github:1"), "", "", nil, nil)

	req := httptest.NewRequest("GET", "/v1/user/notes/"+res.ID, nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code, "another user's note must be 404")
}

func TestServer_UserNotes_GetByID_OwnerCanRead(t *testing.T) {
	s := newTestServer(t)
	// authedDo sessions use userID="github:1"; stableUserID("github:1") is the subject.
	ownerSubject := stableUserID("github:1")
	res, _ := s.memory.Upsert(t.Context(), "", "note", "private", "My Note", "private content", ownerSubject, "", "", nil, nil)

	// Owner reads via /v1/user/notes/{id} — must succeed.
	w := authedDo(t, s.Handler(), "GET", "/v1/user/notes/"+res.ID, nil)
	assert.Equal(t, http.StatusOK, w.Code, "owner must be able to read their private note via /v1/user/notes/{id}")

	// Non-owner (different session) must get 404 on the same path.
	token2 := sessions.create("other-user", "github:2")
	req := httptest.NewRequest("GET", "/v1/user/notes/"+res.ID, nil)
	req.AddCookie(&http.Cookie{Name: "xray_session", Value: token2})
	w2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(w2, req)
	assert.Equal(t, http.StatusNotFound, w2.Code, "non-owner must get 404 on /v1/user/notes/{id}")
}

func TestServer_UserNotes_List_ReturnsOwnerNotes(t *testing.T) {
	s := newTestServer(t)
	ownerSubject := stableUserID("github:1")
	otherSubject := stableUserID("github:2")

	s.memory.Upsert(t.Context(), "", "note", "private", "My Note", "content", ownerSubject, "", "", nil, nil)
	s.memory.Upsert(t.Context(), "", "note", "private", "Other Note", "content", otherSubject, "", "", nil, nil)

	w := authedDo(t, s.Handler(), "GET", "/v1/user/notes", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Items []MemoryItem `json:"items"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Len(t, resp.Items, 1, "must return only the owner's note")
	assert.Equal(t, ownerSubject, resp.Items[0].Subject)
	assert.Equal(t, "My Note", resp.Items[0].Title)
}

func TestServer_UserNotes_List_UnauthenticatedReturnsEmpty(t *testing.T) {
	s := newTestServer(t)
	s.memory.Upsert(t.Context(), "", "note", "private", "My Note", "content", stableUserID("github:1"), "", "", nil, nil)

	req := httptest.NewRequest("GET", "/v1/user/notes", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Items []MemoryItem `json:"items"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Empty(t, resp.Items, "unauthenticated must get empty list")
}

// ── /v1/memory/neighbors ────────────────────────────────────────────────────

// newNeighborServer is a server whose store has an embedding index seeded with
// hand-made vectors, so no embedder is involved.
func newNeighborServer(t *testing.T) *Server {
	t.Helper()
	mem := NewMemoryStore()
	mem.cells = index.NewCellStore(nil, zerolog.Nop())
	seed := func(id, kind, scope string, v ...float32) {
		mem.Restore(context.Background(), &MemoryItem{ID: id, Kind: kind, Scope: scope, Title: id, Content: id, Status: "active"})
		mem.cells.SeedFromEmbedding(index.IndexEntry{ID: id, Kind: kind, Scope: scope, Title: id, Status: "active"}, v)
	}
	seed("centre", "stable_fact", "public", 1, 0, 0, 0)
	seed("near-a", "stable_fact", "public", 0.9, 0.1, 0, 0)
	seed("near-b", "stable_fact", "public", 0.9, 0, 0.1, 0)
	seed("far", "stable_fact", "public", 0, 0, 0, 1)
	seed("secret", "note", "private", 0.95, 0.05, 0, 0)
	return NewServer(zerolog.Nop(), mem, nil, GitHubOAuthConfig{}, AzureOAuthConfig{}, "", 0, nil)
}

func TestMemoryNeighbors_ReturnsSharedNeighboursNearestFirst(t *testing.T) {
	s := newNeighborServer(t)
	w := authedDo(t, s.Handler(), "GET", "/v1/memory/neighbors?id=centre&top=3", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var got MemoryNeighborhood
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.True(t, got.Indexed)
	require.Len(t, got.Neighbors, 3)
	for _, n := range got.Neighbors {
		assert.NotEqual(t, "centre", n.ID, "the centre is not its own neighbour")
		assert.NotEqual(t, "secret", n.ID, "a private cell never appears")
	}
	assert.Contains(t, []string{"near-a", "near-b"}, got.Neighbors[0].ID)
	assert.Equal(t, "far", got.Neighbors[2].ID)
	assert.GreaterOrEqual(t, got.Neighbors[0].Score, got.Neighbors[1].Score)
}

func TestMemoryNeighbors_UnknownAndPrivateCentreAre404(t *testing.T) {
	s := newNeighborServer(t)
	assert.Equal(t, http.StatusNotFound, authedDo(t, s.Handler(), "GET", "/v1/memory/neighbors?id=nope", nil).Code)
	assert.Equal(t, http.StatusNotFound, authedDo(t, s.Handler(), "GET", "/v1/memory/neighbors?id=secret", nil).Code)
}

func TestMemoryNeighbors_KeywordOnlyStoreIsNotIndexed(t *testing.T) {
	s := newTestServer(t)
	res, _ := s.memory.Upsert(context.Background(), "", "stable_fact", "public", "T", "body", "", "", "", nil, nil)
	w := authedDo(t, s.Handler(), "GET", "/v1/memory/neighbors?id="+url.QueryEscape(res.ID), nil)
	require.Equal(t, http.StatusOK, w.Code)
	var got MemoryNeighborhood
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.False(t, got.Indexed)
	assert.Empty(t, got.Neighbors)
}

// With sign-in off (--dev) every request without a session is the built-in dev
// user, so the voice session's notes and the UI's notes list are the same set.
func TestServer_UserNotes_AuthDisabled_ShowsTheDevUsersNotes(t *testing.T) {
	s := newTestServer(t)
	dev := stableUserID(devUserID)
	_, _ = s.memory.Upsert(t.Context(), "", "note", "private", "Mine", "saved by voice", dev, "", "", nil, nil)
	_, _ = s.memory.Upsert(t.Context(), "", "note", "private", "Theirs", "someone else", stableUserID("github:1"), "", "", nil, nil)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/user/notes", nil))
	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Items []MemoryItem `json:"items"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Items, 1)
	assert.Equal(t, "Mine", resp.Items[0].Title)

	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/user/notes/"+resp.Items[0].ID, nil))
	assert.Equal(t, http.StatusOK, w.Code)

	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/auth/me", nil))
	assert.JSONEq(t, `{"username":"dev","userID":"dev"}`, w.Body.String())
}

// Behind a sign-in provider no dev user exists: a request with no session is
// refused, and a real session keeps its own identity.
func TestServer_DevUser_OnlyWithSignInOff(t *testing.T) {
	prod := NewServer(zerolog.Nop(), NewMemoryStore(), nil, GitHubOAuthConfig{ClientID: "test-client"}, AzureOAuthConfig{}, "", 0, nil)
	w := httptest.NewRecorder()
	prod.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/user/notes", nil))
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	req := httptest.NewRequest("GET", "/v1/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: "xray_session", Value: sessions.create("alice", "github:1")})
	w = httptest.NewRecorder()
	prod.Handler().ServeHTTP(w, req)
	assert.JSONEq(t, `{"username":"alice","userID":"github:1"}`, w.Body.String())

	// A dev server keeps a session it is given instead of replacing it.
	dev := newTestServer(t)
	req = httptest.NewRequest("GET", "/v1/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: "xray_session", Value: sessions.create("alice", "github:1")})
	w = httptest.NewRecorder()
	dev.Handler().ServeHTTP(w, req)
	assert.JSONEq(t, `{"username":"alice","userID":"github:1"}`, w.Body.String())
}
