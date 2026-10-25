package httpguard

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// token is a fixed test value, long enough for MinTokenLen and low-entropy on purpose
// so secret scanners do not read it as a credential.
const token = "test-token-test-token"

func serve(cfg Config, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	h := Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}), cfg)
	req := httptest.NewRequest(method, target, nil)
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestIsLoopback(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8084": true, "localhost:8084": true, "[::1]:8084": true, "127.0.0.2:1": true,
		":8084": false, "0.0.0.0:8084": false, "[::]:8084": false, "10.0.0.5:8084": false, "garbage": false,
	} {
		if got := IsLoopback(addr); got != want {
			t.Errorf("IsLoopback(%q) = %v, want %v", addr, got, want)
		}
	}
}

// DNS rebinding: the page resolves its own name to 127.0.0.1, so the request
// reaches a loopback listener carrying the attacker's hostname — and its own
// origin, which is why the Origin check alone cannot stop it.
func TestLoopbackRefusesForeignHost(t *testing.T) {
	cfg := Config{Addr: "127.0.0.1:8084"}
	rebound := map[string]string{"Host": "evil.example:8084", "Origin": "http://evil.example:8084"}
	if rec := serve(cfg, http.MethodPost, "/update", rebound); rec.Code != http.StatusForbidden {
		t.Fatalf("rebound host: got %d, want 403", rec.Code)
	}
	for _, host := range []string{"localhost:8084", "127.0.0.1:8084", "[::1]:8084", "LOCALHOST:8084"} {
		if rec := serve(cfg, http.MethodGet, "/status", map[string]string{"Host": host}); rec.Code != http.StatusOK {
			t.Errorf("host %s: got %d, want 200", host, rec.Code)
		}
	}
	allowed := Config{Addr: "127.0.0.1:8084", AllowedHosts: []string{"devbox.lan"}}
	if rec := serve(allowed, http.MethodGet, "/status", map[string]string{"Host": "devbox.lan:8084"}); rec.Code != http.StatusOK {
		t.Fatalf("allowed host: got %d, want 200", rec.Code)
	}
}

// Every interface: the Host is whatever name the cluster or proxy uses, so it
// is only checked when the operator lists names.
func TestAllInterfacesSkipsHostCheckUnlessListed(t *testing.T) {
	svc := map[string]string{"Host": "xray-memory.tools.svc:8085"}
	if rec := serve(Config{Addr: ":8085"}, http.MethodGet, "/status", svc); rec.Code != http.StatusOK {
		t.Fatalf("unlisted: got %d, want 200", rec.Code)
	}
	if rec := serve(Config{Addr: ":8085", AllowedHosts: []string{"xray.example.com"}}, http.MethodGet, "/status", svc); rec.Code != http.StatusForbidden {
		t.Fatalf("listed elsewhere: got %d, want 403", rec.Code)
	}
}

func TestOrigin(t *testing.T) {
	cfg := Config{Addr: ":8085", AllowedOrigins: []string{"https://inspector.example/"}}
	cases := []struct {
		origin string
		want   int
	}{
		{"", http.StatusOK},                            // curl, MCP clients, hooks
		{"http://xray:8085", http.StatusOK},            // the UI's own origin
		{"https://inspector.example", http.StatusOK},   // listed
		{"https://evil.example", http.StatusForbidden}, // cross-site
		{"null", http.StatusForbidden},                 // sandboxed frame, file://
		{"http://xray:9999", http.StatusForbidden},     // same host, other port
	}
	for _, c := range cases {
		hdr := map[string]string{"Host": "xray:8085"}
		if c.origin != "" {
			hdr["Origin"] = c.origin
		}
		if rec := serve(cfg, http.MethodPost, "/mcp", hdr); rec.Code != c.want {
			t.Errorf("origin %q: got %d, want %d", c.origin, rec.Code, c.want)
		}
	}
}

func TestToken(t *testing.T) {
	cfg := Config{Addr: ":8085", Token: token}
	cases := []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"none", nil, http.StatusUnauthorized},
		{"wrong", map[string]string{"Authorization": "Bearer nope-nope-nope-nope"}, http.StatusUnauthorized},
		{"prefix of the token", map[string]string{"Authorization": "Bearer " + token[:10]}, http.StatusUnauthorized},
		{"bearer", map[string]string{"Authorization": "Bearer " + token}, http.StatusOK},
		{"bearer, any case", map[string]string{"Authorization": "bearer " + token}, http.StatusOK},
		{"cookie", map[string]string{"Cookie": CookieName + "=" + token}, http.StatusOK},
	}
	for _, c := range cases {
		rec := serve(cfg, http.MethodPost, "/mcp", c.hdr)
		if rec.Code != c.want {
			t.Errorf("%s: got %d, want %d", c.name, rec.Code, c.want)
		}
		if c.want == http.StatusUnauthorized && !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Bearer") {
			t.Errorf("%s: 401 without a Bearer challenge", c.name)
		}
	}
	// A token in the query authenticates nothing but the /ui exchange.
	if rec := serve(cfg, http.MethodGet, "/graph?token="+token, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("query token on /graph: got %d, want 401", rec.Code)
	}
}

func TestUITokenExchange(t *testing.T) {
	cfg := Config{Addr: ":8085", Token: token}
	rec := serve(cfg, http.MethodGet, "/ui?project=alpha&token="+token, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("got %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/ui?project=alpha" {
		t.Fatalf("redirect %q still carries the token or lost the project", loc)
	}
	c := rec.Result().Cookies()
	if len(c) != 1 || c[0].Name != CookieName || c[0].Value != token || !c[0].HttpOnly || c[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie = %+v", c)
	}
	if rec := serve(cfg, http.MethodGet, "/ui?token=wrong-wrong-wrong-wrong", nil); rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("wrong token: got %d with cookies %v", rec.Code, rec.Result().Cookies())
	}
}

// Probes carry no token and hit the pod IP, so /healthz passes every check.
func TestHealthzIsOpen(t *testing.T) {
	cfg := Config{Addr: "127.0.0.1:8085", Token: token, AllowedHosts: []string{"x"}}
	if rec := serve(cfg, http.MethodGet, "/healthz", map[string]string{"Host": "10.1.2.3:8085", "Origin": "https://evil.example"}); rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
}

func TestLoadToken(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	short := filepath.Join(dir, "short")
	_ = os.WriteFile(good, []byte("  "+token+"\n"), 0o600)
	_ = os.WriteFile(short, []byte("abc\n"), 0o600)
	if got, err := LoadToken(good); err != nil || got != token {
		t.Fatalf("LoadToken(good) = %q, %v", got, err)
	}
	if _, err := LoadToken(short); err == nil {
		t.Fatal("a 3-character token was accepted")
	}
	if _, err := LoadToken(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("a missing file was accepted")
	}
}
