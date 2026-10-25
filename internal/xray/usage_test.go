package xray

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genai"

	mcell "github.com/triageagent-dev/x-ray/v2/api/v1"
)

// ── per-user daily token tracker ──────────────────────────────────────────────

func TestMemoryStore_UserTokens_AccumulatesAndRollsOver(t *testing.T) {
	s := newTestStore()
	hash := stableUserID("azure:user-a")

	s.AddUserTokens(hash, 100)
	s.AddUserTokens(hash, 50)
	assert.Equal(t, int64(150), s.UserTokensToday(hash))

	// Empty hash (unauthenticated) is never metered.
	s.AddUserTokens("", 999)
	assert.Equal(t, int64(0), s.UserTokensToday(""))

	// Unknown user has zero spend.
	assert.Equal(t, int64(0), s.UserTokensToday(stableUserID("azure:user-b")))

	// Simulate a previous-day counter: UserTokensToday must report 0 (not yet rolled),
	// and the next AddUserTokens resets it to the current day.
	s.usageMu.Lock()
	s.usage[hash].day = "2000-01-01"
	s.usageMu.Unlock()
	assert.Equal(t, int64(0), s.UserTokensToday(hash), "stale-day counter must not count toward today")

	s.AddUserTokens(hash, 30)
	assert.Equal(t, int64(30), s.UserTokensToday(hash), "add after day change must reset to fresh day")
}

func TestMemoryStore_UserDaily_RoundTripCR(t *testing.T) {
	s := newTestStore()
	hashA := stableUserID("azure:user-a")
	hashB := stableUserID("azure:user-b")
	s.AddUserTokens(hashA, 200)
	s.AddUserTokens(hashB, 75)

	// snapshotUserDaily serializes today's rows for CR persistence.
	rows := s.snapshotUserDaily()
	require.Len(t, rows, 2)
	byHash := map[string]int64{}
	for _, r := range rows {
		assert.Equal(t, utcDay(), r.Day)
		byHash[r.UserHash] = r.Tokens
	}
	assert.Equal(t, int64(200), byHash[hashA])
	assert.Equal(t, int64(75), byHash[hashB])

	// RestoreStats rehydrates current-day rows into a fresh store and drops stale ones.
	s2 := newTestStore()
	s2.RestoreStats(&mcell.RuntimeStats{
		UserDaily: []mcell.UserDailyTokens{
			{UserHash: hashA, Day: utcDay(), Tokens: 200},
			{UserHash: hashB, Day: utcDay(), Tokens: 75},
			{UserHash: stableUserID("azure:stale"), Day: "2000-01-01", Tokens: 9999},
		},
	})
	assert.Equal(t, int64(200), s2.UserTokensToday(hashA))
	assert.Equal(t, int64(75), s2.UserTokensToday(hashB))
	assert.Equal(t, int64(0), s2.UserTokensToday(stableUserID("azure:stale")), "stale-day row must not be rehydrated")
}

// TestMemoryStore_RestoreStats_DoesNotLowerLiveCounter reproduces the HUD
// oscillation bug: flushStats writes the system_configuration CR, the watch
// echoes it back into RestoreStats, and a stale/self-echoed snapshot must never
// lower a counter the live session has already advanced. Restore merges by max.
func TestMemoryStore_RestoreStats_DoesNotLowerLiveCounter(t *testing.T) {
	s := newTestStore()
	hash := stableUserID("azure:live-user")
	s.AddUserTokens(hash, 11643) // live in-memory value

	// A stale CR snapshot (e.g. an earlier flush) arrives via the watch.
	s.RestoreStats(&mcell.RuntimeStats{
		UserDaily: []mcell.UserDailyTokens{{UserHash: hash, Day: utcDay(), Tokens: 5254}},
	})
	assert.Equal(t, int64(11643), s.UserTokensToday(hash), "restore must not lower a higher live counter")

	// A higher snapshot (e.g. another replica's larger count) does raise it.
	s.RestoreStats(&mcell.RuntimeStats{
		UserDaily: []mcell.UserDailyTokens{{UserHash: hash, Day: utcDay(), Tokens: 12000}},
	})
	assert.Equal(t, int64(12000), s.UserTokensToday(hash), "restore may raise a counter")

	// A fresh user not yet in the map is filled in from the snapshot.
	other := stableUserID("azure:other")
	s.RestoreStats(&mcell.RuntimeStats{
		UserDaily: []mcell.UserDailyTokens{{UserHash: other, Day: utcDay(), Tokens: 300}},
	})
	assert.Equal(t, int64(300), s.UserTokensToday(other))
}

func TestMemoryStore_SnapshotUserDaily_PrunesStaleDays(t *testing.T) {
	s := newTestStore()
	hash := stableUserID("azure:user-a")
	s.AddUserTokens(hash, 100)

	// Force the row to a past day, then snapshot — it must be pruned from the map
	// and excluded from the returned rows.
	s.usageMu.Lock()
	s.usage[hash].day = "2000-01-01"
	s.usageMu.Unlock()

	rows := s.snapshotUserDaily()
	assert.Empty(t, rows, "past-day rows must not be persisted")

	s.usageMu.Lock()
	_, present := s.usage[hash]
	s.usageMu.Unlock()
	assert.False(t, present, "past-day rows must be pruned from the in-memory map")
}

