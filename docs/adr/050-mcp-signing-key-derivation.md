# ADR-050: Deterministic MCP Signing Key (Multi-Replica Token Validation)

**Status:** Implemented
**Date:** 2026-06-26
**Commit:** (this commit)
**Builds on:** [ADR-034](034-mcp-oauth-authorization.md) (X-Ray as the MCP OAuth authorization server), [ADR-047](047-session-persistence-cr-status.md) (the derive-key-from-shared-secret pattern, `sessionHMACKey`), [ADR-049](049-mcp-oauth-client-persistence.md) (cross-replica state for MCP OAuth clients)

## Context

X-Ray is its own OAuth 2.1 authorization server for MCP (ADR-034). It mints
**RS256 JWT access tokens** signed with an RSA key and validates them on every
MCP request. Until now that key was generated **per process**:

```go
key, _ := rsa.GenerateKey(rand.Reader, 2048) // mcpauth.go, in newMCPAuthorizer
kid:     randB64(8),                          // random per process too
```

This is invisible at **one replica**. At **two or more replicas** it breaks MCP
auth: the load balancer routes a client's `register → authorize → token`
handshake and subsequent tool calls across pods, but

- replica A signs the access token with **A's key** and advertises **A's `kid`**;
- replica B's JWKS holds only **B's key**, so `validateMCPToken` rejects A's
  token (`401`).

To the MCP client this looks like the session died, so it re-runs the whole
OAuth dance — which lands on a different pod again and re-breaks. Observed as
`/mcp` "Authentication successful" immediately followed by "rejected them on
reconnect", flapping indefinitely.

Sessions (ADR-047) and the DCR client registry (ADR-049) already survive this
because both are **shared across replicas** — sessions via an HMAC key
**derived from the OAuth client secrets** (`sessionHMACKey`, identical on every
pod because every pod mounts the same Secret), clients via CR status. The MCP
**signing key** was the one piece of auth state that was never shared. This is
not a *restart-persistence* gap (ADR-047/049 already solve that); it is a
*concurrent-replica agreement* gap.

The user requirement: **do not persist the private key** (not to the CR, not to
a Secret, not to disk). It must be sharable and secure with no new at-rest
material.

## Decision

**Derive the MCP signing key deterministically from the same shared OAuth
client secrets `sessionHMACKey` already uses** — never generate it randomly,
never store it. Every replica recomputes the identical key in memory at startup;
nothing new is written down.

### 1. Switch the signing algorithm RS256 → EdDSA (Ed25519)

RSA key generation consumes a *variable* amount of its random stream (primality
search), so seeding `rsa.GenerateKey` with a deterministic reader is fragile and
not stable across Go versions. **Ed25519's private key is exactly a 32-byte
seed**, so deterministic derivation is exact and version-stable. It is also the
modern default for JWT signing (smaller tokens, faster verify). The token alg
becomes `EdDSA`; JWKS publishes an `OKP`/`Ed25519` key.

### 2. Derive the seed like `sessionHMACKey`

`mcpSigningSeed(secrets ...string) []byte` = SHA-256 over the non-empty OAuth
client secrets, each NUL-terminated, bound by the purpose label
`"xray-mcp-signing-v1"`. This is byte-for-byte the `sessionHMACKey`
construction with a **different label**, so the signing key is cryptographically
separated from the session HMAC key despite sharing root secret material.
Returns `nil` when no secret is configured — the caller then falls back to a
random per-process key (single-replica dev / `--dev`), exactly today's behavior.

### 3. Deterministic `kid`

`kid = base64url(SHA-256(pubkey))[:11]` so every replica derives the **same**
`kid` from the same public key — JWKS `kid` and the JWT header `kid` agree
cross-replica. (Random `randB64` would differ per pod even with a shared key.)

### 4. Wiring

`newMCPAuthorizer` takes an optional seed (`[]byte`). When non-nil it derives
the Ed25519 key from it; when nil it generates a random Ed25519 key (dev path).
`main.go` passes `mcpSigningSeed(githubClientSecret, azureClientSecret)` —
the same two secrets already fed to `sessionHMACKey`.

### Out of scope (documented, not fixed here)

Two pieces of authorizer state remain **per-replica in-memory** and can still
cause a *transient* cross-replica failure inside the ~2-second `authorize→token`
window:

- the single-use **authorize codes** map (`codes`), and
- the per-IP **rate limiter**.

A code issued by pod A cannot be redeemed on pod B. This is far rarer than the
signing-key break (one short round-trip vs. every request) and is left to a
follow-up (self-contained signed codes, or sticky affinity for the OAuth
endpoints). The signing-key fix alone removes the **persistent** breakage.

## Consequences

- **MCP auth works at N replicas** — any pod validates any pod's token; JWKS is
  identical everywhere. No `replicas: 1` constraint, no sticky affinity needed
  for steady-state requests.
- **Survives restart for free** — the key is a pure function of the Secret, so a
  restarted/rescheduled pod derives the identical key. The "signing key resets on
  restart" wart (even single-replica) is gone.
- **No new at-rest secret** — the private key exists only in process memory,
  recomputed each boot. Compromise requires the OAuth client secrets, already the
  most sensitive material in the deployment. No new trust surface.
- **Rotation** — bump the label to `…-v2` to roll the signing key without
  touching the client secrets.
- **Tokens are smaller** (Ed25519 vs RSA-2048) and verify faster.
- **Existing tokens are invalidated** on deploy (alg + key change). MCP clients
  re-auth once; acceptable for an access-token TTL measured in minutes.

## Tests

`TestMCPSigningSeed_StableAndSeparated`, `TestMCPAuth_DerivedKeySharedAcrossReplicas`,
`TestMCPAuth_DerivedKidIsDeterministic`, `TestMCPAuth_ValidateRejectsForeignKey`
(updated to use a distinct seed), `TestMCPAuth_SignAndValidate`,
`TestMCPAuth_JWKSPublishesEd25519`.
