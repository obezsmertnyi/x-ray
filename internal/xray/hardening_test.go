package xray

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	mcell "github.com/triageagent-dev/x-ray/v2/api/v1"
)

// ── C2: WebSocket origin check (CSWSH) ────────────────────────────────────────

func TestWSUpgrader_CheckOrigin(t *testing.T) {
	up := newWSUpgrader("https://xray.example.com")

	cases := []struct {
		name   string
		origin string
		want   bool
	}{
		{"matching origin", "https://xray.example.com", true},
		{"matching origin with path ignored", "https://xray.example.com", true},
		{"no origin header (non-browser)", "", true},
		{"cross-site attacker", "https://evil.example.com", false},
		{"scheme mismatch", "http://xray.example.com", false},
		{"port mismatch", "https://xray.example.com:8443", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/v1/voice", nil)
			if c.origin != "" {
				r.Header.Set("Origin", c.origin)
			}
			assert.Equal(t, c.want, up.CheckOrigin(r))
		})
	}
}

func TestWSUpgrader_EmptyAllowedOrigin_AllowsAny(t *testing.T) {
	up := newWSUpgrader("") // dev mode
	r := httptest.NewRequest(http.MethodGet, "/v1/voice", nil)
	r.Header.Set("Origin", "https://evil.example.com")
	assert.True(t, up.CheckOrigin(r), "empty allowedOrigin disables the check (dev)")
}

func TestSameOrigin(t *testing.T) {
	assert.True(t, sameOrigin("https://A.example.com", "https://a.example.com"), "host is case-insensitive")
	assert.True(t, sameOrigin("HTTPS://a.example.com", "https://a.example.com"), "scheme is case-insensitive")
	assert.False(t, sameOrigin("https://a.example.com", "https://b.example.com"))
	assert.False(t, sameOrigin("://bad", "https://a.example.com"))
}

// ── C1: MCP auth fails closed ─────────────────────────────────────────────────

