package compose

import (
	"strings"
	"testing"
)

func TestParse_ServicesEnvDependsOnBuild(t *testing.T) {
	src := []byte(`name: demo
services:
  checkout:
    build:
      context: ./
      dockerfile: ${CHECKOUT_DOCKERFILE}
    environment:
      - CART_ADDR
      - DB_CONNECTION_STRING=postgres://user:hunter2@${POSTGRES_HOST}/db
      - DB_PASSWORD=hunter2
    depends_on:
      cart:
        condition: service_started
  cart:
    image: cart
networks:
  default:
`)
	res := Parse("compose.yaml", src)
	if len(res.Nodes) != 2 || res.Nodes[0].Name != "checkout" || res.Nodes[1].Name != "cart" {
		t.Fatalf("nodes = %+v, want checkout and cart (networks: is not a service)", res.Nodes)
	}
	got := map[string]string{}
	for _, e := range res.Edges {
		got[e.Target] = e.Extra[ExtraValue]
	}
	for _, want := range []string{"env:CART_ADDR", "env:DB_CONNECTION_STRING", "env:DB_PASSWORD", "deploy:cart", "${CHECKOUT_DOCKERFILE}"} {
		if _, ok := got[want]; !ok {
			t.Fatalf("edges %v missing %s", got, want)
		}
	}
	if _, ok := got["deploy:condition"]; ok {
		t.Fatalf("a depends_on option was taken for a service: %v", got)
	}
	if v := got["env:DB_CONNECTION_STRING"]; strings.Contains(v, "hunter2") || !strings.Contains(v, "${POSTGRES_HOST}") {
		t.Fatalf("connection string stored as %q: credentials must go, the host must stay", v)
	}
	if v := got["env:DB_PASSWORD"]; v != "<redacted>" {
		t.Fatalf("DB_PASSWORD stored as %q", v)
	}
	if strings.Contains(res.Nodes[0].Body, "hunter2") {
		t.Fatalf("body leaks a credential: %s", res.Nodes[0].Body)
	}
}

func TestParseDotEnv_ValuesRedactedAndCommentsCut(t *testing.T) {
	res := ParseDotEnv(".env", []byte("CART_ADDR=cart:${CART_PORT}\nPOSTGRES_PASSWORD=changeit\nX=astronomy   # a comment\n# CART_ADDR=nope\n"))
	got := map[string]string{}
	for _, n := range res.Nodes {
		got[n.Name] = n.Body
	}
	want := map[string]string{"CART_ADDR": "cart:${CART_PORT}", "POSTGRES_PASSWORD": "<redacted>", "X": "astronomy"}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
	if len(got) != 3 {
		t.Fatalf("got %v, a commented-out key must not be a node", got)
	}
}
