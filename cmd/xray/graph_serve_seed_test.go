//go:build cgo

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/engine"
)

type countingHashEmbedder struct {
	hashEmbedder
	embedded int
}

func (c *countingHashEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	c.embedded += len(texts)
	return c.hashEmbedder.EmbedBatch(ctx, texts)
}

// A snapshot extracted under older facts must not be served as-is: the server
// re-parses the repo (so a fact the old extractors never emitted appears) and
// reuses the snapshot's vectors (so unchanged nodes embed nothing).
func TestSeedFromSnapshot_StaleFactsReparsesWithCachedVectors(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "a.go"), []byte("package a\n\nfunc A() int { return 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fp := engine.EmbedFingerprint{Model: "hash", Dims: 8, MaxInputChars: 8192}
	emb := &countingHashEmbedder{hashEmbedder: hashEmbedder{dims: 8}}
	spec := "r=" + repo
	ixr, err := engine.NewIndexer(ctx, []string{spec}, emb, zerolog.Nop(), false)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := ixr.SnapshotFor(ctx, "r", fp, emb)
	if err != nil {
		t.Fatal(err)
	}
	snap.Header.Facts = 0 // as written by a binary from before the facts number
	dir := t.TempDir()
	if err := engine.WriteSnapshot(filepath.Join(dir, "r.graph.gob.gz"), snap); err != nil {
		t.Fatal(err)
	}
	// A fact the snapshot does not hold: a Dockerfile added since.
	if err := os.WriteFile(filepath.Join(repo, "Dockerfile"), []byte("FROM x\nCOPY . .\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	emb.embedded = 0
	seeded, _, err := seedFromSnapshot(ctx, []string{spec}, dir, false, fp, emb, engine.SnapshotKeys{}, zerolog.Nop())
	if err != nil || seeded == nil {
		t.Fatalf("seed: %v, %v", seeded, err)
	}
	if seeded.Graph().Stats().ByKind["Image"] != 1 {
		t.Fatalf("stale snapshot served as-is: the re-parsed Image node is missing")
	}
	if emb.embedded != 1 {
		t.Fatalf("embedded %d texts, want only the new Image node", emb.embedded)
	}
}
