# ADR-027: Public Auth Hardening

**Status:** Implemented  
**Date:** 2026-05-25  
## Context

X-Ray X-Ray was reviewed before making the service publicly accessible. The review found four security gaps that must be closed:

1. **Auth bypass when no OAuth provider is configured.** `requireSession` skips all checks when neither `--github-client-id` nor `--azure-client-id` is set (server.go:75-78). This is intentional for local dev but catastrophic if a prod deployment forgets to set the flags.

2. **CORS allows all origins (`*`).** Any script on any page can read API responses. The frontend is served from a known domain; `*` is unnecessary.

3. **MCP server is unauthenticated when `--mcp-token-secret` is not set.** The bearer check at mcpserver.go:181 is skipped entirely when `getToken()` returns empty. Port `:8083` with write access to memory must not be publicly reachable without a token.

4. **Missing org/tenant restriction defaults.** OAuth login succeeds for any GitHub user or any Entra user when `--github-org` / `--azure-tenant-id` are not configured. There is no visible warning at startup.

## Decision

### 1. Require at least one OAuth provider in non-dev mode

Add a startup check in `cmd/xray/main.go`. If neither GitHub nor Azure client ID is set and `--dev` is not explicitly passed, log a fatal error and exit. For local dev, operators pass `--dev` to opt in to the unauthenticated mode.

Alternative considered: warn instead of fail. Rejected — silent misconfigurations are the highest-risk failure mode for a public service.

### 2. Restrict CORS to the configured public URL

Change `AllowedOrigins` from `[]string{"*"}` to the value of `--public-url` (already a required flag for OAuth callbacks). Fall back to `*` only when `--public-url` is empty and `--dev` is set.

### 3. Enforce MCP token in non-dev mode

If `--mcp-address` is non-empty and `--mcp-token-secret` is empty and `--dev` is not set, log a fatal error and exit. The MCP server must not be started without authentication in production.

### 4. Warn on missing org/tenant restriction

At startup, if an OAuth provider is configured but its corresponding restriction flag is empty (`--github-org` for GitHub, `--azure-tenant-id` for Azure), emit a structured warning log:

```
WARN  github OAuth is configured without --github-org: any GitHub user can log in
WARN  azure OAuth is configured without --azure-tenant-id: any Entra user can log in
```

This is a warning, not a fatal error, because some deployments intentionally allow any authenticated user. The operator must be aware of the choice.

Additionally, expose `/v1/auth/providers` and the `--github-org` / `--azure-tenant-id` values in the `/v1/version` response so ops tooling can detect misconfiguration programmatically.

## Consequences

- Local dev requires `--dev` flag. The Makefile `run` target will be updated to pass it.
- `--public-url` becomes effectively required for prod deployments (it already was for OAuth callbacks).
- MCP server deployments must set `--mcp-token-secret` or be behind a private network boundary.
- No behaviour change for correctly configured prod deployments.

## Files to Touch

1. `cmd/xray/main.go` — startup checks, `--dev` flag, CORS origin wiring, MCP guard
2. `internal/xray/server.go` — pass `allowedOrigin` to `cors.New`, update `requireSession` to accept a `devMode bool` or check origin
3. `internal/xray/mcpserver.go` — no change needed (the guard logic stays; the startup check prevents a tokenless start in prod)
4. `Makefile` — add `--dev` to the `run` target
5. `charts/xray-xray/values.yaml` — document `--github-org` / `--azure-tenant-id` in args comments
