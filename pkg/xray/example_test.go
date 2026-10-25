package xray_test

import (
	"context"
	"fmt"
	"log"

	"github.com/triageagent-dev/x-ray/v2/pkg/xray"
)

// Write an item to X-Ray and search for it by meaning.
func Example() {
	ctx := context.Background()
	c, err := xray.Connect(ctx, "http://localhost:8083/mcp", xray.WithToken("shared-secret"))
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()

	if _, err := c.Upsert(ctx, xray.Write{
		Kind: xray.KindStableFact, Scope: xray.ScopePublic, Confidence: "high",
		Title:   "Checkout retries on 503",
		Content: "The payment client retries a 503 three times with jitter.",
		Subject: "checkout",
	}); err != nil {
		log.Fatal(err)
	}

	items, err := c.Search(ctx, "how does payment retry?", xray.WithLimit(5))
	if err != nil {
		log.Fatal(err)
	}
	for _, it := range items {
		fmt.Println(it.Title, it.Origin)
	}
}

// Ask a graph server what a change reaches.
func ExampleClient_Impact() {
	ctx := context.Background()
	c, err := xray.Connect(ctx, "http://127.0.0.1:8084/mcp")
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()

	hits, err := c.SearchGraph(ctx, "where are snapshots encrypted", xray.OnMap("xray"), xray.Limit(3))
	if err != nil {
		log.Fatal(err)
	}
	if len(hits.Results) == 0 {
		return
	}
	imp, err := c.Impact(ctx, []string{hits.Results[0].QN}, 2, 50)
	if err != nil {
		log.Fatal(err)
	}
	for _, n := range imp.Impacted {
		fmt.Println(n.Depth, n.QN)
	}
}

// Refute an item that turned out to be wrong, and everything derived from it.
func ExampleClient_Refute() {
	ctx := context.Background()
	c, err := xray.Connect(ctx, "http://localhost:8083/mcp", xray.WithToken("shared-secret"))
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()

	out, err := c.Refute(ctx, "item-id", "the spikes predate the cache", []string{"event-1"})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(out.Status, out.Cascaded)
}