func TestMCPServer_FailsClosedWhenTokenUnset(t *testing.T) {
	h := NewMCPServer(newTestStore(), zerolog.Nop(), func() string { return "" }, nil, nil, "", nil, 0, 0, 0)
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	// No token configured → must refuse, not serve unauthenticated.
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

func TestMCPServer_RejectsWrongToken(t *testing.T) {
	h := NewMCPServer(newTestStore(), zerolog.Nop(), func() string { return "right-token" }, nil, nil, "", nil, 0, 0, 0)
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer wrong-token")
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// ── H3: image MIME allowlist ──────────────────────────────────────────────────

func TestAllowedImageMime(t *testing.T) {
	cases := map[string]string{
		"":                "image/jpeg",
		"image/jpeg":      "image/jpeg",
		"image/jpg":       "image/jpeg",
		"IMAGE/PNG":       "image/png",
		" image/webp ":    "image/webp",
		"image/gif":       "image/gif",
		"text/html":       "",
		"image/svg+xml":   "",
		"application/xml": "",
	}
	for in, want := range cases {
		assert.Equal(t, want, allowedImageMime(in), "mime %q", in)
	}
}

func TestServer_ImageEndpoint_RejectsScriptableMime(t *testing.T) {
	mem := NewMemoryStore()
	// Store an item with an HTML payload masquerading as an image.
	mem.Restore(context.Background(), &MemoryItem{
		ID:   "evil",
		Kind: "system_identity",
		Data: map[string]mcell.DataEntry{
			"image": {Mime: "text/html", Bytes: "PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg=="},
		},
	})
	s := NewServer(zerolog.Nop(), mem, nil, GitHubOAuthConfig{}, AzureOAuthConfig{}, "", 0, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/memory/evil/image", nil)
	s.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnsupportedMediaType, rec.Code)
}

func TestServer_ImageEndpoint_SetsNosniff(t *testing.T) {
	mem := NewMemoryStore()
	mem.Restore(context.Background(), &MemoryItem{
		ID:   "ok",
		Kind: "system_identity",
		Data: map[string]mcell.DataEntry{
			"image": {Mime: "image/png", Bytes: "aGVsbG8="},
		},
	})
	s := NewServer(zerolog.Nop(), mem, nil, GitHubOAuthConfig{}, AzureOAuthConfig{}, "", 0, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/memory/ok/image", nil)
	s.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "image/png", rec.Header().Get("Content-Type"))
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
}

// ── M1: security headers ──────────────────────────────────────────────────────

func TestServer_SecurityHeaders(t *testing.T) {
	mem := NewMemoryStore()
	s := NewServer(zerolog.Nop(), mem, nil, GitHubOAuthConfig{}, AzureOAuthConfig{}, "", 0, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/version", nil)
	s.Handler().ServeHTTP(rec, req)

	h := rec.Header()
	assert.Equal(t, "nosniff", h.Get("X-Content-Type-Options"))
	assert.Equal(t, "DENY", h.Get("X-Frame-Options"))
	csp := h.Get("Content-Security-Policy")
	assert.Contains(t, csp, "default-src 'self'")
	assert.Contains(t, csp, "object-src 'none'")
	assert.Contains(t, csp, "base-uri 'self'")
	assert.Contains(t, csp, "script-src 'self' 'unsafe-inline'")
	assert.Contains(t, csp, "frame-ancestors 'none'")
	assert.NotEmpty(t, h.Get("Referrer-Policy"))
	assert.Empty(t, h.Get("Strict-Transport-Security"), "HSTS only over HTTPS")
}

func TestServer_HSTS_OnForwardedHTTPS(t *testing.T) {
	mem := NewMemoryStore()
	s := NewServer(zerolog.Nop(), mem, nil, GitHubOAuthConfig{}, AzureOAuthConfig{}, "", 0, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/version", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	s.Handler().ServeHTTP(rec, req)

	assert.Contains(t, rec.Header().Get("Strict-Transport-Security"), "max-age=")
}

// ── H2: request body size limit ───────────────────────────────────────────────

func TestServer_LimitBody_CapsBodyRead(t *testing.T) {
	mem := NewMemoryStore()
	s := NewServer(zerolog.Nop(), mem, nil, GitHubOAuthConfig{}, AzureOAuthConfig{}, "", 16, nil)

	// limitBody wraps an inner handler that reads the full body; with a 16-byte
	// cap a larger body must fail the read.
	var readErr error
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/anything", strings.NewReader(strings.Repeat("x", 1024)))
	s.limitBody(inner).ServeHTTP(rec, req)

	require.Error(t, readErr, "reading past the cap must error")
	assert.Contains(t, readErr.Error(), "request body too large")
}

func TestServer_LimitBody_ExemptsVoiceWebSocket(t *testing.T) {
	mem := NewMemoryStore()
	s := NewServer(zerolog.Nop(), mem, nil, GitHubOAuthConfig{}, AzureOAuthConfig{}, "", 16, nil)

	var n int
	var readErr error
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		n, readErr = len(b), err
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/voice", strings.NewReader(strings.Repeat("x", 1024)))
	s.limitBody(inner).ServeHTTP(rec, req)

	require.NoError(t, readErr, "/v1/voice is exempt from the body cap")
	assert.Equal(t, 1024, n)
}

func TestMemoryStore_Upsert_RejectsOversizedContent(t *testing.T) {
	s := NewMemoryStore()
	huge := strings.Repeat("a", maxContentChars+1)
	_, errMsg := s.Upsert(t.Context(), "", "stable_fact", "global", "t", huge, "subj", "", "", nil, nil)
	assert.Contains(t, errMsg, "content exceeds")
}

func TestMemoryStore_Upsert_RejectsOversizedImage(t *testing.T) {
	s := NewMemoryStore()
	data := map[string]mcell.DataEntry{
		"image": {Mime: "image/jpeg", Bytes: strings.Repeat("A", maxImageBase64Chars+1)},
	}
	_, errMsg := s.Upsert(t.Context(), "", "stable_fact", "global", "t", "ok", "subj", "", "", data, nil)
	assert.Contains(t, errMsg, "image exceeds")
}

// ── H1: rate limiting ─────────────────────────────────────────────────────────

func TestKeyedRateLimiter_AllowsBurstThenThrottles(t *testing.T) {
	l := newKeyedRateLimiter(time.Hour, 3) // burst 3, then ~never refills in-test

	allowed := 0
	for i := 0; i < 10; i++ {
		if l.allow("user-a") {
			allowed++
		}
	}
	assert.Equal(t, 3, allowed, "only the burst is allowed before refill")

	// A different key has its own bucket.
	assert.True(t, l.allow("user-b"))
}

func TestKeyedRateLimiter_ZeroIntervalDisabled(t *testing.T) {
	l := newKeyedRateLimiter(0, 1)
	for i := 0; i < 100; i++ {
		assert.True(t, l.allow("k"), "zero interval = unlimited")
	}
}

func TestKeyedRateLimiter_NilIsDisabled(t *testing.T) {
	var l *keyedRateLimiter
	assert.True(t, l.allow("k"), "nil limiter allows everything")
}

// ── clientIP spoof-resistance ─────────────────────────────────────────────────

// withTrustedProxyHops sets the package-level hop count for one test and
// restores it afterward (the var is process-global).
func withTrustedProxyHops(t *testing.T, n int) {
	t.Helper()
	prev := trustedProxyHops
	SetTrustedProxyHops(n)
	t.Cleanup(func() { trustedProxyHops = prev })
}

func TestClientIP_NoTrustedProxy_IgnoresXFF(t *testing.T) {
	withTrustedProxyHops(t, 0)
	// With no trusted proxy, a spoofed XFF must be ignored — key on the TCP peer.
	got := clientIP("1.2.3.4, 5.6.7.8", "9.9.9.9", "10.0.0.1:54321")
	assert.Equal(t, "10.0.0.1", got, "spoofable XFF/X-Real-IP ignored; peer host used (port stripped)")
}

func TestClientIP_SpoofedXFFCannotChangeKey(t *testing.T) {
	withTrustedProxyHops(t, 0)
	// The whole point: a client sending a different XFF per request must NOT get
	// a fresh rate-limit bucket. Same peer → same key regardless of XFF.
	a := clientIP("attacker-1", "", "10.0.0.1:1111")
	b := clientIP("attacker-2", "", "10.0.0.1:2222")
	assert.Equal(t, a, b, "same peer keeps one bucket no matter the forged XFF")
}

func TestClientIP_OneTrustedProxy_UsesRightmostTrustedHop(t *testing.T) {
	withTrustedProxyHops(t, 1)
	// One trusted hop appends the real client. The client may forge everything to
	// the left; only the last entry (added by our proxy) is trusted.
	got := clientIP("spoofed, spoofed2, 203.0.113.7", "", "10.0.0.1:1111")
	assert.Equal(t, "203.0.113.7", got, "trusts the entry the outermost proxy appended")
}

func TestClientIP_TrustedProxy_FallsBackToPeerWhenNoXFF(t *testing.T) {
	withTrustedProxyHops(t, 1)
	got := clientIP("", "", "10.0.0.1:1234")
	assert.Equal(t, "10.0.0.1", got, "no forwarded header → peer host")
}
