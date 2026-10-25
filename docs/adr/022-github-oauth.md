# ADR-022: GitHub OAuth for Public Access (YOUR_GITHUB_ORG org)

**Status:** Accepted — implemented, extended with stable userID scoping (see amendment below)  
**Todo:** [github-oauth](../todo/github-oauth.md)  

## Context

The `/avatar` PIN-auth route and PinPad component were removed in the memory-search
refactor (ADR-014 follow-on). The backend session store and `requireSession` middleware
still exist, but there is no frontend way to obtain a session token. All `/v1/*`
endpoints (including `/v1/voice`) must require authentication — a prerequisite for the
per-user quota work.

We want X-Ray to be accessible to anyone in the `YOUR_GITHUB_ORG` GitHub
organisation without managing a PIN database, while still establishing per-user identity
for voice quota enforcement downstream.

## Decision

Replace PIN auth with GitHub OAuth 2.0. Membership of the `YOUR_GITHUB_ORG` GitHub
organisation is the sole gate. The backend issues a session token stored in an
`HttpOnly` cookie (24h `Max-Age`). The browser sends the cookie automatically on every
request — including WebSocket upgrades — so the frontend needs no token management at
all.

MCP clients continue to use `Authorization: Bearer` header; `requireSession` checks
the cookie first, then falls back to the Bearer header.

### Flow

```
Browser                   Go server                GitHub API
  |                           |                        |
  |  GET /v1/auth/github      |                        |
  |-------------------------->|                        |
  |  302 → github.com/login/  |                        |
  |  oauth/authorize          |                        |
  |                           |                        |
  |  (user approves)          |                        |
  |  GET /v1/auth/github/     |                        |
  |  callback?code=&state=    |                        |
  |-------------------------->|                        |
  |                           |  POST /login/oauth/    |
  |                           |  access_token          |
  |                           |----------------------->|
  |                           |  { access_token }      |
  |                           |<-----------------------|
  |                           |  GET /user             |
  |                           |----------------------->|
  |                           |  { login, id, ... }    |
  |                           |<-----------------------|
  |                           |  GET /orgs/YOUR_GITHUB |
  |                           |  _ORG/members/         |
  |                           |  {login}               |
  |                           |----------------------->|
  |                           |  204 = member          |
  |                           |<-----------------------|
  |                           |  Set-Cookie:           |
  |                           |  xray_session=<hex>  |
  |  302 → /                  |  HttpOnly; Max-Age=86400|
  |<--------------------------|                        |
  |  (cookie stored by        |                        |
  |   browser automatically)  |                        |
```

### Endpoints added (all public — added to `requireSession` exemption list)

| Method | Path | Description |
|--------|------|-------------|
| GET | `/v1/auth/github` | Redirect to GitHub OAuth consent screen |
| GET | `/v1/auth/github/callback` | Exchange code, verify org, set cookie, redirect to `/` |
| GET | `/v1/auth/logout` | Clear cookie, invalidate session, redirect to `/login` |

### All other `/v1/*` endpoints now require auth

`requireSession` is updated to:
1. Check `xray_session` cookie — valid session → allow
2. Fall back to `Authorization: Bearer` header — valid token → allow (MCP clients)
3. Otherwise → redirect browser requests to `/login`, return `401` for API requests

This means `/v1/voice` (WebSocket) is now protected. The browser sends the cookie
automatically on the WebSocket upgrade request — no frontend change needed.

### Configuration

New CLI flags (sourced from a k8s Secret via env injection):

| Flag | Env var | Description |
|------|---------|-------------|
| `--github-client-id` | `GITHUB_CLIENT_ID` | OAuth App client ID (required) |
| `--github-client-secret` | `GITHUB_CLIENT_SECRET` | OAuth App client secret (required) |
| `--github-org` | `GITHUB_ORG` | Required org (default `YOUR_GITHUB_ORG`) |
| `--github-team` | `GITHUB_TEAM` | Optional team slug within org |
| `--public-url` | `PUBLIC_URL` | Externally reachable base URL (for callback) |

OAuth scopes requested: `read:org` (org membership check). `read:user` is implicit.

### CSRF protection

A random 16-byte hex `state` param is generated per `/v1/auth/github` request and stored
in a short-lived (10 min) in-memory map. The callback validates `state` before
proceeding. Constant-time comparison used.

### Frontend changes

- New `/login` page — "Sign in with GitHub" button → `GET /v1/auth/github`
- `XrayLayoutClient` calls `GET /v1/version` (or a new `GET /v1/auth/me`) to check
  session validity; redirects to `/login` on `401`
- `GET /v1/auth/logout` navigates to `/login`; cookie is cleared server-side
- No `localStorage`, no token headers, no auth hook — the browser cookie handles everything

### Helm / k8s

Add `github-oauth` Secret (manually created, referenced via `envFrom`). Add `github`
block to `values.yaml`:

```yaml
github:
  org: YOUR_GITHUB_ORG
  team: ""           # optional team slug
  secretName: github-oauth   # must contain GITHUB_CLIENT_ID, GITHUB_CLIENT_SECRET
  publicURL: ""      # e.g. https://xray.example.com
```

## Amendment: Stable UserID for Memory Scoping

**Problem:** GitHub `login` and Azure UPN short-form are human-readable but not globally unique. Two users named `Alice.Example` at different companies (or after an account rename) would share the same `subject` key, merging their personal notes.

**Decision:** Introduce a second identity field — `userID` — stored alongside `username` in `sessionEntry`:

| Provider | `username` (display) | `userID` (stable scope key) |
|----------|---------------------|----------------------------|
| GitHub | `login` (e.g. `octocat`) | `github:<numeric_id>` — never reassigned |
| Azure AD | UPN short form (e.g. `Alice.Example`) | `azure:<object_id>` — tenant-scoped UUID |
| PIN auth | username from store | `pin:<username>` |

- `username` is used for: display in UI, `writeAllowedUsers` check, system prompt identity injection
- `userID` is used for: `agent_note` mcell `subject`, `get_agent_notes` lookup, `/v1/user/notes` scoping
- `userID` is hashed (SHA-256, first 16 hex chars) before writing to traces/logs — SSO object IDs stay out of plain-text telemetry
- `GET /v1/auth/me` now returns both `username` and `userID`

**Files changed:** `internal/xray/auth.go` (sessionEntry), `github_auth.go` (fetch numeric ID), `azure_auth.go` (fetch object ID), `voice.go` (pass both to session), `voice_tools.go` (scope notes to userID), `server.go` (validate returns 3 values).

## Consequences

**Good:**
- No PIN management; any `YOUR_GITHUB_ORG` org member can self-serve
- Browser cookie persists for 24h — users stay logged in across reloads and tab closes
- All `/v1/*` endpoints including `/v1/voice` are now protected
- Frontend is completely free of token management logic
- MCP clients unaffected — Bearer header still accepted
- GitHub `login` stored in session enables per-user quota tracking

**Neutral:**
- Requires outbound HTTPS from the pod to `api.github.com` (already true for Gemini)
- OAuth App must be registered in the `YOUR_GITHUB_ORG` org (one-time setup)
- `--github-client-id` and `--github-client-secret` are required; server fails fast on startup if missing
- Cookie requires `Secure` flag in production (HTTPS only) — fine for the deployed environment
