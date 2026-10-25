# ADR-047: Session Persistence on `system_configuration` CR Status

**Status:** Implemented
**Date:** 2026-06-12
**Builds on:** [ADR-031](031-per-user-daily-token-limit.md) / [ADR-042](042-audio-token-quota.md) (per-user `UserDaily` rows on CR status — the pattern this reuses), [ADR-027] (`requireSession` auth gate)

## Context

Authenticated sessions live in a process-local map (`internal/xray/auth.go`):

```go
var sessions = &sessionStore{entries: make(map[string]sessionEntry)}
```

It is wiped on every process exit. Two consequences:

1. **Every deploy/restart logs everyone out.** A rolling update — the common case
   — invalidates all sessions; every active user is bounced to the OAuth login
   page mid-use. With a 24h session TTL but multiple deploys a day, the TTL is
   meaningless: the restart, not the expiry, ends the session.
2. **The map cannot be shared across replicas.** A session minted on pod A does
   not exist on pod B. Scaling past `replicaCount: 1` would require sticky
   sessions at the LB, or every other-replica request 401s. This is the wall the
   X-Ray scale work hit.

We already solve the *identical* problem for a different piece of per-user state.
**`RuntimeStats.UserDaily`** (ADR-031/042) is a per-user, restart-surviving,
replica-shared set: each user's daily token spend is one row on
`system_configuration` CR `.status.stats`, flushed by `flushStats`, restored by
`RestoreStats` on the watch event, pruned to active users so status stays
bounded. The `ConfigWatcher` informer that loads the CR already exists and already
fans changes to every replica. Sessions are the same shape: one bounded row per
active user, keyed by `userHash`.

## Decision

**Persist the session set as `RuntimeStats.Sessions` on the `system_configuration`
CR status, reusing the exact `UserDaily` machinery.** A session created on any pod
is flushed to status; the shared informer delivers it to every replica and to the
next process after a restart, which rehydrates it. Sessions survive restarts and
are visible across replicas with **zero new infrastructure** — no Redis, no extra
Secret, no new flag.

```go
type UserSession struct {
    UserHash    string // SHA-256(userID)[:16] — keys the row; re-login replaces in place
    TokenHMAC   string // HMAC-SHA256(token, serverKey) — NOT the raw token
    IdentityEnc string // AES-256-GCM(username "\x00" userID) — NOT cleartext PII
    ExpiresAt   string // RFC3339; prune key, analogous to UserDailyTokens.Day
}
```

Wiring (each piece mirrors its `UserDaily` sibling):

| Concern | `UserDaily` (existing) | `Sessions` (this ADR) |
|---|---|---|
| Snapshot for flush | `snapshotUserDaily()` | `sessionStore.snapshotSessions()` |
| Write to status | `flushStats` → `.status.stats.userDaily` | same flush → `.status.stats.sessions` |
| Restore on watch | `RestoreStats` (merge, never lower) | `restoreSessions` (merge, never clobber) |
| Bound | prune past-day rows | prune expired rows |
| Flush trigger | search/voice counters | `create`/`invalidate` via `onChange` hook |

`SetSessionPersistence(hmacKey, onChange)` is called once at startup, right after
the CR writer is wired, with `onChange = memory.FlushStats`.

## Why status holds no token and no cleartext identity

A session token is a live bearer credential. Anyone with `get mcell` RBAC could
read it from status and impersonate the user for up to 24h. So status holds only
`TokenHMAC = HMAC-SHA256(token, serverKey)` — `validate()` hashes the incoming
cookie and compares. The status subresource holds **no usable credential**.

The same RBAC reader must also not learn **who is logged in**. Storing the login
name (`Alice.Example`) and SSO ID (`azure:00000000-…`) in clear would be a PII
leak into etcd — and inconsistent with hashing the token. But `validate()` must
still return the real `username`/`userID` after a restart, because voice
**write-gating** matches `WriteAllowedUsers` on the username and **token metering**
keys on the userID. So identity is neither stored in clear nor dropped: it is
sealed as `IdentityEnc = AES-256-GCM(username "\x00" userID)` under a key derived
from the server secret (`SHA-256(hmacKey ‖ "identity")`, domain-separated from the
token HMAC). `restoreSessions` decrypts it to recover live identity; a blob that
fails GCM authentication (tampered, or sealed under a different key) is rejected,
not trusted. `UserHash = SHA-256(userID)[:16]` remains in clear as the row key — it
is already a one-way hash (the same value `UserDaily` stores) and reveals nothing.

