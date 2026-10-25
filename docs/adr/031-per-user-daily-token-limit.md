# ADR-031: Per-User Daily Gemini Live Token Limit

**Status:** Implemented  
**Date:** 2026-05-27  

## Context

Voice sessions stream bidirectional audio to Gemini Live, which is the only
token-consuming path in X-Ray and the dominant cost driver. A single user can
open back-to-back sessions with no ceiling on spend. There was no per-user cost
control: the only counters were per-session (`sessionTotalTokens` in
`voice.go`), discarded when the WebSocket closed.

The prerequisites for a real quota already exist:

- **User identity is resolved at session start.** `ServeHTTP` validates the
  `xray_session` cookie or `Authorization: Bearer` token and derives
  `userHash = stableUserID(rawUserID)` — SHA-256(rawSSOID)[:16]. Raw SSO IDs are
  never stored. (The original todo predates OAuth and assumed voice had no auth;
  that premise is stale — see ADR-022, ADR-026.)
- **Per-turn token usage is already captured.** `emitTurnSpan` reads
  `genai.UsageMetadata` (`PromptTokenCount` / `ResponseTokenCount` /
  `TotalTokenCount`) on every turn.
- **CR status persistence is already wired.** `MemoryStore.flushStats` writes a
  `RuntimeStats` snapshot to the `system_configuration` MemoryCell's
  `.status.stats` on every session start/end. It degrades gracefully to a no-op
  when k8s is unavailable.

## Decision

Add a **per-user, per-UTC-calendar-day token cap** that **rejects new voice
sessions** once a user is over their daily limit. Existing in-flight sessions are
not terminated.

### Accounting

- `MemoryStore` holds an in-memory `map[userHash]{day, tokens}` guarded by
  `usageMu`. `AddUserTokens(hash, n)` rolls the counter over when the UTC day
  (`utcDay()` → "YYYY-MM-DD") changes; `UserTokensToday(hash)` returns 0 for a
  stale-day or unknown user. Empty `userHash` (unauthenticated / dev sessions) is
  never metered — there is no stable key.
- `emitTurnSpan` calls `s.memory.AddUserTokens(s.userHash, total)` after the
  existing session atomics.

### Persistence (reuse system_configuration CR status)

No new CR and no new storage layer. The per-user map is mirrored into the
existing `RuntimeStats` on the `system_configuration` cell:

- `RuntimeStats` gains `UserDaily []UserDailyTokens` (`{UserHash, Day, Tokens}`).
- `flushStats` calls `snapshotUserDaily()`, which serializes today's rows and
  **prunes past-day rows** from both the snapshot and the in-memory map, so
  status size stays bounded by active users.
- `RestoreStats` rehydrates only rows whose `Day == today`, so a quota check is
  accurate immediately after a pod restart. Because the `ConfigWatcher` watches
  the same CR that `flushStats` writes, every flush echoes back into
  `RestoreStats` — so it **merges per-user rows by max** rather than replacing the
  map. A stale or self-echoed snapshot can only fill in or raise a count, never
  lower a counter the live session has already advanced (the alternative clobbered
  the live value and made the HUD oscillate).

Flush cadence is unchanged: start/end of every session (`TrackVoiceCall` /
`TrackVoiceEnd`). A hard crash loses at most one in-flight session's tail tokens
— an accepted trade for not writing the CR on every turn.

### Enforcement

`ServeHTTP` checks the limit **before** the WebSocket upgrade:

```
limit := flag --user-daily-token-limit
if CR maxUserDailyTokens != 0 { limit = CR value }   // CR overrides flag
if limit > 0 && userHash != "" && UserTokensToday(userHash) >= limit:
    HTTP 429 Too Many Requests
```

`limit == 0` means unlimited. The check is `>=` so a user exactly at the cap is
rejected.

### Configuration (flag + CR override)

- `--user-daily-token-limit int64` (default `0`) — the cluster-wide flag default.
- `VoiceSessionConfig.MaxUserDailyTokens` on the `system_configuration` CR —
  overrides the flag when non-zero, hot-reloadable via the ConfigWatcher (no
  redeploy). Matches the existing `--voice-name` / `--voice-model` override
  pattern.
- Helm: `voice.userDailyTokenLimit` (flag) defaults to `100000`;
  `voice.systemConfig.maxUserDailyTokens` (CR) defaults to `0` (override
  opt-in). The CR field is omitted from the rendered manifest when `0`, so the
  shipped cap of 100k tokens/user/day comes from the flag unless an operator sets
  the CR value.

### UI surfacing

The X-Ray `StatsHud` shows the caller their own quota alongside the existing
counters (sessions, searches, tool latency). `GET /v1/memory/metrics` resolves
the caller from the `xray_session` cookie (`Server.callerIdentity`) and, when a
`VoiceManager` is attached, adds a `quota` block:

- authenticated + capped → `{used, limit, remaining}` (`remaining` floored at 0);
- unlimited (effective limit 0) → `{limit: 0}`, rendered as `tokens unlimited`;
- unauthenticated → no `quota` block (the endpoint is otherwise public/aggregate).

`VoiceManager.UserQuota(rawUserID)` computes this from the same
`effectiveUserDailyLimit()` (CR-over-flag) used by the enforcement gate, so the
displayed cap and the enforced cap can never diverge. The HUD renders
`tokens <used> / <limit>` with thousands separators. `fetchMetrics` sends the
session cookie (`credentials: "include"`).

**Live updates during a session.** Polling alone lags by up to the 30s metrics
interval. While a voice session is open, the server pushes a `quota`
(`{used, limit, remaining}`) on every turn over the existing voice WebSocket
(`pushQuota` in `emitTurnSpan`, after `AddUserTokens`). The browser writes it to
a small `quota-store` (`setLiveQuota`/`useLiveQuota` via `useSyncExternalStore`);
`StatsHud` prefers the live value and falls back to the polled metrics when no
session is active (cleared on session teardown). No new endpoint — it rides the
audio WS the session already holds.

### Mid-session exhaustion

The admission gate only blocks *new* sessions; a session already open can spend
past the cap. When `remaining` reaches 0 mid-session, `pushQuota` injects a
one-shot system turn telling the model to say a brief goodbye and call
`end_session` (`shouldTriggerLimitFarewell` guards it to once per session via
CAS). The existing audio-gated hangup (ADR-015) then disconnects once the
farewell finishes playing. If the injection fails, `pendingHangup` is set as a
safety net so the session still ends. This is best-effort termination — the model
speaks the farewell rather than a hard cut — not a hard kill.

## Consequences

- Cost is bounded per authenticated user per day, surviving pod restarts via CR
  status.
- **Unauthenticated voice is uncapped.** With no stable identity there is no key
  to meter; dev mode and any future anonymous path bypass the quota. Blocking
  anonymous voice outright is a separate decision.
- Enforcement is primarily at session admission (429). A session already open is
  not hard-killed at the cap; instead, on exhaustion the model is prompted to say
  goodbye and call `end_session`, so a user can overshoot by roughly the tokens of
  one farewell turn. Hard mid-turn termination remains out of scope.
- The token figures come from Gemini's `UsageMetadata`; if the SDK stops
  populating it, accounting silently undercounts (the per-turn span would also go
  empty, so this is observable in traces).

## Alternatives Considered

- **Per-session cap only** — trivially bypassed by reconnecting; rejected.
- **Dedicated quota CR** — more moving parts than reusing `RuntimeStats`, which
  already round-trips through the cell the ConfigWatcher watches.
- **Per-turn CR flush** — durable to the last turn but chatty against the API
  server for marginal benefit; rejected in favor of start/end flush.
