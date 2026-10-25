// Package codegraph is X-Ray's code graph, run inside your own process.
//
// It parses source code, Markdown and Kubernetes manifests into one graph of
// symbols, call edges, imports and document sections, embeds every node, and
// answers structural questions: where something is implemented, who calls it,
// what a change reaches, how risky it is. It is the engine the xray server and
// "xray graph" command use: Tree-sitter for parsing, an HNSW index and a
// trigram index for search, fused by reciprocal rank.
//
//	emb, _ := codegraph.NewEmbeddings(ctx, "http://localhost:8090", "nomic", 256, 8192)
//	g, err := codegraph.Build(ctx, emb, codegraph.Repo{Label: "myapp", Path: "./myapp"})
//	if err != nil { ... }
//
//	hits, _ := g.SearchGraph(ctx, "where do we retry payments?", xray.Limit(5))
//	impact, _ := g.Impact(ctx, []string{hits.Results[0].QN}, 2, 50)
//
//	g.UpdateGraph(ctx, "./myapp/pay/retry.go") // after the agent edits a file
//	g.Save(ctx, "myapp.graph.gob.gz")          // later: codegraph.Load
//
// Graph implements xray.CodeGraph, the interface the remote xray.Client also
// implements, so code written against it runs in process or against a server.
//
// Parsing needs the Tree-sitter grammars, which are C, so Build, UpdateGraph and
// Reindex need a build with CGO_ENABLED=1 and a C compiler. A build without cgo
// can still Load a snapshot, search it, and serve it; Build returns ErrNoParser.
// Embeddings come from an OpenAI-compatible endpoint you run, or from your own
// Embedder.
package codegraph