**The HMAC key must be stable across restarts and identical across replicas** —
otherwise a token hashed on pod A wouldn't match the row restored on pod B, and a
restart would invalidate every persisted session (defeating the point). It is
derived as `SHA-256(oauthClientSecret ‖ "xray-session-hmac-v1")`: the OAuth
client secret is injected from the same k8s Secret on every pod and does not
change across restarts, so the derived key is stable and shared without adding a
new Secret or flag. The per-process `stateSigningKey` (login-CSRF state cookie) is
deliberately **not** reused — it is regenerated each process precisely because
state is short-lived, the opposite of what sessions need.

When no OAuth client secret is configured (local `--dev`, no k8s), the key is nil
and persistence is disabled — sessions stay in-memory only, exactly as before.

## Restore keying and the self-echo invariant

The raw token is unrecoverable from status (only its HMAC is stored), so a
restored session is keyed in the live map by its `TokenHMAC`. `validate(token)`
therefore tries the raw token first and, on a miss, falls back to an HMAC lookup;
on that hit it **re-keys** the entry under the raw token so subsequent requests
take the fast path.

`restoreSessions` runs on *every* `system_configuration` watch event, and
`flushStats` writes that same CR — so a flush echoes its own snapshot back. As
with `RestoreStats`'s merge-by-max discipline, restore must never clobber a live
entry: it skips any row whose HMAC is already held (as a live raw token or an
already-restored HMAC key), and skips expired or unparseable rows. A genuine
restart starts from an empty map and rehydrates fully.

## Debounced flush — one writer, not one-per-request

The persisted snapshot is written by a **single background flusher**, not inline on
the request path. Search/voice/login call `markStatsDirty()` (an atomic flag flip,
no etcd), and `StartStatsFlusher` writes the CR at most once per interval (5s), and
only when the flag is set. It flushes once more on context cancellation so the last
window's state survives a graceful shutdown.

This replaces an earlier design where `Search` did `defer flushStats()` — a full
`Get`+`Status().Update` round-trip **per search**. That coupled a hot read path to
an etcd write: 10 concurrent searches meant 10 reads + 10 writes racing on one
object (most failing `409 Conflict` and silently dropped), and at N replicas it
multiplied. The counters are already in-process `atomic.Int64`; persisting them is
only to survive restart and feed the stats UI, neither of which needs per-request
freshness. Debouncing collapses unbounded request volume to a handful of writes per
minute, and idle pods write nothing.

## Consequences

- **Restarts no longer log users out.** Sessions survive rolling deploys up to
  their real 24h TTL.
- **Search throughput is no longer gated on an etcd write** — the hot path only
  flips a flag; the single flusher does the I/O off the request path.
- **`replicaCount > 1` works** without sticky sessions — every replica converges
  on the same session set via the shared informer. This unblocks the X-Ray
  horizontal-scale path.
- **Login/logout marks stats dirty**, persisted on the next flush tick (≤5s) — not
  a synchronous write. A restart in that sub-5s window loses at most the one session
  just created, an acceptable trade for keeping the write off the request path.
- **Status size stays bounded by active sessions** — one row per live session,
  expired rows pruned on flush, exactly like `UserDaily`.
- **No new dependency, Secret, or flag.** Pure reuse of the `UserDaily` pattern
  and the OAuth-secret-derived key.

## Tests

`TestSessionStore_SurvivesRestart`, `TestSessionStore_SnapshotStoresHashNotRawToken`,
`TestSessionStore_PersistenceDisabledWithoutKey`, `TestSessionStore_SnapshotPrunesExpired`,
`TestSessionStore_RestoreSkipsExpiredRows`, `TestSessionStore_RestoreDoesNotClobberLiveSession`,
`TestSessionStore_ValidateRekeysRestoredSession` (all in `internal/xray/auth_test.go`).
