package xray

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	mcell "github.com/triageagent-dev/x-ray/v2/api/v1"
)

// ── sessionStore ──────────────────────────────────────────────────────────────

func newTestSessionStore() *sessionStore {
	return &sessionStore{entries: make(map[string]sessionEntry), restored: make(map[string]sessionEntry)}
}

// newPersistedSessionStore returns a store with CR-status persistence enabled
// (ADR-047): a fixed HMAC key so token hashing is deterministic across the
// simulated restart. onChange is left nil — tests drive snapshot/restore directly.
func newPersistedSessionStore() *sessionStore {
	return &sessionStore{entries: make(map[string]sessionEntry), restored: make(map[string]sessionEntry), hmacKey: []byte("test-session-hmac-key")}
}

func TestSessionStore_CreateAndValidate(t *testing.T) {
	s := newTestSessionStore()
	token := s.create("alice", "github:1")
	require.NotEmpty(t, token)

	user, userID, ok := s.validate(token)
	assert.True(t, ok)
	assert.Equal(t, "alice", user)
	assert.Equal(t, "github:1", userID)
}

func TestSessionStore_ValidateUnknownToken(t *testing.T) {
	s := newTestSessionStore()
	_, _, ok := s.validate("no-such-token")
	assert.False(t, ok)
}

func TestSessionStore_TokensAreUnique(t *testing.T) {
	s := newTestSessionStore()
	t1 := s.create("alice", "github:1")
	t2 := s.create("alice", "github:1")
	assert.NotEqual(t, t1, t2, "two tokens for the same user must differ")
}

func TestSessionStore_ExpiredToken(t *testing.T) {
	s := newTestSessionStore()
	token := "expired-token"
	s.mu.Lock()
	s.entries[token] = sessionEntry{username: "bob", expiresAt: time.Now().Add(-time.Second)}
	s.mu.Unlock()

	_, _, ok := s.validate(token)
	assert.False(t, ok, "expired token must be rejected")

	// Entry should have been pruned
	s.mu.Lock()
	_, stillThere := s.entries[token]
	s.mu.Unlock()
	assert.False(t, stillThere, "expired entry should be deleted on validation")
}

// ── session persistence (ADR-047) ────────────────────────────────────────────

// TestSessionStore_SurvivesRestart is the load-bearing claim: a session created
// on one process snapshots to CR status, and after a "restart" (fresh empty store
// with the same HMAC key) the raw token still validates.
func TestSessionStore_SurvivesRestart(t *testing.T) {
	before := newPersistedSessionStore()
	token := before.create("alice", "github:1")

	rows := before.snapshotSessions()
	require.Len(t, rows, 1)
	assert.Equal(t, stableUserID("github:1"), rows[0].UserHash)

	// Fresh process: empty entries, same key. Rehydrate from the persisted rows.
	after := newPersistedSessionStore()
	after.restoreSessions(rows)

	// validate() must return the real identity — decrypted from the sealed blob —
	// because write-gating (username) and metering (userID) depend on it.
	user, userID, ok := after.validate(token)
	assert.True(t, ok, "raw token must validate against a session restored from status")
	assert.Equal(t, "alice", user, "username recovered from encrypted identity blob")
	assert.Equal(t, "github:1", userID, "userID recovered from encrypted identity blob")
}

// TestSessionStore_RestoreReturnsCount guards the startup-ordering bug: restore
// reports its breakdown: added on a fresh restore, alreadyHeld on a self-echo
// (must NOT warn), undecryptable on a key mismatch (the only warn case), and
// disabled when the key is not set (the signature of SetSessionPersistence
// running after the CR was already replayed).
func TestSessionStore_RestoreReturnsBreakdown(t *testing.T) {
	before := newPersistedSessionStore()
	before.create("alice", "github:1")
	rows := before.snapshotSessions()

	// Key set (correct order): the row restores once.
	withKey := newPersistedSessionStore()
	r1 := withKey.restoreSessions(rows)
	assert.Equal(t, 1, r1.added, "valid row restores → added 1")
	assert.Zero(t, r1.undecryptable)

	// Self-echo: same rows again → alreadyHeld, NOT added, NOT undecryptable (silent).
	r2 := withKey.restoreSessions(rows)
	assert.Equal(t, 0, r2.added, "self-echo adds nothing")
	assert.Equal(t, 1, r2.alreadyHeld, "self-echo counts as alreadyHeld, not a warning")
	assert.Zero(t, r2.undecryptable)

	// Key NOT set yet (the ordering bug): restore is a no-op flagged as disabled.
	noKey := newTestSessionStore() // hmacKey == nil
	assert.True(t, noKey.restoreSessions(rows).disabled, "no key → restore disabled")

	// Foreign key: rows sealed under a different key → undecryptable (the warn case).
	foreign := &sessionStore{entries: make(map[string]sessionEntry), hmacKey: []byte("different-key")}
	rf := foreign.restoreSessions(rows)
	assert.Equal(t, 0, rf.added)
	assert.Equal(t, 1, rf.undecryptable, "foreign-key rows are undecryptable")
}

