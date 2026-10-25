package xray

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	mcell "github.com/triageagent-dev/x-ray/v2/api/v1"
)

const (
	testIssuer   = "https://xray.test"
	testResource = "https://xray.test/mcp"
)

func newTestAuthorizer(t *testing.T, dev bool, writeAllowed []string) *mcpAuthorizer {
	t.Helper()
	// nil seed ⇒ random per-process Ed25519 key, fine for tests that don't assert
	// cross-instance key agreement.
	a, err := newMCPAuthorizer(testIssuer, testResource, 15*time.Minute, dev, writeAllowed, nil, zerolog.Nop())
	require.NoError(t, err)
	return a
}

// pkcePair returns a verifier and its S256 challenge.
func pkcePair() (verifier, challenge string) {
	verifier = randB64(32)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return
}

// registerClient runs DCR for the given redirect_uri and returns the client_id.
func registerClient(t *testing.T, a *mcpAuthorizer, redirectURI string) string {
	t.Helper()
	body := `{"client_name":"test","redirect_uris":["` + redirectURI + `"]}`
	rec := httptest.NewRecorder()
	a.handleRegister(rec, httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body)))
	require.Equal(t, http.StatusCreated, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	id, _ := resp["client_id"].(string)
	require.NotEmpty(t, id)
	return id
}

// ── token round-trip + validation ────────────────────────────────────────────

func TestMCPAuth_SignAndValidate(t *testing.T) {
	a := newTestAuthorizer(t, true, nil)
	tok, err := a.signToken("user:1", []string{scopeRead, scopeWrite})
	require.NoError(t, err)

	sub, scopes, ok := a.validateMCPToken(tok)
	require.True(t, ok)
	assert.Equal(t, "user:1", sub)
	assert.ElementsMatch(t, []string{scopeRead, scopeWrite}, scopes)
}

func TestMCPAuth_ValidateRejectsWrongAudience(t *testing.T) {
	// Same signing key, but the token's aud is a different resource. Audience
	// binding (RFC 8707) must reject it independently of the signature.
	a := newTestAuthorizer(t, true, nil)
	mint := &mcpAuthorizer{issuer: a.issuer, resource: "https://other.test/mcp", key: a.key, kid: a.kid}
	tok, err := mint.signToken("user:1", []string{scopeRead})
	require.NoError(t, err)

	_, _, ok := a.validateMCPToken(tok)
	assert.False(t, ok, "token with aud=other.test must be rejected for resource=xray.test/mcp")
}

func TestMCPAuth_ValidateRejectsWrongIssuer(t *testing.T) {
	// Same key + aud, but a different issuer must be rejected.
	a := newTestAuthorizer(t, true, nil)
	mint := &mcpAuthorizer{issuer: "https://evil.test", resource: a.resource, key: a.key, kid: a.kid}
	tok, err := mint.signToken("user:1", []string{scopeRead})
	require.NoError(t, err)

	_, _, ok := a.validateMCPToken(tok)
	assert.False(t, ok, "token with iss=evil.test must be rejected")
}

func TestMCPAuth_ValidateRejectsForeignKey(t *testing.T) {
	// Same iss/aud but signed by a different key → signature check fails. Use
	// explicitly distinct seeds so the two keys are guaranteed to differ.
	a, err := newMCPAuthorizer(testIssuer, testResource, 15*time.Minute, true, nil, mcpSigningSeed("secret-a"), zerolog.Nop())
	require.NoError(t, err)
	b, err := newMCPAuthorizer(testIssuer, testResource, 15*time.Minute, true, nil, mcpSigningSeed("secret-b"), zerolog.Nop())
	require.NoError(t, err)
	tok, err := b.signToken("user:1", []string{scopeRead})
	require.NoError(t, err)
	_, _, ok := a.validateMCPToken(tok)
	assert.False(t, ok, "token signed by a different key must be rejected")
}

// ── ADR-050: deterministic, replica-shared signing key ───────────────────────

func TestMCPSigningSeed_StableAndSeparated(t *testing.T) {
	// Same secrets ⇒ same seed (so every replica derives the same key).
	s1 := mcpSigningSeed("gh-secret", "az-secret")
	s2 := mcpSigningSeed("gh-secret", "az-secret")
	require.Len(t, s1, 32, "seed must be ed25519.SeedSize")
	assert.Equal(t, s1, s2, "identical secrets must yield identical seed")

	// Different secrets ⇒ different seed.
	assert.NotEqual(t, s1, mcpSigningSeed("gh-secret", "other"), "different secrets must yield a different seed")

	// No secret ⇒ nil (caller falls back to a random key).
	assert.Nil(t, mcpSigningSeed("", ""), "empty secrets must yield nil seed")

	// Cryptographically separated from a same-secret SHA-256 that lacks the MCP
	// purpose label — proving the label (not just the secret) shapes the key, so it
	// can't collide with sessionHMACKey, which uses a different label over the same
	// secret material.
	bare := sha256.Sum256(append([]byte("gh-secret"), 0))
	assert.NotEqual(t, bare[:], mcpSigningSeed("gh-secret"),
		"the purpose label must change the derived seed (separation from other uses of the same secret)")
}

