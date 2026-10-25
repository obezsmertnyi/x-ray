package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/engine"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/index"
)

// search needs no server: it reads the snapshot straight from the directory,
// both when the map is named and when the registry routes to it by meaning.
func TestSearchSnapshots_NoServer(t *testing.T) {
	ctx := context.Background()
	fp := engine.EmbedFingerprint{Model: "stub", Dims: 8, MaxInputChars: 512}
	emb := hashEmbedder{dims: 8}
	entries := []catalogEntry{
		{Name: "alpha", Kind: "Student", Text: "alpha. field engineer. 7 years."},
		{Name: "beta", Kind: "Student", Text: "beta. platform engineer. 12 years."},
	}
	snap, _, err := buildCatalogSnapshot(ctx, entries, "cat", "cat.json", "students of the course", emb, fp, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := engine.WriteSnapshot(filepath.Join(dir, "cat.graph.gob.gz"), snap); err != nil {
		t.Fatal(err)
	}

	for _, project := range []string{"cat", ""} {
		var out bytes.Buffer
		so := searchOpts{kind: "all", project: project}
		if err := searchSnapshots(ctx, &out, emb, fp, engine.SnapshotKeys{}, dir, 1, "beta platform engineer", so, index.Select{}, zerolog.Nop()); err != nil {
			t.Fatalf("project=%q: %v", project, err)
		}
		var got struct {
			RoutedTo []string `json:"routedTo"`
			Results  []struct {
				QN string `json:"qn"`
			} `json:"results"`
		}
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("project=%q: %v\n%s", project, err, out.String())
		}
		if len(got.RoutedTo) != 1 || got.RoutedTo[0] != "cat" || len(got.Results) == 0 {
			t.Fatalf("project=%q: %s", project, out.String())
		}
	}

	var out bytes.Buffer
	so := searchOpts{kind: "all", project: "nosuch"}
	if err := searchSnapshots(ctx, &out, emb, fp, engine.SnapshotKeys{}, dir, 1, "q", so, index.Select{}, zerolog.Nop()); err == nil {
		t.Fatal("unknown -project: want an error")
	}
	if err := searchSnapshots(ctx, &out, emb, fp, engine.SnapshotKeys{}, t.TempDir(), 1, "q", searchOpts{}, index.Select{}, zerolog.Nop()); err == nil {
		t.Fatal("empty dir: want an error")
	}
}
