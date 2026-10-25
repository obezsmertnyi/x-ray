# ADR-036: MCP is an Admin-Only Interface (userHash Allowlist, Fail-Closed)

**Status:** Implemented
**Date:** 2026-05-31
## Context

ADR-034 made X-Ray its own OAuth 2.1 authorization server for the MCP endpoint,
issuing per-user JWTs carrying `xray:memory.read` / `xray:memory.write`
scopes. But the authorizer was constructed with an empty write-allowlist
(`writeAllowed=nil` at `main.go`), and `scopesForUser` treated an empty
allowlist as **allow-all** — so every authenticated org user who completed the
OAuth flow received both read and write scopes.

Two problems compound:

1. **Read is itself privileged.** MCP `search_knowledge` / `get_knowledge_context`
   can retrieve any subject's cells — including another user's private notes — by
   passing that subject. There is no read-only tier that is actually safe.
2. **No per-item ownership check on write.** `delete_memory` / `upsert_memory`
   operate on any cell by ID; a write-scoped caller could overwrite or delete
   anyone's notes and any public cell.

MCP is meant to be an **operator/admin interface**, not a per-user one. The voice
layer already gates its write tools behind a `writeAllowedUsers` allowlist (from
the `system_configuration` CR), but that list was never wired into the MCP
authorizer.

## Decision

**MCP is admin-only. Access is gated by an allowlist, matched on the userHash,
and fails closed.**

- `mcpAccessForUser` / `isMCPAllowed` grant **full read+write** to a user on the
  allowlist and **nothing** to anyone else. There is no read-only tier — the
  interface is all-or-nothing.
- `/authorize` denies a logged-in but non-allowlisted user with OAuth
  `access_denied` (RFC 6749 §4.1.2.1) — they never receive a usable token (deny
  at issuance, not at the tool call).
- **Fail-closed:** an empty allowlist denies everyone, flipping the prior
  fail-open default. Provisioning is explicit.
- The allowlist is the existing **`system_configuration` CR `writeAllowedUsers`**,
  read live via a getter (`SetAllowFn` → `ConfigWatcher.Get().WriteAllowedUsers`),
  so edits apply without a restart — one source of truth shared with the voice
  write-gate.
- **Match key is the userHash** (stable SHA-256[:16] of the SSO ID), not the
  display name, which is spoofable. The voice write-gate (`buildVoiceTools`) is
  switched to the same userHash key so both consumers interpret
  `writeAllowedUsers` identically as a list of hashes.
- The static **`--mcp-token-secret`** bearer remains the out-of-band full-access
  admin/automation path (e.g. the upsert-memory skill), unaffected by the
  user allowlist. `--dev` bypasses the gate so the flow stays testable.

## Consequences

- MCP can no longer read or mutate one user's private data under another user's
  identity — non-admins get no token at all.
- The admin list lives in the CR as **userHashes**; a migration is required when
  adopting this (the prior value was a display name). The voice write-gate also
  now needs hashes, not names — a behavior change for that consumer.
- Per-item ownership checks on the MCP write handlers were considered and
  deferred: with MCP now admin-only, an admin legitimately manages any cell, so
  capability-gating at token issuance is sufficient for the stated model. If MCP
  is ever opened to per-user access, ownership checks become necessary.

## Tests

`TestMCPAuth_ScopesForUser` (admin-only + fail-closed empty list + live
`SetAllowFn` getter + dev bypass), `TestMCPAuth_AuthorizeDeniesNonAllowlistedUser`
(access_denied, no code minted), `TestMCPAuth_AuthorizeUsesExistingSession`
(allowlisted session → full scopes), `TestBuildVoiceTools_WriteGateMatchesUserHashNotName`
(voice gate keys on hash, not spoofable name).
