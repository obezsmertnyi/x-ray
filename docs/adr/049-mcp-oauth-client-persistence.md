# ADR-049: MCP OAuth Dynamic-Client Registry Persistence

**Status:** Implemented
**Date:** 2026-06-12
**Builds on:** [ADR-034](034-mcp-oauth-authorization.md) (X-Ray as the MCP OAuth authorization server), [ADR-047](047-session-persistence-cr-status.md) (the persist-to-CR-status pattern and the key-before-mgr.Start ordering lesson)

## Context

X-Ray is its own OAuth 2.1 authorization server for MCP (ADR-034). MCP clients
like Claude Code use RFC 7591 **dynamic client registration**: they `POST
/register`, get a `client_id`, cache it, and reuse it on every subsequent
`/authorize`. The server validates the `/authorize` `redirect_uri` against the
`redirect_uris` recorded at registration.

That registry was an **in-process map** (`mcpAuthorizer.clients`), wiped on every
restart. So:

1. Claude Code registers → gets `client_id` → caches it.
2. Pod restarts (deploy, crash, scale event) → the map is empty.
3. Claude Code reuses its cached `client_id` at `/authorize` → the server doesn't
   know it → **`invalid_request: redirect_uri not registered for client`**.
4. Claude Code must re-register and the user re-authorizes — **every restart.**

This is the same class of bug ADR-047 fixed for browser sessions, on a different
map. It is the actual cause of "Claude Code asks to re-auth every restart" — *not*
the browser session store (which ADR-047 already handles).

(The RSA token-signing key is also per-process — a separate restart/multi-replica
gap noted in ADR-034 — and is **not** addressed here; this ADR covers only the
client registry.)

## Decision

**Persist the DCR client registry to `system_configuration` CR status**
(`RuntimeStats.OAuthClients`), restored on startup via the same ConfigWatcher
informer. A client registered before a restart stays known afterward and across
replicas, so its cached `client_id` keeps working — no re-registration, no
re-auth.

The registrations are **public-client, non-secret**: only the `client_id` and its
`redirect_uris` (PKCE, no client secret). So unlike sessions, nothing is hashed or
encrypted — the rows are stored plainly.

```go
type OAuthClientReg struct {
    ClientID     string   // issued public client id
    RedirectURIs []string // allowlist for /authorize redirect_uri validation
    IssuedAt     string   // RFC3339; eviction key at the cap
}
```

Wiring mirrors ADR-047's session machinery exactly:

| Concern | mechanism |
|---|---|
| Snapshot for flush | `oauthClientStore.snapshot()` → `flushStats` → `.status.stats.oauthClients` |
| Restore on watch | `oauthClientStore.restore()` from `RestoreStats` |
| Flush trigger | `/register` → `add()` → `onChange` (marks stats dirty; debounced flusher writes) |
| Bound | evict oldest at `maxOAuthClients` |
| No-clobber | `restore` never overwrites a `client_id` already held (self-echo safe) |

## Why a package-level store (and the ordering that matters)

The registry is a **package-level `oauthClients` var**, not a field on
`mcpAuthorizer`. This is deliberate and is the direct lesson from ADR-047's
post-deploy bug:

The informer replays the existing `system_configuration` CR **exactly once**
during cache-sync, which drives `restore`. That replay happens early in startup —
**before** `mcpAuthorizer` is even constructed in `main.go` (it's built well after
`mgr.Start`). If the registry lived on the authorizer, the one restore opportunity
would find no authorizer (or an empty key, as the session bug did) and silently
restore nothing, never to fire again.

A package-level store exists at program init, so the first CR replay populates it
regardless of when the authorizer is wired. And `SetOAuthClientPersistence` (the
flush hook) is called **before `mgr.Start`**, so the hook is set when that first
replay runs — the same fix that made session persistence actually work.

## Consequences

- **Claude Code stays registered across restarts** — no `redirect_uri not
  registered`, no re-auth, as long as it reuses its cached `client_id`.
- **Works across replicas** — every pod converges on the same registry via the
  shared informer.
- **No new secret/flag/dependency** — reuses the `RuntimeStats` flush/restore and
  the debounced flusher.
- **Still open (out of scope):** the RSA token-signing key is per-process, so
  already-issued *access tokens* still fail validation after a restart / on another
  replica. Claude Code recovers from that with a silent token refresh (it still
  knows its client), but for true multi-replica token validity the key must be
  shared (k8s Secret). Tracked separately.

## Tests

`TestOAuthClientStore_SurvivesRestart`, `TestOAuthClientStore_RestoreDoesNotClobberLive`,
`TestOAuthClientStore_RestoreSkipsInvalid`, plus the existing
`TestMCPAuth_ClientMapIsBounded` (re-pointed at the package store) and the full
`TestMCPAuth_*` register→authorize→token suite (unchanged behavior).
