# TODO: GitHub OAuth (YOUR_GITHUB_ORG org)

ADR: `docs/adr/022-github-oauth.md`

## Backend

### 1. New flags in `cmd/xray/main.go`

```go
githubClientID     = flag.String("github-client-id",     "",               "GitHub OAuth App client ID (required)")
githubClientSecret = flag.String("github-client-secret", "",               "GitHub OAuth App client secret (required)")
githubOrg          = flag.String("github-org",           "YOUR_GITHUB_ORG", "Required GitHub org for access")
githubTeam         = flag.String("github-team",          "",               "Optional GitHub team slug within org")
publicURL          = flag.String("public-url",           "",               "Externally reachable base URL (e.g. https://xray.example.com)")
```

Fail fast on startup if `--github-client-id` or `--github-client-secret` are empty.
Pass to `NewServer` via a `GitHubOAuthConfig` struct.

### 2. New file `internal/xray/github_auth.go`

- `GitHubOAuthConfig` struct: `ClientID`, `ClientSecret`, `Org`, `Team`, `PublicURL`
- `stateStore`: `sync.Map` of `state → expiresAt`; prune goroutine (10-min TTL)
- `handleGitHubLogin(w, r)` — generate 16-byte hex state, store it, redirect to:
  `https://github.com/login/oauth/authorize?client_id=...&redirect_uri=...&scope=read:org&state=...`
- `handleGitHubCallback(w, r)`:
  1. Validate `state` (constant-time, check expiry, delete after use)
  2. POST `https://github.com/login/oauth/access_token` with code
  3. GET `https://api.github.com/user` → extract `login`
  4. GET `https://api.github.com/orgs/{org}/members/{login}` → expect 204
  5. If `--github-team` set: GET `https://api.github.com/orgs/{org}/teams/{team}/memberships/{login}` → expect active state
  6. `token := sessions.create(login)`
  7. Set cookie:
     ```go
     http.SetCookie(w, &http.Cookie{
         Name:     "xray_session",
         Value:    token,
         Path:     "/",
         MaxAge:   86400,
         HttpOnly: true,
         Secure:   true,
         SameSite: http.SameSiteLaxMode,
     })
     ```
  8. `http.Redirect(w, r, "/", http.StatusFound)`
- `handleLogout(w, r)`:
  - Read token from `xray_session` cookie
  - Call `sessions.invalidate(token)`
  - Clear cookie (set `MaxAge: -1`)
  - Redirect to `/login`

### 3. Update `requireSession` in `server.go`

Remove all public endpoint exemptions except `/v1/auth/*`, `/healthz`, `/manifest.json`, `/sw.js`.

New logic:
```go
// 1. Cookie (browser / voice WebSocket)
if cookie, err := r.Cookie("xray_session"); err == nil {
    if _, ok := sessions.validate(cookie.Value); ok {
        next.ServeHTTP(w, r)
        return
    }
}
// 2. Bearer header (MCP clients)
if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
    if _, ok := sessions.validate(auth[7:]); ok {
        next.ServeHTTP(w, r)
        return
    }
}
// 3. Redirect browsers to /login, return 401 for API/WS
if strings.HasPrefix(r.URL.Path, "/v1/") {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(http.StatusUnauthorized)
    w.Write([]byte(`{"error":"unauthorized"}`))
    return
}
http.Redirect(w, r, "/login", http.StatusFound)
```

### 4. Register routes in `server.go` `build()`

```go
mux.HandleFunc("/v1/auth/github",          s.github.handleGitHubLogin)
mux.HandleFunc("/v1/auth/github/callback", s.github.handleGitHubCallback)
mux.HandleFunc("/v1/auth/logout",          s.github.handleLogout)
```

Add to `requireSession` public list:
```go
strings.HasPrefix(path, "/v1/auth/") ||
```

### 5. Add `sessions.invalidate(token)` to `internal/xray/auth.go`

```go
func (s *sessionStore) invalidate(token string) {
    s.mu.Lock()
    delete(s.m, token)
    s.mu.Unlock()
}
```

### 6. Add `GET /v1/auth/me` endpoint

Returns `{"username":"<github-login>"}` for a valid session, `401` otherwise.
Used by the frontend to check session validity on mount.

### 7. Helm / k8s

- `charts/xray/templates/secret-github-oauth.yaml` — empty Secret template
  (user creates manually with `GITHUB_CLIENT_ID` + `GITHUB_CLIENT_SECRET`)
- `charts/xray/values.yaml` — add:
  ```yaml
  github:
    org: YOUR_GITHUB_ORG
    team: ""
    secretName: github-oauth
    publicURL: ""   # e.g. https://xray.example.com
  ```
- `charts/xray/templates/deployment.yaml` — add `envFrom` for the secret,
  and pass flags `--github-org`, `--github-team`, `--public-url`

---

## Frontend

### 8. New `/login` page — `ui/app/login/page.tsx`

Static page (no auth required). Renders a "Sign in with GitHub" button:
```tsx
<a href="/v1/auth/github">Sign in with GitHub</a>
```
Style to match existing dark theme. No JS token logic needed.

### 9. Session check in `XrayLayoutClient`

On mount, call `GET /v1/auth/me`:
- `200` → session valid, render children
- `401` → `router.push('/login')`

No `localStorage`, no token headers — the browser sends the `xray_session` cookie
automatically on every request including the WebSocket upgrade.

### 10. Logout link

Add a logout button/link somewhere in the layout:
```tsx
<a href="/v1/auth/logout">Sign out</a>
```
Server clears the cookie and redirects to `/login`.

---

## GitHub OAuth App setup (one-time, manual)

1. In the `YOUR_GITHUB_ORG` org: **Settings → Developer settings → OAuth Apps → New**
2. Homepage URL: `https://xray.example.com`
3. Callback URL: `https://xray.example.com/v1/auth/github/callback`
4. Scopes: `read:org` (set in the authorize redirect, not in the app settings)
5. Copy Client ID + Secret into the `github-oauth` k8s Secret:
   ```bash
   kubectl create secret generic github-oauth -n xray \
     --from-literal=GITHUB_CLIENT_ID=<id> \
     --from-literal=GITHUB_CLIENT_SECRET=<secret>
   ```

---

## Notes

- No new Go dependencies — use `net/http` for all GitHub API calls
- Cookie `Secure: true` requires HTTPS; fine for the deployed environment
- `sessions.create(login)` stores the GitHub `login` as username — same field used
  later for voice quota tracking
- `/v1/voice` WebSocket is now protected; the browser sends the cookie on the upgrade
  request automatically — no frontend change needed
- OAuth routes are always registered; `--github-client-id` is required