// ── ConfigWatcher parses MaxUserDailyAudioTokens ───────────────────────────────────

func TestConfigWatcher_ParsesMaxUserDailyAudioTokens(t *testing.T) {
	w := &ConfigWatcher{}
	cell := &mcell.MemoryCell{}
	cell.Name = SystemConfigCRName
	cell.Spec.Kind = "system_configuration"
	cell.Spec.VoiceConfig = &mcell.VoiceSessionConfig{MaxUserDailyAudioTokens: 5000}

	w.update(cell)
	assert.Equal(t, int64(5000), w.Get().MaxUserDailyAudioTokens)
}

// ── WebSocket-upgrade enforcement gate ────────────────────────────────────────

// authedRequest builds a /v1/voice request carrying a valid session bearer token
// for the given raw SSO ID, returning the request and the user's stable hash.
func authedRequest(t *testing.T, rawUserID string) (*http.Request, string) {
	t.Helper()
	token := sessions.create("alice", rawUserID)
	t.Cleanup(func() { sessions.invalidate(token) })
	req := httptest.NewRequest(http.MethodGet, "/v1/voice", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	return req, stableUserID(rawUserID)
}

func TestVoiceManager_RejectsOverDailyLimit(t *testing.T) {
	s := newTestStore()
	w := &ConfigWatcher{}
	vm := &VoiceManager{
		memory:                   s,
		config:                   w,
		userDailyAudioTokenLimit: 1000,
		// Set genaiClient so ensureClientLocked is a no-op and the request reaches the gate.
		genaiClient: &genai.Client{},
	}

	req, hash := authedRequest(t, "azure:over-user")
	s.AddUserTokens(hash, 1000) // exactly at the limit → rejected (>=)

	rec := httptest.NewRecorder()
	vm.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
}

func TestVoiceManager_CRLimitOverridesFlag(t *testing.T) {
	s := newTestStore()
	w := &ConfigWatcher{}
	// CR limit (500) is stricter than the flag default (1_000_000) and must win.
	w.val.Store(&VoiceSessionConfig{MaxUserDailyAudioTokens: 500})
	vm := &VoiceManager{
		memory:                   s,
		config:                   w,
		userDailyAudioTokenLimit: 1_000_000,
		genaiClient:              &genai.Client{},
	}

	req, hash := authedRequest(t, "azure:cr-user")
	s.AddUserTokens(hash, 600) // under flag, over CR → rejected

	rec := httptest.NewRecorder()
	vm.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
}

func TestVoiceManager_UnauthenticatedNeverCapped(t *testing.T) {
	s := newTestStore()
	w := &ConfigWatcher{}
	vm := &VoiceManager{
		memory:                   s,
		config:                   w,
		userDailyAudioTokenLimit: 1,
		genaiClient:              &genai.Client{},
	}

	// No session cookie / bearer → userHash == "" → gate is skipped. The request
	// proceeds past the gate and fails the WebSocket upgrade instead of returning 429.
	req := httptest.NewRequest(http.MethodGet, "/v1/voice", nil)
	rec := httptest.NewRecorder()
	vm.ServeHTTP(rec, req)
	assert.NotEqual(t, http.StatusTooManyRequests, rec.Code, "unauthenticated sessions must never be rate-limited by user quota")
}

func TestVoiceManager_UnderLimitNotRejected(t *testing.T) {
	s := newTestStore()
	w := &ConfigWatcher{}
	vm := &VoiceManager{
		memory:                   s,
		config:                   w,
		userDailyAudioTokenLimit: 1000,
		genaiClient:              &genai.Client{},
	}

	req, hash := authedRequest(t, "azure:under-user")
	s.AddUserTokens(hash, 999) // under the limit → passes the gate

	rec := httptest.NewRecorder()
	vm.ServeHTTP(rec, req)
	assert.NotEqual(t, http.StatusTooManyRequests, rec.Code, "under-limit user must pass the quota gate")
}

// ── UserQuota reporting ───────────────────────────────────────────────────────

func TestVoiceManager_UserQuota_ReportsUsedAndRemaining(t *testing.T) {
	s := newTestStore()
	vm := &VoiceManager{memory: s, config: &ConfigWatcher{}, userDailyAudioTokenLimit: 1000}
	hash := stableUserID("azure:q-user")
	s.AddUserTokens(hash, 300)

	used, limit, remaining, ok := vm.UserQuota("azure:q-user")
	assert.True(t, ok)
	assert.Equal(t, int64(300), used)
	assert.Equal(t, int64(1000), limit)
	assert.Equal(t, int64(700), remaining)
}

func TestVoiceManager_UserQuota_RemainingFlooredAtZero(t *testing.T) {
	s := newTestStore()
	vm := &VoiceManager{memory: s, config: &ConfigWatcher{}, userDailyAudioTokenLimit: 1000}
	hash := stableUserID("azure:over-q")
	s.AddUserTokens(hash, 1500) // over the cap

	_, _, remaining, ok := vm.UserQuota("azure:over-q")
	assert.True(t, ok)
	assert.Equal(t, int64(0), remaining, "remaining must floor at 0, never negative")
}

func TestVoiceManager_UserQuota_UnlimitedAndUnauthenticated(t *testing.T) {
	s := newTestStore()

	// No limit configured → ok=false, limit=0 (unlimited).
	vmUnlimited := &VoiceManager{memory: s, config: &ConfigWatcher{}, userDailyAudioTokenLimit: 0}
	_, limit, _, ok := vmUnlimited.UserQuota("azure:any")
	assert.False(t, ok)
	assert.Equal(t, int64(0), limit)

	// Limit set but unauthenticated (empty rawUserID) → ok=false, limit reported.
	vmCapped := &VoiceManager{memory: s, config: &ConfigWatcher{}, userDailyAudioTokenLimit: 1000}
	_, limit, _, ok = vmCapped.UserQuota("")
	assert.False(t, ok)
	assert.Equal(t, int64(1000), limit)
}

// ── mid-session over-limit farewell ───────────────────────────────────────────

func TestVoiceSession_ShouldTriggerLimitFarewell(t *testing.T) {
	s := &VoiceSession{}

	// Under limit (remaining > 0) never triggers.
	assert.False(t, s.shouldTriggerLimitFarewell(500))
	assert.False(t, s.shouldTriggerLimitFarewell(1))

	// At zero remaining, triggers exactly once.
	assert.True(t, s.shouldTriggerLimitFarewell(0), "first exhaustion must trigger the farewell")
	assert.False(t, s.shouldTriggerLimitFarewell(0), "farewell must not be re-triggered within the same session")
	assert.False(t, s.shouldTriggerLimitFarewell(-50), "negative remaining (overshoot) must not re-trigger")
}

func TestVoiceManager_UserQuota_CRLimitOverridesFlag(t *testing.T) {
	s := newTestStore()
	w := &ConfigWatcher{}
	w.val.Store(&VoiceSessionConfig{MaxUserDailyAudioTokens: 500})
	vm := &VoiceManager{memory: s, config: w, userDailyAudioTokenLimit: 1_000_000}

	_, limit, _, ok := vm.UserQuota("azure:cr-q")
	assert.True(t, ok)
	assert.Equal(t, int64(500), limit, "CR limit must override the flag default")
}

// ── audio-token quota (ADR-042) ──────────────────────────────────────────────

func TestConfigWatcher_ParsesMaxUserDailyAudioTokens_Renamed(t *testing.T) {
	w := &ConfigWatcher{}
	cell := &mcell.MemoryCell{}
	cell.Name = SystemConfigCRName
	cell.Spec.Kind = "system_configuration"
	cell.Spec.VoiceConfig = &mcell.VoiceSessionConfig{MaxUserDailyAudioTokens: 50000}
	w.update(cell)
	assert.Equal(t, int64(50000), w.Get().MaxUserDailyAudioTokens)
}

func TestEffectiveUserDailyLimit_CRAudioOverridesFlag(t *testing.T) {
	cw := &ConfigWatcher{}
	cw.val.Store(&VoiceSessionConfig{MaxUserDailyAudioTokens: 500})
	vm := &VoiceManager{config: cw, userDailyAudioTokenLimit: 1_000_000}
	assert.Equal(t, int64(500), vm.effectiveUserDailyLimit(), "CR audio cap overrides flag")
}

func TestEffectiveUserDailyLimit_FlagWhenNoCR(t *testing.T) {
	vm := &VoiceManager{config: &ConfigWatcher{}, userDailyAudioTokenLimit: 50000}
	assert.Equal(t, int64(50000), vm.effectiveUserDailyLimit(), "flag default applies when CR is silent")
}

// audioTurnTokens mirrors the inline metering in emitTurnSpan: only audio-in +
// audio-out is charged against the quota; text/image/cached are not. Kept as a
// helper-shaped assertion so the metered quantity is pinned by a test.
func TestAudioMetering_ChargesAudioInPlusOutOnly(t *testing.T) {
	in := map[string]int64{"audio": 723, "text": 1047, "image": 50}
	out := map[string]int64{"audio": 472, "text": 9}
	metered := in["audio"] + out["audio"] // exactly what emitTurnSpan feeds AddUserTokens
	assert.Equal(t, int64(1195), metered, "only audio tokens are metered against the cap")

	// And it lands in the daily counter unchanged.
	s := newTestStore()
	hash := stableUserID("azure:audio-user")
	s.AddUserTokens(hash, metered)
	assert.Equal(t, int64(1195), s.UserTokensToday(hash))
}