func TestMCPAuth_DerivedKeySharedAcrossReplicas(t *testing.T) {
	// Two authorizers built from the SAME seed model two replicas mounting the
	// same Secret. A token signed by one must validate on the other — this is the
	// multi-replica case ADR-050 fixes.
	seed := mcpSigningSeed("shared-oauth-secret")
	repA, err := newMCPAuthorizer(testIssuer, testResource, 15*time.Minute, true, nil, seed, zerolog.Nop())
	require.NoError(t, err)
	repB, err := newMCPAuthorizer(testIssuer, testResource, 15*time.Minute, true, nil, seed, zerolog.Nop())
	require.NoError(t, err)

	tok, err := repA.signToken("user:1", []string{scopeRead, scopeWrite})
	require.NoError(t, err)

	sub, scopes, ok := repB.validateMCPToken(tok)
	require.True(t, ok, "replica B must validate a token signed by replica A")
	assert.Equal(t, "user:1", sub)
	assert.ElementsMatch(t, []string{scopeRead, scopeWrite}, scopes)
}

func TestMCPAuth_DerivedKidIsDeterministic(t *testing.T) {
	// The kid must be identical across replicas (derived from the public key), so
	// JWKS and the JWT header kid agree no matter which pod a client hits.
	seed := mcpSigningSeed("shared-oauth-secret")
	repA, err := newMCPAuthorizer(testIssuer, testResource, 15*time.Minute, true, nil, seed, zerolog.Nop())
	require.NoError(t, err)
	repB, err := newMCPAuthorizer(testIssuer, testResource, 15*time.Minute, true, nil, seed, zerolog.Nop())
	require.NoError(t, err)
	assert.Equal(t, repA.kid, repB.kid, "kid must be deterministic across replicas")
	assert.NotEmpty(t, repA.kid)
}

