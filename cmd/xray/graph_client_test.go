package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestSearchURLOmitsDefaults(t *testing.T) {
	got := searchURL("http://localhost:8084/", "who calls Update", searchOpts{kind: "all"})
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if u.Path != "/search" {
		t.Fatalf("path = %q, want /search", u.Path)
	}
	if q := u.Query(); len(q) != 1 || q.Get("q") != "who calls Update" {
		t.Fatalf("query = %v, want only q", q)
	}
}

func TestSearchURLCarriesEveryOption(t *testing.T) {
	got := searchURL("http://h", "q", searchOpts{
		kind: "docs", limit: 7, project: "academy", attr: "team=core", order: "age:desc", gap: true,
	})
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"q": "q", "kind": "docs", "limit": "7", "project": "academy",
		"attr": "team=core", "order": "age:desc", "cutoff": "gap",
	}
	q := u.Query()
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
}

// With -server the subcommand joins the words after the flags into one query and
// sends the bearer token, so it works unquoted against a server behind
// -auth-token-file.
func TestRunClientSearch(t *testing.T) {
	var gotQ, gotKind, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQ, gotKind = r.URL.Query().Get("q"), r.URL.Query().Get("kind")
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer srv.Close()
	t.Setenv(envAuthToken, "test-token-test-token")

	if err := runSearchTo(context.Background(), io.Discard, io.Discard, []string{"-server", srv.URL, "-kind", "code", "where", "is", "flush"}); err != nil {
		t.Fatal(err)
	}
	if gotQ != "where is flush" || gotKind != "code" {
		t.Fatalf("q=%q kind=%q", gotQ, gotKind)
	}
	if gotAuth != "Bearer test-token-test-token" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if err := runSearchTo(context.Background(), io.Discard, io.Discard, []string{"-server", srv.URL}); err == nil {
		t.Fatal("empty query: want a usage error")
	}
}
