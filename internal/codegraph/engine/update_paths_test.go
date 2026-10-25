//go:build cgo

package engine

// Path resolution for update_code_graph.
//
// The bug these cover: a relative path used to be resolved with filepath.Abs,
// i.e. against the SERVER process's working directory. Under launchd that is
// "/", so every relative path landed under no repo, was dropped at debug level,
// and the caller was told `changed: 0` — which is also what a genuinely
// up-to-date file returns. A silent no-op wearing the costume of success.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// registryWithRepo builds a one-project writable registry over a fixture repo
// and returns it with the repo's root on disk.
func registryWithRepo(t *testing.T, label string) (*Registry, string) {
	t.Helper()
	emb := &stubEmbedder{dims: 64}
	fp := EmbedFingerprint{Model: "stub", Dims: 64, MaxInputChars: 8192}
	repoDir := writeFixtureRepo(t, label)

	ctx := context.Background()
	ixr, err := NewIndexer(ctx, []string{label + "=" + repoDir}, emb, testLogger(), false)
	if err != nil {
		t.Fatalf("NewIndexer: %v", err)
	}
	snap, err := ixr.SnapshotFor(ctx, label, fp, emb)
	if err != nil {
		t.Fatalf("SnapshotFor: %v", err)
	}
	snapPath := filepath.Join(t.TempDir(), label+".graph.gob.gz")
	if err := WriteSnapshot(snapPath, snap); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}
	reg := NewRegistry(emb, fp, 4, time.Hour, true, testLogger())
	if err := reg.AddSnapshot(snapPath); err != nil {
		t.Fatalf("AddSnapshot: %v", err)
	}
	return reg, repoDir
}

// A repo-relative path must be indexed no matter what the process CWD is.
func TestUpdateByPathsResolvesRepoRelative(t *testing.T) {
	ctx := context.Background()
	reg, repoDir := registryWithRepo(t, "alpha")

	// Move the process CWD somewhere that is NOT the repo, which is the whole
	// point: resolution must not depend on where the server happens to run.
	t.Chdir(t.TempDir())

	mustWrite(t, filepath.Join(repoDir, "extra.go"),
		"package main\n\nfunc ExtraThing() int { return 7 }\n")

	changed, _, _, unowned, err := reg.UpdateByPaths(ctx, []string{"extra.go"})
	if err != nil {
		t.Fatalf("UpdateByPaths: %v", err)
	}
	if changed != 1 {
		t.Fatalf("relative path must be indexed: changed=%d (want 1)", changed)
	}
	if len(unowned) != 0 {
		t.Fatalf("relative path must not be reported unowned: %v", unowned)
	}
}

// A path that resolves nowhere is reported, not silently dropped.
func TestUpdateByPathsReportsUnowned(t *testing.T) {
	ctx := context.Background()
	reg, _ := registryWithRepo(t, "alpha")

	outside := filepath.Join(t.TempDir(), "elsewhere.go")
	mustWrite(t, outside, "package main\n")

	changed, _, _, unowned, err := reg.UpdateByPaths(ctx,
		[]string{"no/such/file.go", outside})
	if err != nil {
		t.Fatalf("UpdateByPaths: %v", err)
	}
	if changed != 0 {
		t.Fatalf("changed=%d (want 0)", changed)
	}
	if len(unowned) != 2 {
		t.Fatalf("both paths must be reported unowned, got %v", unowned)
	}
}

// The same relative path under two served repos is ambiguous. Guessing would
// silently index the wrong repo, so it is reported instead.
func TestUpdateByPathsRefusesAmbiguousRelative(t *testing.T) {
	ctx := context.Background()
	reg, repoA := registryWithRepo(t, "alpha")

	// Add a second project whose repo also has a file at the same relative path.
	emb := &stubEmbedder{dims: 64}
	fp := EmbedFingerprint{Model: "stub", Dims: 64, MaxInputChars: 8192}
	repoB := writeFixtureRepo(t, "beta")
	ixr, err := NewIndexer(ctx, []string{"beta=" + repoB}, emb, testLogger(), false)
	if err != nil {
		t.Fatalf("NewIndexer: %v", err)
	}
	snap, err := ixr.SnapshotFor(ctx, "beta", fp, emb)
	if err != nil {
		t.Fatalf("SnapshotFor: %v", err)
	}
	snapPath := filepath.Join(t.TempDir(), "beta.graph.gob.gz")
	if err := WriteSnapshot(snapPath, snap); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}
	if err := reg.AddSnapshot(snapPath); err != nil {
		t.Fatalf("AddSnapshot: %v", err)
	}

	body := "package main\n\nfunc Shared() int { return 1 }\n"
	mustWrite(t, filepath.Join(repoA, "shared.go"), body)
	mustWrite(t, filepath.Join(repoB, "shared.go"), body)

	changed, _, _, unowned, err := reg.UpdateByPaths(ctx, []string{"shared.go"})
	if err != nil {
		t.Fatalf("UpdateByPaths: %v", err)
	}
	if changed != 0 {
		t.Fatalf("ambiguous path must not be indexed: changed=%d", changed)
	}
	if len(unowned) != 1 || unowned[0] != "shared.go" {
		t.Fatalf("ambiguous path must be reported: %v", unowned)
	}
}

// An absolute path still works, including for a file that no longer exists —
// which is how a deletion is recorded.
func TestUpdateByPathsAbsoluteDeletion(t *testing.T) {
	ctx := context.Background()
	reg, repoDir := registryWithRepo(t, "alpha")

	gone := filepath.Join(repoDir, "extra.go")
	mustWrite(t, gone, "package main\n\nfunc ExtraThing() int { return 7 }\n")
	if changed, _, _, _, err := reg.UpdateByPaths(ctx, []string{gone}); err != nil || changed != 1 {
		t.Fatalf("seed update: changed=%d err=%v", changed, err)
	}
	if err := os.Remove(gone); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	_, removed, _, unowned, err := reg.UpdateByPaths(ctx, []string{gone})
	if err != nil {
		t.Fatalf("UpdateByPaths: %v", err)
	}
	if removed != 1 {
		t.Fatalf("deletion must drop the file's nodes: removed=%d", removed)
	}
	if len(unowned) != 0 {
		t.Fatalf("absolute path of a deleted file must still resolve: %v", unowned)
	}
}