// TestSessionStore_SnapshotStoresNoCleartextIdentity guarantees the persisted row
// carries no login name or SSO ID in clear — only an opaque encrypted blob. A
// reader with `get mcell` RBAC must not learn who is logged in.
func TestSessionStore_SnapshotStoresNoCleartextIdentity(t *testing.T) {
	s := newPersistedSessionStore()
	s.create("Alice.Example", "azure:00000000-0000-0000-0000-000000000001")

	rows := s.snapshotSessions()
	require.Len(t, rows, 1)
	assert.NotEmpty(t, rows[0].IdentityEnc, "identity must be persisted (encrypted)")
	assert.NotContains(t, rows[0].IdentityEnc, "Alice", "no cleartext username in status")
	assert.NotContains(t, rows[0].IdentityEnc, "azure:", "no cleartext SSO ID in status")
	assert.NotContains(t, rows[0].IdentityEnc, "00000000", "no cleartext SSO ID in status")
}

// TestSessionStore_RestoreRejectsForeignKeyIdentity: a row sealed under a
// different server key (or tampered) must not be restored — GCM auth fails.
func TestSessionStore_RestoreRejectsForeignKeyIdentity(t *testing.T) {
	foreign := &sessionStore{entries: make(map[string]sessionEntry), hmacKey: []byte("a-totally-different-key")}
	token := foreign.create("mallory", "github:9")
	rows := foreign.snapshotSessions() // IdentityEnc sealed under the foreign key

	s := newPersistedSessionStore() // our key — cannot open the foreign blob
	s.restoreSessions(rows)

	_, _, ok := s.validate(token)
	assert.False(t, ok, "session whose identity was sealed under a foreign key must be rejected")
}

// TestSessionStore_SnapshotStoresHashNotRawToken guarantees the raw bearer token
// never reaches CR status — only its HMAC. A reader with `get mcell` RBAC must not
// be able to lift a usable credential.
func TestSessionStore_SnapshotStoresHashNotRawToken(t *testing.T) {
	s := newPersistedSessionStore()
	token := s.create("alice", "github:1")

	rows := s.snapshotSessions()
	require.Len(t, rows, 1)
	assert.NotEmpty(t, rows[0].TokenHMAC)
	assert.NotEqual(t, token, rows[0].TokenHMAC, "persisted row must not contain the raw token")
	assert.Equal(t, s.tokenHMAC(token), rows[0].TokenHMAC, "persisted hash must be HMAC(token)")
}

// TestSessionStore_PersistenceDisabledWithoutKey: with no HMAC key, nothing is
// persisted (in-memory-only mode for local dev without k8s).
func TestSessionStore_PersistenceDisabledWithoutKey(t *testing.T) {
	s := newTestSessionStore() // no hmacKey
	s.create("alice", "github:1")
	assert.Nil(t, s.snapshotSessions(), "no key means no persistence")
}