func TestMCPAuth_JWKSPublishesEd25519(t *testing.T) {
	a := newTestAuthorizer(t, true, nil)
	rec := httptest.NewRecorder()
	a.jwks(rec, httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		Keys []map[string]any `json:"keys"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Keys, 1)
	k := resp.Keys[0]
	assert.Equal(t, "OKP", k["kty"])
	assert.Equal(t, "Ed25519", k["crv"])
	assert.Equal(t, "EdDSA", k["alg"])
	assert.Equal(t, a.kid, k["kid"])
	assert.NotEmpty(t, k["x"], "must publish the public key bytes")
}

func TestMCPAuth_ValidateRejectsExpired(t *testing.T) {
	a, err := newMCPAuthorizer(testIssuer, testResource, -time.Minute, true, nil, nil, zerolog.Nop())
	require.NoError(t, err)
	tok, err := a.signToken("user:1", []string{scopeRead})
	require.NoError(t, err)
	_, _, ok := a.validateMCPToken(tok)
	assert.False(t, ok, "expired token must be rejected")
}

func TestMCPAuth_ValidateRejectsGarbage(t *testing.T) {
	a := newTestAuthorizer(t, true, nil)
	_, _, ok := a.validateMCPToken("not.a.jwt")
	assert.False(t, ok)
}

// ── discovery metadata ─────────────────────────────────────────────────────────

func TestMCPAuth_ProtectedResourceMetadata(t *testing.T) {
	a := newTestAuthorizer(t, true, nil)
	rec := httptest.NewRecorder()
	a.protectedResourceMetadata(rec, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var m map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &m))
	assert.Equal(t, testResource, m["resource"])
	servers, _ := m["authorization_servers"].([]any)
	require.Len(t, servers, 1)
	assert.Equal(t, testIssuer, servers[0])
}

func TestMCPAuth_AuthServerMetadata(t *testing.T) {
	a := newTestAuthorizer(t, true, nil)
	rec := httptest.NewRecorder()
	a.authServerMetadata(rec, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var m map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &m))
	assert.Equal(t, testIssuer, m["issuer"])
	assert.Equal(t, testIssuer+"/authorize", m["authorization_endpoint"])
	assert.Equal(t, testIssuer+"/token", m["token_endpoint"])
	methods, _ := m["code_challenge_methods_supported"].([]any)
	assert.Contains(t, methods, "S256")
}

// ── /authorize + /token (PKCE) ─────────────────────────────────────────────────

// runAuthorizeToken drives the dev-mode authorize→token flow and returns the
// token JSON response.
func runAuthorizeToken(t *testing.T, a *mcpAuthorizer, verifier, challenge string) *httptest.ResponseRecorder {
	t.Helper()
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("redirect_uri", "http://localhost:9999/cb")
	q.Set("state", "s1")
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("resource", testResource)
	authRec := httptest.NewRecorder()
	a.handleAuthorize(authRec, httptest.NewRequest(http.MethodGet, "/authorize?"+q.Encode(), nil))
	require.Equal(t, http.StatusFound, authRec.Code, "authorize should 302")
	loc, err := url.Parse(authRec.Header().Get("Location"))
	require.NoError(t, err)
	code := loc.Query().Get("code")
	require.NotEmpty(t, code)
	assert.Equal(t, "s1", loc.Query().Get("state"))

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("code_verifier", verifier)
	form.Set("resource", testResource)
	req := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokRec := httptest.NewRecorder()
	a.handleToken(tokRec, req)
	return tokRec
}

func TestMCPAuth_AuthorizeTokenFlow(t *testing.T) {
	a := newTestAuthorizer(t, true, nil)
	verifier, challenge := pkcePair()
	rec := runAuthorizeToken(t, a, verifier, challenge)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	tok, _ := resp["access_token"].(string)
	require.NotEmpty(t, tok)
	assert.Equal(t, "Bearer", resp["token_type"])

	sub, scopes, ok := a.validateMCPToken(tok)
	require.True(t, ok)
	assert.Equal(t, "dev:local", sub)
	assert.ElementsMatch(t, []string{scopeRead, scopeWrite}, scopes)
}

func TestMCPAuth_TokenRejectsBadVerifier(t *testing.T) {
	a := newTestAuthorizer(t, true, nil)
	_, challenge := pkcePair()
	rec := runAuthorizeToken(t, a, "wrong-verifier", challenge)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "PKCE mismatch must be rejected")
}

func TestMCPAuth_AuthCodeSingleUse(t *testing.T) {
	a := newTestAuthorizer(t, true, nil)
	verifier, challenge := pkcePair()

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("redirect_uri", "http://localhost:9999/cb")
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	authRec := httptest.NewRecorder()
	a.handleAuthorize(authRec, httptest.NewRequest(http.MethodGet, "/authorize?"+q.Encode(), nil))
	loc, _ := url.Parse(authRec.Header().Get("Location"))
	code := loc.Query().Get("code")

	exchange := func() int {
		form := url.Values{}
		form.Set("grant_type", "authorization_code")
		form.Set("code", code)
		form.Set("code_verifier", verifier)
		req := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		a.handleToken(rec, req)
		return rec.Code
	}
	assert.Equal(t, http.StatusOK, exchange(), "first exchange succeeds")
	assert.Equal(t, http.StatusBadRequest, exchange(), "replayed code must be rejected")
}

func TestMCPAuth_AuthorizeRequiresPKCE(t *testing.T) {
	a := newTestAuthorizer(t, true, nil)
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("redirect_uri", "http://localhost:9999/cb")
	// no code_challenge
	rec := httptest.NewRecorder()
	a.handleAuthorize(rec, httptest.NewRequest(http.MethodGet, "/authorize?"+q.Encode(), nil))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestMCPAuth_AuthorizeRedirectsWhenNotLoggedIn(t *testing.T) {
	// Non-dev, registered client, no session cookie → redirect to /login.
	a := newTestAuthorizer(t, false, nil)
	clientID := registerClient(t, a, "http://localhost:9999/cb")
	_, challenge := pkcePair()
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", "http://localhost:9999/cb")
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	rec := httptest.NewRecorder()
	a.handleAuthorize(rec, httptest.NewRequest(http.MethodGet, "/authorize?"+q.Encode(), nil))
	require.Equal(t, http.StatusFound, rec.Code)
	assert.True(t, strings.HasPrefix(rec.Header().Get("Location"), "/login"), "must redirect to login")
}

func TestMCPAuth_AuthorizeUsesExistingSession(t *testing.T) {
	// Non-dev, registered client + valid xray_session cookie for an ALLOWLISTED
	// user → issues a code bound to that user (pre-login model). MCP is admin-only,
	// so the session's userHash must be on the allowlist.
	a := newTestAuthorizer(t, false, []string{stableUserID("github:42")})
	clientID := registerClient(t, a, "http://localhost:9999/cb")
	sessTok := sessions.create("alice", "github:42")
	defer sessions.invalidate(sessTok)

	_, challenge := pkcePair()
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", "http://localhost:9999/cb")
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	req := httptest.NewRequest(http.MethodGet, "/authorize?"+q.Encode(), nil)
	req.AddCookie(&http.Cookie{Name: "xray_session", Value: sessTok})
	rec := httptest.NewRecorder()
	a.handleAuthorize(rec, req)
	require.Equal(t, http.StatusFound, rec.Code)
	loc, _ := url.Parse(rec.Header().Get("Location"))
	code := loc.Query().Get("code")
	require.NotEmpty(t, code, "logged-in user must get an authorization code")

	// The code carries the session user as sub and full admin scopes.
	a.mu.Lock()
	ac := a.codes[code]
	a.mu.Unlock()
	assert.Equal(t, "github:42", ac.sub)
	assert.ElementsMatch(t, allMCPScopes, ac.scopes)
}

func TestMCPAuth_AuthorizeDeniesNonAllowlistedUser(t *testing.T) {
	// A logged-in user NOT on the MCP admin allowlist is denied a token
	// (access_denied), never issued a code — MCP is admin-only.
	a := newTestAuthorizer(t, false, []string{"some-other-admin-hash"})
	clientID := registerClient(t, a, "http://localhost:9999/cb")
	sessTok := sessions.create("bob", "github:99") // hash not on the allowlist
	defer sessions.invalidate(sessTok)

	_, challenge := pkcePair()
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", "http://localhost:9999/cb")
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", "xyz")
	req := httptest.NewRequest(http.MethodGet, "/authorize?"+q.Encode(), nil)
	req.AddCookie(&http.Cookie{Name: "xray_session", Value: sessTok})
	rec := httptest.NewRecorder()
	a.handleAuthorize(rec, req)

	require.Equal(t, http.StatusFound, rec.Code)
	loc, err := url.Parse(rec.Header().Get("Location"))
	require.NoError(t, err)
	// Redirects back to the client with access_denied (RFC 6749 §4.1.2.1), no code.
	assert.Equal(t, "http://localhost:9999", loc.Scheme+"://"+loc.Host)
	assert.Equal(t, "access_denied", loc.Query().Get("error"))
	assert.Equal(t, "xyz", loc.Query().Get("state"))
	assert.Empty(t, loc.Query().Get("code"), "denied user must not receive a code")

	// No code was minted.
	a.mu.Lock()
	n := len(a.codes)
	a.mu.Unlock()
	assert.Zero(t, n, "no authorization code should be stored for a denied user")
}

func TestMCPAuth_AuthorizeRejectsUnknownClient(t *testing.T) {
	// Non-dev: an unregistered client_id presenting a redirect_uri open DCR would
	// NOT have accepted (a public host, here) must still be rejected (fail closed,
	// #2) — unchanged by the self-heal feature, which only ever admits a
	// DCR-acceptable (loopback/custom-scheme) redirect_uri. See
	// TestMCPAuth_AuthorizeSelfHealsUnknownClientWithLoopbackRedirect /
	// …WithCustomScheme for the cases that now legitimately proceed instead of
	// rejecting.
	a := newTestAuthorizer(t, false, nil)
	_, challenge := pkcePair()
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", "never-registered")
	q.Set("redirect_uri", "https://evil.example.com/cb")
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	rec := httptest.NewRecorder()
	a.handleAuthorize(rec, httptest.NewRequest(http.MethodGet, "/authorize?"+q.Encode(), nil))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "unknown client_id + non-DCR-acceptable redirect_uri must be rejected in prod")
}

func TestMCPAuth_AuthorizeRejectsMissingClientInProd(t *testing.T) {
	// Non-dev: an empty client_id must be rejected (fail closed, #2).
	a := newTestAuthorizer(t, false, nil)
	_, challenge := pkcePair()
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("redirect_uri", "http://localhost:9999/cb")
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	rec := httptest.NewRecorder()
	a.handleAuthorize(rec, httptest.NewRequest(http.MethodGet, "/authorize?"+q.Encode(), nil))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "missing client_id must be rejected in prod")
}

// ── dynamic client registration (RFC 7591) ─────────────────────────────────────

func TestMCPAuth_RegisterReturnsClientID(t *testing.T) {
	a := newTestAuthorizer(t, true, nil)
	body := `{"client_name":"Claude Code","redirect_uris":["http://localhost:3000/cb"],"grant_types":["authorization_code"],"response_types":["code"],"token_endpoint_auth_method":"none"}`
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body))
	rec := httptest.NewRecorder()
	a.handleRegister(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp["client_id"])
	assert.Equal(t, "none", resp["token_endpoint_auth_method"])
	assert.Nil(t, resp["client_secret"], "public client must not get a secret")
}

func TestMCPAuth_RegisterRequiresRedirectURIs(t *testing.T) {
	a := newTestAuthorizer(t, true, nil)
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(`{"client_name":"x"}`))
	rec := httptest.NewRecorder()
	a.handleRegister(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestMCPAuth_MetadataAdvertisesRegistration(t *testing.T) {
	a := newTestAuthorizer(t, true, nil)
	rec := httptest.NewRecorder()
	a.authServerMetadata(rec, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil))
	var m map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &m))
	assert.Equal(t, testIssuer+"/register", m["registration_endpoint"])
}

func TestMCPAuth_AuthorizeRejectsUnregisteredRedirect(t *testing.T) {
	a := newTestAuthorizer(t, true, nil)
	// Register a client with one redirect_uri.
	reg := httptest.NewRecorder()
	a.handleRegister(reg, httptest.NewRequest(http.MethodPost, "/register",
		strings.NewReader(`{"client_name":"c","redirect_uris":["http://localhost:3000/cb"]}`)))
	var r map[string]any
	require.NoError(t, json.Unmarshal(reg.Body.Bytes(), &r))
	clientID, _ := r["client_id"].(string)

	_, challenge := pkcePair()
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", "http://evil.test/cb") // not registered
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	rec := httptest.NewRecorder()
	a.handleAuthorize(rec, httptest.NewRequest(http.MethodGet, "/authorize?"+q.Encode(), nil))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "unregistered redirect_uri must be rejected")
}

func TestMCPAuth_AuthorizeAcceptsRegisteredRedirect(t *testing.T) {
	a := newTestAuthorizer(t, true, nil)
	reg := httptest.NewRecorder()
	a.handleRegister(reg, httptest.NewRequest(http.MethodPost, "/register",
		strings.NewReader(`{"client_name":"c","redirect_uris":["http://localhost:3000/cb"]}`)))
	var r map[string]any
	require.NoError(t, json.Unmarshal(reg.Body.Bytes(), &r))
	clientID, _ := r["client_id"].(string)

	_, challenge := pkcePair()
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", "http://localhost:3000/cb") // registered
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	rec := httptest.NewRecorder()
	a.handleAuthorize(rec, httptest.NewRequest(http.MethodGet, "/authorize?"+q.Encode(), nil))
	require.Equal(t, http.StatusFound, rec.Code)
	loc, _ := url.Parse(rec.Header().Get("Location"))
	assert.NotEmpty(t, loc.Query().Get("code"))
}

// ── DCR redirect_uri allowlist (open-redirect defense) ───────────────────────

func TestIsAllowedDCRRedirectURI(t *testing.T) {
	cases := []struct {
		name string
		uri  string
		want bool
	}{
		{"localhost http, any port", "http://localhost:9999/cb", true},
		{"127.0.0.1 http", "http://127.0.0.1:54321/cb", true},
		{"IPv6 loopback", "http://[::1]:9999/cb", true},
		{"custom scheme (native app deep link)", "com.anthropic.claude://callback", true},
		{"custom scheme, no host (opaque)", "cursor:callback", false},
		{"custom scheme with host", "cursor://callback", true},
		{"public https host", "https://evil.example.com/cb", false},
		{"public http host", "http://attacker.test/cb", false},
		{"localhost as suffix must not match", "https://localhost.evil.example.com/cb", false},
		{"https on loopback host is allowed too (host check doesn't gate on scheme)", "https://localhost:9999/cb", true},
		{"unparsable", "://not a url", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isAllowedDCRRedirectURI(tc.uri), "uri=%q", tc.uri)
		})
	}
}

func TestMCPAuth_RegisterRejectsPublicHostRedirectURI(t *testing.T) {
	a := newTestAuthorizer(t, true, nil)
	for _, uri := range []string{
		"https://evil.example.com/callback",
		"http://attacker.test/callback",
	} {
		body := `{"client_name":"evil-client","redirect_uris":["` + uri + `"]}`
		rec := httptest.NewRecorder()
		a.handleRegister(rec, httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body)))
		assert.Equal(t, http.StatusBadRequest, rec.Code, "redirect_uri %q must be rejected at registration", uri)
	}
}

func TestMCPAuth_RegisterAllowsLoopbackAndCustomScheme(t *testing.T) {
	a := newTestAuthorizer(t, true, nil)
	for _, uri := range []string{
		"http://localhost:9999/callback",
		"http://127.0.0.1:54321/callback",
		"com.anthropic.claude://callback",
	} {
		body := `{"client_name":"test-client","redirect_uris":["` + uri + `"]}`
		rec := httptest.NewRecorder()
		a.handleRegister(rec, httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body)))
		assert.Equal(t, http.StatusCreated, rec.Code, "redirect_uri %q must be accepted at registration: %s", uri, rec.Body.String())
	}
}

// ── /authorize self-heal for unknown client_id ───────────────────────────────
//
// Real MCP clients (Claude Code) cache their client_id after the first DCR and do
// NOT re-register when /authorize rejects it, so a lost registry (evicted entry,
// restart before ADR-049 persistence landed) bricked re-auth with "redirect_uri
// not registered for client" and no client-side recovery path (production
// incident, 2026-07-24 — ported from a sibling project's fix for the
// same class of bug). clientRedirectAllowed now self-heals an unknown client_id
// whose redirect_uri is one open DCR (handleRegister) would have accepted
// verbatim, instead of rejecting it outright.

func TestMCPAuth_AuthorizeSelfHealsUnknownClientWithLoopbackRedirect(t *testing.T) {
	a := newTestAuthorizer(t, true, nil)
	const clientID = "cached-after-registry-loss"
	_, known := oauthClients.get(clientID)
	require.False(t, known, "test setup: client_id must start out unknown")

	_, challenge := pkcePair()
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", "http://localhost:64664/cb")
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	rec := httptest.NewRecorder()
	a.handleAuthorize(rec, httptest.NewRequest(http.MethodGet, "/authorize?"+q.Encode(), nil))
	require.Equal(t, http.StatusFound, rec.Code, "self-heal must let authorize proceed instead of rejecting")
	loc, _ := url.Parse(rec.Header().Get("Location"))
	assert.NotEmpty(t, loc.Query().Get("code"), "a code must be issued after self-heal")

	// The client is now registered in the persistent (ADR-049) registry, so a
	// later restart/replica restore keeps it working.
	c, ok := oauthClients.get(clientID)
	require.True(t, ok, "self-heal must register the previously-unknown client_id")
	assert.Equal(t, []string{"http://localhost:64664/cb"}, c.redirectURIs)
	var inSnapshot bool
	for _, row := range oauthClients.snapshot() {
		if row.ClientID == clientID {
			inSnapshot = true
		}
	}
	assert.True(t, inSnapshot, "self-healed registration must appear in the persistable snapshot")
}

func TestMCPAuth_AuthorizeSelfHealsUnknownClientWithCustomScheme(t *testing.T) {
	a := newTestAuthorizer(t, true, nil)
	const clientID = "cached-custom-scheme-client"
	_, known := oauthClients.get(clientID)
	require.False(t, known, "test setup: client_id must start out unknown")

	_, challenge := pkcePair()
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", "cursor://callback")
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	rec := httptest.NewRecorder()
	a.handleAuthorize(rec, httptest.NewRequest(http.MethodGet, "/authorize?"+q.Encode(), nil))
	require.Equal(t, http.StatusFound, rec.Code, "self-heal must let authorize proceed for a custom-scheme redirect")
	loc, _ := url.Parse(rec.Header().Get("Location"))
	assert.NotEmpty(t, loc.Query().Get("code"))

	c, ok := oauthClients.get(clientID)
	require.True(t, ok, "self-heal must register the previously-unknown client_id")
	assert.Equal(t, []string{"cursor://callback"}, c.redirectURIs)
}

func TestMCPAuth_AuthorizeSelfHealDeniesPublicHostRedirect(t *testing.T) {
	// An unknown client_id presenting a public-host redirect_uri must NOT be
	// self-healed — this is the actual open-redirect regression case: open DCR
	// would never have accepted this URI, so /authorize must not either.
	a := newTestAuthorizer(t, true, nil)
	const clientID = "attacker-client"
	_, known := oauthClients.get(clientID)
	require.False(t, known, "test setup: client_id must start out unknown")

	_, challenge := pkcePair()
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", "https://evil.example/cb")
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	rec := httptest.NewRecorder()
	a.handleAuthorize(rec, httptest.NewRequest(http.MethodGet, "/authorize?"+q.Encode(), nil))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "unknown client_id + public-host redirect_uri must be rejected, exactly like today")

	_, ok := oauthClients.get(clientID)
	assert.False(t, ok, "a rejected self-heal attempt must not register the client")
}

func TestMCPAuth_AuthorizeKnownClientWrongRedirectStillRejected(t *testing.T) {
	// Self-heal applies ONLY to unknown client_ids. A KNOWN client presenting a
	// redirect_uri outside its own registered set must still be rejected — the
	// self-heal must never widen an existing registration.
	a := newTestAuthorizer(t, true, nil)
	clientID := registerClient(t, a, "http://localhost:3000/cb")

	_, challenge := pkcePair()
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", "http://localhost:4000/other") // not in this client's registered set
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	rec := httptest.NewRecorder()
	a.handleAuthorize(rec, httptest.NewRequest(http.MethodGet, "/authorize?"+q.Encode(), nil))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "a known client presenting an unregistered redirect_uri must still be rejected")

	c, ok := oauthClients.get(clientID)
	require.True(t, ok)
	assert.Equal(t, []string{"http://localhost:3000/cb"}, c.redirectURIs, "the existing registration must not be widened")
}

func TestMCPAuth_RegisterIsRateLimited(t *testing.T) {
	// The public OAuth endpoints must be throttled per IP (#1): burst then 429.
	a := newTestAuthorizer(t, true, nil)
	body := `{"client_name":"x","redirect_uris":["http://localhost:3000/cb"]}`
	var got429 bool
	for range 60 {
		req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body))
		req.Header.Set("X-Real-IP", "203.0.113.7")
		rec := httptest.NewRecorder()
		a.handleRegister(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			got429 = true
			break
		}
	}
	assert.True(t, got429, "a flood from one IP must eventually be rate limited")
}

func TestMCPAuth_ClientMapIsBounded(t *testing.T) {
	// Registrations beyond the cap must not grow the map unboundedly (#1).
	// The registry is package-level (ADR-049); reset it so this test is isolated.
	oauthClients.mu.Lock()
	oauthClients.clients = make(map[string]oauthClient)
	oauthClients.mu.Unlock()
	a := newTestAuthorizer(t, true, nil)
	// Seed past the cap directly (bypassing the rate limiter) to assert the
	// eviction logic in handleRegister keeps the map at the cap.
	for i := range maxOAuthClients + 50 {
		body := `{"client_name":"x","redirect_uris":["http://localhost:3000/cb"]}`
		req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body))
		// Vary the peer address so the limiter never trips during seeding. With no
		// trusted proxy, the limiter keys on RemoteAddr (X-Forwarded-For/X-Real-IP
		// are spoofable and ignored), so we must vary RemoteAddr, not the header.
		req.RemoteAddr = "10.0." + itoa(i/250) + "." + itoa(i%250) + ":1234"
		rec := httptest.NewRecorder()
		a.handleRegister(rec, req)
		require.Equal(t, http.StatusCreated, rec.Code)
	}
	oauthClients.mu.Lock()
	n := len(oauthClients.clients)
	oauthClients.mu.Unlock()
	assert.LessOrEqual(t, n, maxOAuthClients, "client map must stay bounded at the cap")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// ── scope gating ───────────────────────────────────────────────────────────────

func TestMCPAuth_ScopesForUser(t *testing.T) {
	// MCP is admin-only: the allowlist (userHashes) gates access entirely. dev=false
	// so the gate is live (dev would bypass it).

	// Empty allowlist → deny everyone (fail-closed).
	closed := newTestAuthorizer(t, false, nil)
	assert.Nil(t, closed.scopesForUser("anyhash"))
	assert.False(t, closed.isMCPAllowed("anyhash"))

	// Non-empty allowlist → listed users get full read+write; others get nothing.
	gated := newTestAuthorizer(t, false, []string{"hash-alice"})
	assert.ElementsMatch(t, allMCPScopes, gated.scopesForUser("hash-alice"))
	assert.True(t, gated.isMCPAllowed("hash-alice"))
	assert.Nil(t, gated.scopesForUser("hash-bob"))
	assert.False(t, gated.isMCPAllowed("hash-bob"))

	// dev bypasses the gate entirely (testable flow without provisioning).
	dev := newTestAuthorizer(t, true, nil)
	assert.True(t, dev.isMCPAllowed("anyone"))
	assert.ElementsMatch(t, allMCPScopes, dev.scopesForUser("anyone"))

	// SetAllowFn overrides the static list with a live getter (CR-driven, no restart).
	live := newTestAuthorizer(t, false, nil)
	allowed := []string{"hash-live"}
	live.SetAllowFn(func() []string { return allowed })
	assert.True(t, live.isMCPAllowed("hash-live"))
	allowed = nil // simulate a CR edit removing the admin
	assert.False(t, live.isMCPAllowed("hash-live"))
}

// ── MCP handler: credential acceptance + scope enforcement ──────────────────────

func mcpInitWith(t *testing.T, srv *httptest.Server, bearer string) (status int, sessionID string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 0, "method": "initialize",
		"params": map[string]any{"protocolVersion": "2024-11-05", "clientInfo": map[string]any{"name": "t", "version": "1"}},
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Mcp-Session-Id")
}

func TestMCPServer_JWTAccepted(t *testing.T) {
	a := newTestAuthorizer(t, true, nil)
	h := NewMCPServer(newTestStore(), zerolog.Nop(), func() string { return "" }, a, nil, "", nil, 0, 0, 0)
	srv := httptest.NewServer(h)
	defer srv.Close()

	tok, err := a.signToken("user:1", []string{scopeRead, scopeWrite})
	require.NoError(t, err)
	status, sid := mcpInitWith(t, srv, tok)
	assert.Equal(t, http.StatusOK, status)
	assert.NotEmpty(t, sid)
}

func TestMCPServer_ChallengeOnNoToken(t *testing.T) {
	a := newTestAuthorizer(t, true, nil)
	h := NewMCPServer(newTestStore(), zerolog.Nop(), func() string { return "" }, a, nil, "", nil, 0, 0, 0)
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	wa := resp.Header.Get("WWW-Authenticate")
	assert.Contains(t, wa, `realm="mcp"`)
	assert.Contains(t, wa, "/.well-known/oauth-protected-resource")
}

func TestMCPServer_StaticSecretFallback(t *testing.T) {
	// With a static secret AND an authorizer, the static secret is still accepted
	// (full scope), and a wrong secret is challenged.
	a := newTestAuthorizer(t, true, nil)
	h := NewMCPServer(newTestStore(), zerolog.Nop(), func() string { return "static-secret" }, a, nil, "", nil, 0, 0, 0)
	srv := httptest.NewServer(h)
	defer srv.Close()

	status, sid := mcpInitWith(t, srv, "static-secret")
	assert.Equal(t, http.StatusOK, status, "correct static secret accepted")
	assert.NotEmpty(t, sid)

	status, _ = mcpInitWith(t, srv, "wrong-secret")
	assert.Equal(t, http.StatusUnauthorized, status, "wrong static secret challenged")
}

func TestMCPServer_NoCredentialFailsClosed(t *testing.T) {
	// Neither authorizer nor static secret → 503, never open.
	h := NewMCPServer(newTestStore(), zerolog.Nop(), func() string { return "" }, nil, nil, "", nil, 0, 0, 0)
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

func TestMCPServer_ReadOnlyTokenRejectedOnWriteTool(t *testing.T) {
	a := newTestAuthorizer(t, true, nil)
	h := NewMCPServer(newTestStore(), zerolog.Nop(), func() string { return "" }, a, nil, "", nil, 0, 0, 0)
	srv := httptest.NewServer(h)
	defer srv.Close()

	readTok, err := a.signToken("user:1", []string{scopeRead})
	require.NoError(t, err)

	// initialize ok (read scope).
	status, sid := mcpInitWith(t, srv, readTok)
	require.Equal(t, http.StatusOK, status)

	// tools/call delete_memory → needs write scope → 403.
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "delete_memory", "arguments": map[string]any{"id": "x"}},
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+readTok)
	req.Header.Set("Mcp-Session-Id", sid)
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "read-only token must be forbidden on delete_memory")
}

func TestMCPServer_WriteTokenAllowedOnWriteTool(t *testing.T) {
	a := newTestAuthorizer(t, true, nil)
	h := NewMCPServer(newTestStore(), zerolog.Nop(), func() string { return "" }, a, nil, "", nil, 0, 0, 0)
	srv := httptest.NewServer(h)
	defer srv.Close()

	tok, err := a.signToken("user:1", []string{scopeRead, scopeWrite})
	require.NoError(t, err)
	status, sid := mcpInitWith(t, srv, tok)
	require.Equal(t, http.StatusOK, status)

	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "delete_memory", "arguments": map[string]any{"id": "x"}},
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Mcp-Session-Id", sid)
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode, "write token allowed on delete_memory")
}

// ── DCR client persistence (ADR-049) ─────────────────────────────────────────

// TestOAuthClientStore_SurvivesRestart is the fix for "Claude Code re-auths every
// restart": a registered client snapshots to CR status and restores into a fresh
// store, so its client_id is still known and /authorize accepts its redirect_uri.
func TestOAuthClientStore_SurvivesRestart(t *testing.T) {
	before := &oauthClientStore{clients: make(map[string]oauthClient)}
	before.add("cc-client-1", oauthClient{redirectURIs: []string{"http://localhost:7777/cb"}, issuedAt: time.Now()})

	rows := before.snapshot()
	require.Len(t, rows, 1)
	assert.Equal(t, "cc-client-1", rows[0].ClientID)
	assert.Equal(t, []string{"http://localhost:7777/cb"}, rows[0].RedirectURIs)

	// Fresh process: empty registry, restore from the persisted rows.
	after := &oauthClientStore{clients: make(map[string]oauthClient)}
	assert.Equal(t, 1, after.restore(rows), "one client restored")

	c, ok := after.get("cc-client-1")
	require.True(t, ok, "restored client_id must be known after restart")
	assert.Contains(t, c.redirectURIs, "http://localhost:7777/cb")
}

// TestOAuthClientStore_RestoreDoesNotClobberLive: self-echo from a flush must not
// overwrite a live registration.
func TestOAuthClientStore_RestoreDoesNotClobberLive(t *testing.T) {
	s := &oauthClientStore{clients: make(map[string]oauthClient)}
	s.add("c1", oauthClient{redirectURIs: []string{"http://a/cb"}, issuedAt: time.Now()})
	rows := s.snapshot()

	// A stale CR row for the same client with a DIFFERENT redirect must be ignored.
	rows[0].RedirectURIs = []string{"http://evil/cb"}
	assert.Equal(t, 0, s.restore(rows), "already-held client is not re-added")

	c, _ := s.get("c1")
	assert.Equal(t, []string{"http://a/cb"}, c.redirectURIs, "live registration preserved, not clobbered")
}

// TestOAuthClientStore_RestoreSkipsInvalid: rows with no client_id or no redirect
// URIs are skipped.
func TestOAuthClientStore_RestoreSkipsInvalid(t *testing.T) {
	s := &oauthClientStore{clients: make(map[string]oauthClient)}
	n := s.restore([]mcell.OAuthClientReg{
		{ClientID: "", RedirectURIs: []string{"http://a/cb"}},
		{ClientID: "c2", RedirectURIs: nil},
	})
	assert.Equal(t, 0, n, "invalid rows must not restore")
}
