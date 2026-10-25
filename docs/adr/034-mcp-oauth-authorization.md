# ADR-034: MCP OAuth Authorization — X-Ray as the Authorization Server

**Status:** Implemented  
**Date:** 2026-05-28  
## Context

The X-Ray MCP server (`mcpserver.go`) authenticated with a single static shared
Bearer secret (`--mcp-token-secret`, ADR-004/ADR-032). It had no per-user identity,
no scopes, and was never reachable through the public nginx ingress (which routes
only `/` → port 80; the MCP listener was on `:8083`). The live failure surfaced in
the Claude Code MCP client as:

```
X-Ray MCP server  ✘ failed
Issue: Streamable HTTP error: Unexpected content type: text/html  ✘ not authenticated
URL:   https://xray.example.com/mcp
```

Root cause: `/mcp` hit the main server, fell through to the Next.js UI catch-all,
and returned `text/html` — the MCP server was unreachable. Even once routed, the
client's "Authenticate" affordance expects an OAuth flow (RFC 9728 discovery), which
the static-secret server did not provide.

The MCP authorization spec (modelcontextprotocol.io/specification/draft/basic/authorization)
makes the MCP server an OAuth 2.1 **Protected Resource** that validates tokens minted
by an **Authorization Server (AS)**. The spec explicitly permits the AS to be
**co-located with the resource server**, and marks Dynamic Client Registration (DCR,
RFC 7591) as optional (MAY). RFC 9728 Protected Resource Metadata is required (MUST).

A localhost prototype against the real Claude Code client established a decisive
finding: **Claude Code requires DCR.** With no `registration_endpoint` advertised it
refuses with *"Incompatible auth server: does not support dynamic client
registration."* So DCR is not optional for our client.

## Decision

### 1. X-Ray is its own OAuth 2.1 Authorization Server

X-Ray already authenticates users via GitHub/Entra (as an OAuth *client* to those
upstream IdPs, minting a `xray_session` cookie). It now *also* acts as a minimal
OAuth 2.1 AS for MCP: it brokers that existing login into short-lived,
audience-bound JWT access tokens. No Keycloak, no new infrastructure.

`internal/xray/mcpauth.go` (`mcpAuthorizer`) implements:

- `GET /.well-known/oauth-protected-resource` (RFC 9728): `resource`,
  `authorization_servers`, `scopes_supported`.
- `GET /.well-known/oauth-authorization-server` (RFC 8414): issuer + authorize/token/
  register/jwks endpoints, `code_challenge_methods_supported: ["S256"]`.
- `GET /.well-known/jwks.json`: RS256 public key (with `kid`).
- `POST /register` (RFC 7591 DCR): records the client's `redirect_uris`, returns a
  `client_id`. Public client (PKCE, no secret).
- `GET /authorize`: OAuth 2.1 auth-code + PKCE (S256 mandatory). Resolves the user
  from the existing `xray_session` cookie; binds the code to the PKCE challenge,
  resource, and resolved scopes. Validates `redirect_uri` against the registered
  client.
- `POST /token`: verifies PKCE, single-use code, aud match → issues a signed JWT.

### 2. Served on the main HTTP server (no separate listener, no extra ingress path)

`/mcp` and all OAuth endpoints mount on the main `:8080` mux (`Server.SetMCPAuth`).
The existing nginx `/` → port 80 route already reaches them, so **no ingress change
is required** and the dual-port routing problem disappears. The standalone `:8083`
listener is retired when OAuth is on. The `withUI` and `requireSession` path lists
were extended so `/mcp`, `/.well-known/*`, `/register`, `/authorize`, `/token` reach
the mux instead of the UI catch-all (this was the original `text/html` bug).

### 3. Audience-bound JWT access tokens, per-tool scopes

Tokens are RS256 JWTs: `iss` = public URL, `aud` = MCP resource URL (rejected on
mismatch, RFC 8707), `sub` = userID, `scope`, short TTL (default 15m). Validated
statelessly with `golang-jwt/jwt/v5` (no hand-rolled crypto). Two scopes:
`xray:memory.read` (read tools) and `xray:memory.write` (`upsert_memory`,
`delete_memory`). The MCP handler peeks the JSON-RPC body for the `tools/call` name
and enforces the required scope (403 on insufficient). `writeAllowedUsers` gates
granting write scope at `/authorize` time.

### 4. Static-secret fallback retained

The MCP endpoint accepts **two** credentials, tried in order: (1) an X-Ray OAuth JWT
(per-user scopes); (2) the static `--mcp-token-secret` (constant-time compare,
treated as full scope) for automated/in-cluster callers. With neither configured the
endpoint fails closed (503) — it must never serve unauthenticated.

### 5. Pre-login model

`/authorize` requires an existing `xray_session`; an unauthenticated browser is
redirected to the UI `/login`. The user logs into the web app once, then re-triggers
MCP auth. (`--dev` issues a synthetic user so the flow is testable without an IdP.)

### 6. Public-endpoint hardening (post-review)

A security review of the public surface drove two fixes:

- **Rate limiting + bounded state.** `/register`, `/authorize`, and `/token` are
  unauthenticated, so they are throttled per client IP (keyed token bucket, ~5 req/s,
  burst 20) — previously only `/mcp` was. The in-memory client and auth-code maps are
  capped (`maxOAuthClients`/`maxAuthCodes` = 10000) with oldest-first eviction, so an
  unauthenticated flood cannot exhaust pod memory.
- **`redirect_uri` validation fails closed.** `/authorize` now **rejects** an empty or
  unregistered `client_id` in production (previously it waved unknown clients through).
  The registered `redirect_uri` allowlist is the defense against redirecting an
  authorization code to an attacker-controlled URL. `--dev` still permits an empty
  `client_id` so curl can drive the flow without registering.

The crypto path was reviewed and accepted as-is: RS256 with `jwt.WithValidMethods(["RS256"])`
(blocks alg:none / HS256 key-confusion), issuer + audience + expiry enforced, PKCE S256
mandatory and constant-time compared, single-use codes.

## Consequences

- The Claude Code client completes the full flow end-to-end (DCR → discovery →
  authorize+PKCE → token → authenticated `/mcp`), verified locally.
- DCR is mandatory for this client despite the spec marking it optional.
- **The RS256 signing key is ephemeral (regenerated per process).** Tokens do not
  survive a pod restart; clients re-authenticate (cheap given DCR + an existing
  web-app session). Persisting the key to a Secret is a documented follow-up.
- Refresh tokens are not implemented; clients re-run the short flow on expiry.

## Alternatives considered

- **Run Keycloak as a dedicated AS.** Rejected — the spec allows a co-located AS, and
  X-Ray already owns the upstream login; Keycloak is infra to operate for no gain.
- **Reuse GitHub as the AS.** Rejected — GitHub is not a full OIDC/OAuth-2.1 AS (no
  DCR, no audience-bound access tokens, no JWKS for API tokens).
- **Drop the static secret entirely.** Rejected — automated/in-cluster writers
  (scripts, voice layer) need a non-interactive credential.
- **Resume login back to `/authorize` via a signed return cookie.** Deferred — the
  pre-login model is simpler and avoids touching the shared web-app login path.