// TestSessionStore_SnapshotPrunesExpired: expired sessions are dropped from both
// the snapshot and the in-memory map so status stays bounded by live sessions.
func TestSessionStore_SnapshotPrunesExpired(t *testing.T) {
	s := newPersistedSessionStore()
	live := s.create("alice", "github:1")
	s.mu.Lock()
	s.entries["dead"] = sessionEntry{username: "bob", userID: "github:2", expiresAt: time.Now().Add(-time.Hour)}
	s.mu.Unlock()

	rows := s.snapshotSessions()
	require.Len(t, rows, 1, "only the live session is persisted")
	assert.Equal(t, stableUserID("github:1"), rows[0].UserHash, "the live session is alice's")

	s.mu.Lock()
	_, deadStillThere := s.entries["dead"]
	_, liveStillThere := s.entries[live]
	s.mu.Unlock()
	assert.False(t, deadStillThere, "expired entry pruned from the map")
	assert.True(t, liveStillThere, "live entry retained")
}

// TestSessionStore_RestoreSkipsExpiredRows: a status row past its expiry must not
// be rehydrated (it would be a zombie session).
func TestSessionStore_RestoreSkipsExpiredRows(t *testing.T) {
	s := newPersistedSessionStore()
	s.restoreSessions([]mcell.UserSession{
		{UserHash: "x", TokenHMAC: s.tokenHMAC("whatever"),
			IdentityEnc: s.encryptIdentity("bob", "github:2"),
			ExpiresAt:   time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)},
	})
	s.mu.Lock()
	n := len(s.entries) + len(s.restored)
	s.mu.Unlock()
	assert.Zero(t, n, "expired status rows must not be restored")
}

// TestSessionStore_RestoreDoesNotClobberLiveSession: restore runs on every watch
// event and flushStats writes the same CR, so a snapshot echoes back. A live raw
// token must not be duplicated or replaced by its own HMAC-keyed echo.
func TestSessionStore_RestoreDoesNotClobberLiveSession(t *testing.T) {
	s := newPersistedSessionStore()
	token := s.create("alice", "github:1")
	rows := s.snapshotSessions() // contains the HMAC of `token`

	s.restoreSessions(rows) // self-echo

	s.mu.Lock()
	n := len(s.entries) + len(s.restored)
	_, rawStillThere := s.entries[token]
	s.mu.Unlock()
	assert.Equal(t, 1, n, "self-echo must not create a duplicate HMAC-keyed entry")
	assert.True(t, rawStillThere, "live raw-token entry must be preserved")

	// And the raw token still validates (fast path, no re-key needed).
	_, _, ok := s.validate(token)
	assert.True(t, ok)
}

// TestSessionStore_ValidateRekeysRestoredSession: after a restart a session is
// keyed by its HMAC; the first validate() re-keys it under the raw token so
// subsequent lookups take the fast path.
func TestSessionStore_ValidateRekeysRestoredSession(t *testing.T) {
	before := newPersistedSessionStore()
	token := before.create("alice", "github:1")
	rows := before.snapshotSessions()

	after := newPersistedSessionStore()
	after.restoreSessions(rows)

	// Before first validate: keyed by HMAC, not raw token.
	after.mu.Lock()
	_, rawBefore := after.entries[token]
	_, hashBefore := after.restored[after.tokenHMAC(token)]
	after.mu.Unlock()
	assert.False(t, rawBefore, "restored entry is keyed by HMAC, not raw token")
	assert.True(t, hashBefore, "restored entry present under HMAC key")

	_, _, ok := after.validate(token)
	require.True(t, ok)

	// After validate: re-keyed under the raw token.
	after.mu.Lock()
	_, rawAfter := after.entries[token]
	_, hashAfter := after.restored[after.tokenHMAC(token)]
	after.mu.Unlock()
	assert.True(t, rawAfter, "session re-keyed under raw token after validate")
	assert.False(t, hashAfter, "stale HMAC key removed after re-key")
}

// TestSessionStore_PersistedHashIsNotACredential: the HMAC written to CR status
// must never validate as a session token, before or after the genuine holder's
// first request, or anyone able to read the status could take over the session.
func TestSessionStore_PersistedHashIsNotACredential(t *testing.T) {
	before := newPersistedSessionStore()
	token := before.create("alice", "github:1")
	rows := before.snapshotSessions()
	require.Len(t, rows, 1)
	hash := rows[0].TokenHMAC

	after := newPersistedSessionStore()
	after.restoreSessions(rows)

	_, _, ok := after.validate(hash)
	assert.False(t, ok, "the persisted hash must not validate as a bearer or cookie")

	// The genuine token still works, and the hash is still no credential after it.
	_, _, ok = after.validate(token)
	require.True(t, ok)
	_, _, ok = after.validate(hash)
	assert.False(t, ok, "the persisted hash must not validate after the session is re-keyed")
}

