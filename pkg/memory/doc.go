// Package memory is X-Ray's memory store, run inside your own process.
//
// It is the same store the X-Ray server uses: typed items with a scope and a
// subject, a keyword index, an HNSW index over embeddings when you give it an
// embedding endpoint, near-duplicate merging on write, recall shaping, and
// provenance, where an item a person wrote is never replaced by one a model
// wrote. There is no server, no Kubernetes and no network except the embedding
// endpoint you choose.
//
//	store, err := memory.New(ctx,
//		memory.WithEmbeddings("http://localhost:8090", "nomic", 256),
//		memory.WithFile("agent-memory.json"),
//	)
//	if err != nil { ... }
//	defer store.Close()
//
//	store.Upsert(ctx, xray.Write{
//		Kind: xray.KindStableFact, Scope: xray.ScopePublic, Confidence: "high",
//		Title: "Checkout retries on 503", Content: "Three times, with jitter.",
//	})
//	items, _ := store.Search(ctx, "payment retries", xray.WithLimit(5))
//
// Store implements xray.Memory, the interface the remote xray.Client also
// implements. An agent written against it runs in process during development
// and against a shared X-Ray in production without a change.
//
// With no embedding endpoint search is by keyword and trigram only. With
// WithFile the store loads the file on start and writes it after every change,
// so memory survives a restart; without it memory lives until the process ends.
package memory
