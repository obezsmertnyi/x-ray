package observability

import (
	"context"
	"testing"
)

func TestBearerToken(t *testing.T) {
	var b BearerToken
	if md, _ := b.GetRequestMetadata(context.Background()); len(md) != 0 {
		t.Fatalf("no token set: want no metadata, got %v", md)
	}
	b.Set(" abc\n")
	md, err := b.GetRequestMetadata(context.Background())
	if err != nil || md["authorization"] != "Bearer abc" {
		t.Fatalf("got %v, %v", md, err)
	}
	b.Set("def")
	if md, _ := b.GetRequestMetadata(context.Background()); md["authorization"] != "Bearer def" {
		t.Fatalf("token did not follow Set: %v", md)
	}
	if b.RequireTransportSecurity() {
		t.Fatal("cluster-local collectors speak plaintext gRPC")
	}
}