// TestSessionStore_RestoredSessionsStaySnapshotted: a restored session nobody has
// used yet must be written back unchanged, not hashed a second time, or the next
// replica could not match it.
func TestSessionStore_RestoredSessionsStaySnapshotted(t *testing.T) {
	before := newPersistedSessionStore()
	token := before.create("alice", "github:1")
	rows := before.snapshotSessions()

	after := newPersistedSessionStore()
	after.restoreSessions(rows)
	again := after.snapshotSessions()
	require.Len(t, again, 1)
	assert.Equal(t, rows[0].TokenHMAC, again[0].TokenHMAC)

	third := newPersistedSessionStore()
	third.restoreSessions(again)
	_, _, ok := third.validate(token)
	assert.True(t, ok, "a session passed through an unused replica still validates")
}

// ── OAuth sign-in flow cookie (loginflow.go) ────────────────────────────────────

// callbackWith builds a callback request carrying the cookies a login just set.
func callbackWith(rec *httptest.ResponseRecorder) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/auth/github/callback", nil)
	for _, c := range rec.Result().Cookies() {
		r.AddCookie(c)
	}
	return r
}

// withSessionKey runs f with the shared session key set as main.go sets it.
func withSessionKey(t *testing.T, key string, f func()) {
	t.Helper()
	sessions.mu.Lock()
	old := sessions.hmacKey
	sessions.hmacKey = []byte(key)
	sessions.mu.Unlock()
	defer func() {
		sessions.mu.Lock()
		sessions.hmacKey = old
		sessions.mu.Unlock()
	}()
	f()
}

func TestLoginFlow_RoundTrip(t *testing.T) {
	rec := httptest.NewRecorder()
	f := startLoginFlow(rec, "", "github", "https://ui.example")
	got, ok := finishLoginFlow(httptest.NewRecorder(), callbackWith(rec), "", "github", f.State)
	require.True(t, ok)
	assert.Equal(t, "https://ui.example", got.UIOrigin)
}

// The point of the cookie: a sign-in started on one replica finishes on another,
// because every replica derives the key from the same OAuth client secret.
func TestLoginFlow_FinishesOnAnotherReplicaWithTheSameSecret(t *testing.T) {
	var rec *httptest.ResponseRecorder
	var f loginFlow
	withSessionKey(t, "shared-secret-derived", func() {
		rec = httptest.NewRecorder()
		f = startLoginFlow(rec, "/isms", "google", "")
	})
	withSessionKey(t, "shared-secret-derived", func() {
		_, ok := finishLoginFlow(httptest.NewRecorder(), callbackWith(rec), "/isms", "google", f.State)
		assert.True(t, ok, "a replica with the same secret opens the flow")
	})
	withSessionKey(t, "another-deployment", func() {
		_, ok := finishLoginFlow(httptest.NewRecorder(), callbackWith(rec), "/isms", "google", f.State)
		assert.False(t, ok, "a deployment with another secret cannot open it")
	})
}

func TestLoginFlow_Refuses(t *testing.T) {
	rec := httptest.NewRecorder()
	f := startLoginFlow(rec, "", "github", "")
	cases := map[string]func() (*http.Request, string, string){
		"mismatched query state (login CSRF)": func() (*http.Request, string, string) { return callbackWith(rec), "github", "different" },
		"empty query state":                   func() (*http.Request, string, string) { return callbackWith(rec), "github", "" },
		"another provider's callback":         func() (*http.Request, string, string) { return callbackWith(rec), "google", f.State },
		"no cookie": func() (*http.Request, string, string) {
			return httptest.NewRequest(http.MethodGet, "/v1/auth/github/callback", nil), "github", f.State
		},
		"tampered cookie": func() (*http.Request, string, string) {
			r := httptest.NewRequest(http.MethodGet, "/v1/auth/github/callback", nil)
			raw, err := base64.RawURLEncoding.DecodeString(rec.Result().Cookies()[0].Value)
			require.NoError(t, err)
			raw[len(raw)/2] ^= 0x01
			r.AddCookie(&http.Cookie{Name: oauthStateCookie, Value: base64.RawURLEncoding.EncodeToString(raw)})
			return r, "github", f.State
		},
		"not a sealed value": func() (*http.Request, string, string) {
			r := httptest.NewRequest(http.MethodGet, "/v1/auth/github/callback", nil)
			r.AddCookie(&http.Cookie{Name: oauthStateCookie, Value: f.State})
			return r, "github", f.State
		},
		// A sign-in started on a replica of the previous release, during a roll.
		"previous release's signed state": func() (*http.Request, string, string) {
			r := httptest.NewRequest(http.MethodGet, "/v1/auth/github/callback", nil)
			r.AddCookie(&http.Cookie{Name: oauthStateCookie, Value: f.State + ".0123456789abcdef"})
			return r, "github", f.State
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r, provider, state := c()
			_, ok := finishLoginFlow(httptest.NewRecorder(), r, "", provider, state)
			assert.False(t, ok)
		})
	}
}

