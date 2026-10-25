// Package xray is the Go SDK for X-Ray, an agentic memory substrate for local
// and Kubernetes-native use.
//
// A Client talks to a running X-Ray over MCP (streamable HTTP). It covers both
// kinds of memory the server holds:
//
//   - Authored memory: typed items that agents and people write. Search, List,
//     Fetch, Upsert, Delete, Refute, Expire, Supersede, Consolidate and Context.
//   - Derived memory: a graph of code, documentation and manifests. SearchGraph,
//     SearchGraphDocs, Node, Impact, Risk, GraphStats and UpdateGraph, and the
//     agent notes Remember, Recall and Forget.
//
// Connect to the memory server (default port 8083, or /mcp on the main port
// when OAuth is on) or to a graph server (xray graph serve, port 8084, or
// serve-snapshots, port 8085):
//
//	c, err := xray.Connect(ctx, "http://localhost:8083/mcp", xray.WithToken(token))
//	if err != nil { ... }
//	defer c.Close()
//
//	res, err := c.Upsert(ctx, xray.Write{
//		Kind: xray.KindStableFact, Scope: xray.ScopePublic, Confidence: "high",
//		Title: "Checkout retries on 503", Content: "Three times, with jitter.",
//	})
//	items, err := c.Search(ctx, "payment retries", xray.WithLimit(5))
//
// A server answers only the tools it has: the memory tools on the memory
// server, the graph tools on a graph server, and both on a memory server that
// loads maps. Call reports the rest as an error.
//
// Which credential the server sees decides how a write is classed. A write made
// with an OAuth token that names a person is recorded as human; one made with
// the shared secret is recorded as derived. The SDK cannot set it.
package xray
