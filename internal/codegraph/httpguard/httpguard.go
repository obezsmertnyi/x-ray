// Package httpguard is the access check in front of every route both servers
// expose: /mcp, /update and /reindex, the UI and the JSON routes it reads,
// and /ws.
//
// Why it exists: both servers used to listen on every interface with no
// credential and no Origin or Host check. On a workstation that let any web
// page the developer opened rebind its own hostname to 127.0.0.1 and drive
// the server from the browser — index a file with update_code_graph, read it
// back with get_graph_node. In a cluster it let any pod read every map,
// including catalog maps that are only encrypted at rest.
//
// Three checks, cheapest first, every one skipped for /healthz so probes need
// nothing:
//
//   - Host. When the listener is loopback, the Host header must name loopback
//     (or an -allowed-hosts entry). This is what defeats DNS rebinding: the
//     rebound page still sends its own hostname.
//   - Origin. A browser request from another origin is refused unless it is
//     listed in -allowed-origins. Non-browser clients send no Origin and pass.
//   - Token. With a token configured, every request needs
//     "Authorization: Bearer <token>", or the cookie the UI gets from
//     /ui?token=<token> — a browser cannot attach a header to a page load or
//     a WebSocket.
package httpguard

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// CookieName carries the token for the browser UI.
const CookieName = "xray_memory_token"

// MinTokenLen refuses a token short enough to guess.
const MinTokenLen = 16

// Config is what the guard enforces. The zero value checks Origin only.
type Config struct {
	// Addr is the listen address. A loopback one ("127.0.0.1:8084",
	// "localhost:8084", "[::1]:8084") turns on the Host check.
	Addr string
	// Token, when set, is required on every request but /healthz.
	Token string
	// AllowedHosts are further Host names (no port) to accept. Any entry turns
	// the Host check on, whatever Addr is.
	AllowedHosts []string
	// AllowedOrigins are further browser origins (scheme://host[:port]) to
	// accept beside the server's own.
	AllowedOrigins []string
}

// IsLoopback reports whether addr binds only the loopback interface. ":8084"
// and "0.0.0.0:8084" are not: they listen on every interface.
func IsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// LoadToken reads a bearer token from path, trimming surrounding whitespace so
// a file written by `openssl rand -hex 32 > token` works as is.
func LoadToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("auth token: %w", err)
	}
	tok := strings.TrimSpace(string(b))
	if len(tok) < MinTokenLen {
		return "", fmt.Errorf("auth token in %s is %d characters; use at least %d (openssl rand -hex 32)", path, len(tok), MinTokenLen)
	}
	return tok, nil
}

// Wrap returns next behind the checks cfg describes.
func Wrap(next http.Handler, cfg Config) http.Handler {
	hosts := map[string]bool{}
	checkHost := IsLoopback(cfg.Addr) || len(cfg.AllowedHosts) > 0
	if IsLoopback(cfg.Addr) {
		hosts["localhost"], hosts["127.0.0.1"], hosts["::1"] = true, true, true
	}
	for _, h := range cfg.AllowedHosts {
		hosts[strings.ToLower(strings.TrimSpace(h))] = true
	}
	origins := map[string]bool{}
	for _, o := range cfg.AllowedOrigins {
		origins[normalizeOrigin(o)] = true
	}
	want := sha256.Sum256([]byte(cfg.Token))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		if checkHost && !hosts[hostname(r.Host)] {
			http.Error(w, "host not allowed", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" && !originAllowed(o, r.Host, origins) {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		if cfg.Token != "" {
			if r.URL.Path == "/ui" && r.URL.Query().Has("token") {
				exchangeToken(w, r, want)
				return
			}
			if !authorized(r, want) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="xray-memory"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// exchangeToken turns /ui?token=… into a cookie and redirects to the same page
// without the token, so it does not stay in the address bar or history.
func exchangeToken(w http.ResponseWriter, r *http.Request, want [32]byte) {
	q := r.URL.Query()
	if !matches(q.Get("token"), want) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    q.Get("token"),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
	})
	q.Del("token")
	u := url.URL{Path: r.URL.Path, RawQuery: q.Encode()}
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}

func authorized(r *http.Request, want [32]byte) bool {
	if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return matches(strings.TrimSpace(h[7:]), want)
	}
	if c, err := r.Cookie(CookieName); err == nil {
		return matches(c.Value, want)
	}
	return false
}

// matches compares hashes so the comparison is constant-time in the token's
// length as well as its content.
func matches(got string, want [32]byte) bool {
	sum := sha256.Sum256([]byte(got))
	return subtle.ConstantTimeCompare(sum[:], want[:]) == 1
}

// originAllowed accepts the server's own origin (the Origin's host:port is the
// Host the request was sent to) or a listed one. "null" — a sandboxed frame or
// a file:// page — parses to no host and is refused.
func originAllowed(origin, host string, allowed map[string]bool) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, host) {
		return true
	}
	return allowed[normalizeOrigin(origin)]
}

func normalizeOrigin(o string) string {
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(o)), "/")
}

// hostname strips the port and IPv6 brackets from a Host header.
func hostname(hostport string) string {
	h := hostport
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		h = host
	}
	return strings.ToLower(strings.Trim(h, "[]"))
}