func TestLoginFlow_RefusesAnExpiredFlow(t *testing.T) {
	// Seal a flow that expired a second ago with the real key.
	f := loginFlow{Provider: "github", State: "abc123", Expires: time.Now().Add(-time.Second).Unix()}
	plain, err := json.Marshal(f)
	require.NoError(t, err)
	gcm := loginAEAD()
	nonce := make([]byte, gcm.NonceSize())
	r := httptest.NewRequest(http.MethodGet, "/v1/auth/github/callback", nil)
	r.AddCookie(&http.Cookie{Name: oauthStateCookie, Value: base64.RawURLEncoding.EncodeToString(gcm.Seal(nonce, nonce, plain, loginFlowAAD("github")))})
	_, ok := finishLoginFlow(httptest.NewRecorder(), r, "", "github", "abc123")
	assert.False(t, ok)
}

func TestLoginFlow_CallbackClearsTheCookie(t *testing.T) {
	rec := httptest.NewRecorder()
	f := startLoginFlow(rec, "/hr", "github", "")
	out := httptest.NewRecorder()
	_, ok := finishLoginFlow(out, callbackWith(rec), "/hr", "github", f.State)
	require.True(t, ok)
	cookies := out.Result().Cookies()
	require.Len(t, cookies, 1)
	assert.Equal(t, oauthStateCookie, cookies[0].Name)
	assert.Less(t, cookies[0].MaxAge, 0, "the browser drops the flow, so it cannot be sent again")
}

func TestLoginFlow_SetCookieAttributes(t *testing.T) {
	rec := httptest.NewRecorder()
	startLoginFlow(rec, "", "github", "")
	cookies := rec.Result().Cookies()
	require.Len(t, cookies, 1)
	c := cookies[0]
	assert.Equal(t, oauthStateCookie, c.Name)
	assert.Equal(t, "/v1/auth/", c.Path)
	assert.True(t, c.HttpOnly, "state cookie must be HttpOnly")
	assert.True(t, c.Secure, "state cookie must be Secure")
	assert.Equal(t, http.SameSiteLaxMode, c.SameSite, "Lax is required to survive the OAuth redirect")
	assert.Equal(t, int(loginFlowTTL.Seconds()), c.MaxAge)
}

func TestSessionStore_ConcurrentCreate(t *testing.T) {
	s := newTestSessionStore()
	var wg sync.WaitGroup
	tokens := make([]string, 100)
	var mu sync.Mutex

	for i := range 100 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tok := s.create("user", "github:999")
			mu.Lock()
			tokens[i] = tok
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	seen := make(map[string]struct{})
	for _, tok := range tokens {
		require.NotEmpty(t, tok)
		seen[tok] = struct{}{}
	}
	assert.Len(t, seen, 100, "all tokens must be unique")
}

// Without an OAuth client secret (local dev) the flow is sealed under a per-process
// key: one process, so it still completes.
func TestLoginFlow_WorksWithoutASecret(t *testing.T) {
	withSessionKey(t, "", func() {
		rec := httptest.NewRecorder()
		f := startLoginFlow(rec, "", "github", "")
		_, ok := finishLoginFlow(httptest.NewRecorder(), callbackWith(rec), "", "github", f.State)
		assert.True(t, ok)
	})
}
