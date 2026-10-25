# ADR-032: Public Exposure Hardening (Round 2)

**Status:** Implemented  
**Date:** 2026-05-27  

## Context

ADR-027 closed the first round of pre-public gaps (auth-disabled fail-open,
CORS `*`, MCP token startup guard, org/tenant warnings). A second security
review before exposing X-Ray X-Ray on the public internet found gaps ADR-027
did not cover. Two are exploitable against authenticated users / the shared
graph; the rest are DoS and defense-in-depth.

1. **MCP write/delete fails open (CRITICAL).** The bearer check only ran when
   the resolved token was non-empty (`mcpserver.go`); a missing Secret, missing
   `token` key, or an un-synced informer left `upsert_memory` / `delete_memory`
   unauthenticated. The chart also routed `/mcp` on the public app hostname.

2. **Voice WebSocket accepts any Origin (CRITICAL).** `CheckOrigin` returned
   `true` unconditionally. The session cookie is `SameSite=Lax`, which does not
   block cross-site WebSocket handshakes, so any site a logged-in user visits
   could open `wss://…/v1/voice` with the victim's cookie (Cross-Site WebSocket
   Hijacking) and spend their Gemini quota / drive tool calls as them.

3. **No rate limiting or concurrency cap (HIGH).** Nothing bounded voice
   session starts, concurrent sessions, or MCP call volume — a financial DoS on
   Gemini Live / Vertex AI embeddings and a goroutine/memory DoS.

4. **No request body size limit (HIGH).** No `MaxBytesReader`; the in-process
   item map is unbounded and `Upsert` did not cap title/content/image size.

5. **Image endpoint reflects attacker-controlled MIME (HIGH).** The stored
   `image` MIME was echoed verbatim as `Content-Type` with no allowlist and no
   `nosniff`; a stored `text/html` or `image/svg+xml` payload executed as
   same-origin script (stored XSS).

6. **No security response headers + debug logging in prod (MEDIUM).** No CSP /
   HSTS / X-Frame-Options / nosniff; chart shipped `logLevel: debug`, which logs
   voice tool args/results and user content.

## Decision

### 1. MCP auth fails closed + opt-in public routing
The MCP handler refuses **all** requests (`503`) when the resolved token is
empty, and uses `crypto/subtle.ConstantTimeCompare` for the token check.
Routing `/mcp` on the public app hostname is gated behind a new
`mcp.exposeOnPublicHost` (default `false`) — prefer the dedicated `mcp.hostname`
route or in-cluster access.

### 2. WebSocket Origin validation
`newWSUpgrader(allowedOrigin)` validates the `Origin` header against the
configured public URL (scheme+host, case-insensitive). Empty `allowedOrigin`
(dev mode) disables the check. Requests with no `Origin` (non-browser clients)
are allowed — CSWSH requires a browser. `allowedOrigin` is the same value
already wired for CORS.

### 3. Rate limiting + concurrency cap
A reusable `keyedRateLimiter` (token bucket, per-key, with an hourly idle
sweep) replaces the dead PIN-era limiter. Voice session starts are limited per
user (or per client IP when unauthenticated); MCP calls are limited per client
IP. `--max-concurrent-voice` caps simultaneous voice sessions.

### 4. Request body + item size limits
`--max-request-bytes` (default 1 MiB) wraps non-WebSocket handlers in
`http.MaxBytesReader` (`/v1/voice` is exempt — it hijacks the connection).
`Upsert` rejects oversized title/content/image at the store boundary, which
the MCP server (separate port, bypasses the HTTP limit) also passes through.

### 5. Image MIME allowlist + nosniff
`allowedImageMime` normalizes and allowlists raster types
(jpeg/png/webp/gif); anything else (notably `text/html`, `image/svg+xml`) is
rejected `415`. The endpoint always sends `X-Content-Type-Options: nosniff`
and `Content-Disposition: inline`.

### 6. Security headers + prod log level
A `securityHeaders` middleware sets `X-Content-Type-Options: nosniff`,
`X-Frame-Options: DENY`, `Referrer-Policy`, `Content-Security-Policy:
frame-ancestors 'none'`, and `Strict-Transport-Security` (only when the request
is HTTPS, honoring `X-Forwarded-Proto`). The chart default `logLevel` is `info`.

## Consequences

- A public deployment must set `--public-url` (already required for OAuth) for
  the WS Origin check to engage; in dev (`--dev`, empty origin) it is disabled.
- MCP is no longer reachable on the public app hostname unless explicitly
  opted in, and never serves unauthenticated requests.
- Operators get `--max-concurrent-voice` and `--max-request-bytes` knobs;
  defaults (20 / 1 MiB) are set in the xray-xray chart.
- No behaviour change for correctly configured prod deployments beyond the new
  caps and headers.

## Files Touched

1. `internal/xray/voice.go` — `newWSUpgrader`/`sameOrigin`, per-manager
   upgrader, `maxConcurrent`, `startLimiter`, checks in `ServeHTTP`.
2. `internal/xray/mcpserver.go` — fail-closed + constant-time auth, per-IP
   rate limiter.
3. `internal/xray/server.go` — `securityHeaders` + `limitBody` middleware,
   `allowedImageMime`, image endpoint nosniff, `maxBodyBytes` field.
4. `internal/xray/memory.go` — `Upsert` size caps + constants.
5. `internal/xray/auth.go` — `keyedRateLimiter` (replaces dead PIN limiter).
6. `cmd/xray/main.go` — `--max-concurrent-voice`, `--max-request-bytes`,
   wiring `allowedOrigin` into the VoiceManager.
7. `charts/xray-xray/` — `mcp.exposeOnPublicHost`, `voice.maxConcurrent`,
   `maxRequestBytes`, `logLevel: info`, httproute gating, deployment args.
