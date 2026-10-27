package index

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// batchTestVec returns a 256-dim orthogonal unit vector distinguished by n —
// same convention as TestHandleDeletedCell in the controller package.
// Orthogonal basis vectors give HNSW an exact nearest-neighbor answer even at
// tiny scale, so the search-correctness assertions below aren't at the mercy
// of approximate-search recall noise.
func batchTestVec(n int) []float32 {
	v := make([]float32, 256)
	v[n%256] = 1
	return v
}

func batchTestDoc(id string) IndexEntry {
	return IndexEntry{
		ID: id, Kind: "event",
		Disposition: "new_issue", // SearchByEmbedding requires a disposition
		Timestamp:   time.Now(),
	}
}

// TestMemoryCellStore_DeleteBatchesIndexRebuild is the regression guard for
// a bug where CellStore.Delete used to call
// rebuildIndexLocked on every call, so a retention-janitor sweep of N cells
// was N full HNSW rebuilds on every replica. Delete must only mark the index
// dirty; a single rebuild happens lazily on the first index read after the
// storm (searchByEmbedding / SearchByEmbedding), no matter how many deletes
// preceded it, and a deleted id must never come back from that read.
func TestMemoryCellStore_DeleteBatchesIndexRebuild(t *testing.T) {
	store := newRawStore(nil)
	ctx := context.Background()
	const total = 40
	const deleted = 25

	for i := 0; i < total; i++ {
		store.SeedFromEmbedding(batchTestDoc(fmt.Sprintf("cell-%d", i)), batchTestVec(i))
	}

	if afterSeed := store.rebuildCount(); afterSeed != 0 {
		t.Fatalf("rebuildCount after seed = %d, want 0 (seeding inserts incrementally)", afterSeed)
	}

	for i := 0; i < deleted; i++ {
		if err := store.Delete(ctx, fmt.Sprintf("cell-%d", i)); err != nil {
			t.Fatalf("delete cell-%d: %v", i, err)
		}
	}

	afterDeletes, dirty := store.rebuildCount(), store.anyDirty()
	if afterDeletes != 0 {
		t.Fatalf("rebuildCount after %d deletes = %d, want 0 (deletes must not rebuild)", deleted, afterDeletes)
	}
	if !dirty {
		t.Fatal("store not marked dirty after deletes")
	}
	if got := store.CollectionSize(); got != total-deleted {
		t.Fatalf("CollectionSize = %d, want %d (deletes remove from entries immediately)", got, total-deleted)
	}

	// First read after the storm: exactly one rebuild, and no deleted id
	// surfaces even though the pre-rebuild index still held their nodes.
	res := store.searchAll(batchTestVec(total-1), total, "")
	if afterFirstRead := store.rebuildCount(); afterFirstRead != 1 {
		t.Fatalf("rebuildCount after first post-delete read = %d, want 1 (exactly one rebuild for the whole storm)", afterFirstRead)
	}
	if len(res) != total-deleted {
		t.Fatalf("search returned %d results, want %d survivors", len(res), total-deleted)
	}
	for _, r := range res {
		for i := 0; i < deleted; i++ {
			if r.Document.ID == fmt.Sprintf("cell-%d", i) {
				t.Fatalf("search returned deleted id %s", r.Document.ID)
			}
		}
	}

	// A second read with no intervening delete must NOT trigger another rebuild.
	if _, err := store.SearchByEmbedding(ctx, "", batchTestVec(total-1), time.Time{}, 0, ""); err != nil {
		t.Fatalf("SearchByEmbedding: %v", err)
	}
	if afterSecondRead := store.rebuildCount(); afterSecondRead != 1 {
		t.Fatalf("rebuildCount after second read = %d, want 1 (no further rebuild without a new delete)", afterSecondRead)
	}
}

// TestMemoryCellStore_ConcurrentDeleteAndSearch_NoRace exercises Delete
// racing both index read paths concurrently — run with `go test -race`. It
// makes no ordering assertions (outcomes are inherently racy); the point is
// that the dirty-flag/rebuild-once scheme has no unsynchronized access to
// s.entries/s.index/s.dirty/s.rebuildCount.
func TestMemoryCellStore_ConcurrentDeleteAndSearch_NoRace(t *testing.T) {
	store := newRawStore(nil)
	ctx := context.Background()
	const total = 50

	for i := 0; i < total; i++ {
		store.SeedFromEmbedding(batchTestDoc(fmt.Sprintf("race-cell-%d", i)), batchTestVec(i))
	}

	var wg sync.WaitGroup
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = store.Delete(ctx, fmt.Sprintf("race-cell-%d", i))
		}(i)
	}
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = store.SearchByEmbedding(ctx, "", batchTestVec(i%total), time.Time{}, 0, "")
			_ = store.searchAll(batchTestVec(i%total), 5, "")
		}(i)
	}
	wg.Wait()
}
