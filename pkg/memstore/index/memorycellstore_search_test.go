package index

import (
	"context"
	"testing"
	"time"
)

// seedCell inserts an issue/event-payload cell into the store with a
// pre-computed embedding (the same path the reconciler's restart seed uses).
func seedCell(t *testing.T, store *CellStore, e sampleEvent, ts time.Time) {
	t.Helper()
	doc := IndexEntry{
		ID:          e.source + "-cell",
		Kind:        "event",
		Subject:     e.service,
		Content:     e.message,
		Status:      "active",
		Source:      e.source,
		Service:     e.service,
		Disposition: e.disposition,
		GroupID:     e.groupID,
		Timestamp:   ts,
	}
	store.SeedFromEmbedding(doc, embed(t, e.source, e.message))
}

// seedCellPartition is seedCell plus an explicit Partition tag and a caller-chosen
// id suffix (so two partitions can seed a cell for the identical source/service
// without an ID collision).
func seedCellPartition(t *testing.T, store *CellStore, e sampleEvent, ts time.Time, partition, idSuffix string) {
	t.Helper()
	doc := IndexEntry{
		ID:          e.source + idSuffix + "-cell",
		Kind:        "event",
		Subject:     e.service,
		Content:     e.message,
		Status:      "active",
		Source:      e.source,
		Service:     e.service,
		Disposition: e.disposition,
		GroupID:     e.groupID,
		Timestamp:   ts,
		Partition:   partition,
	}
	store.SeedFromEmbedding(doc, embed(t, e.source, e.message))
}

// TestCellSearchByEmbedding_CrossEnvGuard mirrors the MemoryEventStore
// regression: a DEV billing-service decision must NOT be returned for a PROD
// billing-service event. Source, not service, is the guard key.
func TestCellSearchByEmbedding_CrossEnvGuard(t *testing.T) {
	store := newRawStore(nil)
	ctx := context.Background()
	now := time.Now()
	since := now.Add(-2 * time.Hour)

	dev := find(t, "app/dev/dev/billing-service")
	prod := find(t, "app/prod/prod/billing-service")

	seedCell(t, store, dev, now.Add(-2*time.Minute))
	query := embed(t, prod.source, prod.message)

	// Sanity: unguarded, the dev entry IS returned for the prod event.
	unguarded, err := store.SearchByEmbedding(ctx, "", query, since, 0.85, "")
	if err != nil {
		t.Fatal(err)
	}
	if unguarded == nil {
		t.Fatal("sanity: real dev/prod messages did not collide above 0.85 — expected a hit")
	}

	// With the prod source guard, the dev entry must be rejected.
	guarded, err := store.SearchByEmbedding(ctx, "", query, since, 0.85, prod.source)
	if err != nil {
		t.Fatal(err)
	}
	if guarded != nil {
		t.Fatalf("cross-env guard FAILED: dev billing-service %q returned for a PROD event",
			guarded.Document.Disposition)
	}
}

// TestCellSearchByEmbedding_SameSourceHits: a same-source cached decision is
// still returned — the guard doesn't break the legitimate flap-dedup case.
func TestCellSearchByEmbedding_SameSourceHits(t *testing.T) {
	store := newRawStore(nil)
	ctx := context.Background()
	now := time.Now()
	since := now.Add(-2 * time.Hour)

	prod := find(t, "app/prod/prod/billing-service")
	seedCell(t, store, prod, now.Add(-3*time.Minute))

	query := embed(t, prod.source, prod.message)
	hit, err := store.SearchByEmbedding(ctx, "", query, since, 0.85, prod.source)
	if err != nil {
		t.Fatal(err)
	}
	if hit == nil {
		t.Fatal("same-source cache hit expected, got miss — guard too aggressive")
	}
	if hit.Document.Source != prod.source {
		t.Fatalf("unexpected hit source=%s", hit.Document.Source)
	}
}

// TestCellSearchByEmbedding_RequiresDisposition: a cell with no disposition
// (still in-flight, e.g. a memory-item cell) is never returned as a cache hit.
func TestCellSearchByEmbedding_RequiresDisposition(t *testing.T) {
	store := newRawStore(nil)
	ctx := context.Background()
	now := time.Now()
	since := now.Add(-2 * time.Hour)

	prod := find(t, "app/prod/prod/billing-service")
	noDisp := prod
	noDisp.disposition = "" // in-flight
	seedCell(t, store, noDisp, now.Add(-2*time.Minute))

	query := embed(t, prod.source, prod.message)
	hit, err := store.SearchByEmbedding(ctx, "", query, since, 0.85, "")
	if err != nil {
		t.Fatal(err)
	}
	if hit != nil {
		t.Fatalf("disposition-less cell returned as cache hit: %+v", hit.Document)
	}
}

// TestCellSearchByEmbedding_SinceWindow: a cell older than the `since` window
// is excluded.
func TestCellSearchByEmbedding_SinceWindow(t *testing.T) {
	store := newRawStore(nil)
	ctx := context.Background()
	now := time.Now()

	prod := find(t, "app/prod/prod/billing-service")
	seedCell(t, store, prod, now.Add(-3*time.Hour)) // outside a 2h window

	query := embed(t, prod.source, prod.message)
	hit, err := store.SearchByEmbedding(ctx, "", query, now.Add(-2*time.Hour), 0.85, prod.source)
	if err != nil {
		t.Fatal(err)
	}
	if hit != nil {
		t.Fatalf("expired cell returned inside `since` window: %+v", hit.Document)
	}
}

// TestCellGetRecentBySubject filters by service, disposition, and time window,
// and returns oldest-first, capped at topN.
func TestCellGetRecentBySubject(t *testing.T) {
	store := newRawStore(nil)
	ctx := context.Background()
	now := time.Now()

	prod := find(t, "app/prod/prod/billing-service")
	df := find(t, "app/ops/report-service")

	// Three prod billing-service cells at distinct times, plus one data-forge cell.
	older := prod
	older.source = prod.source + "#1"
	newer := prod
	newer.source = prod.source + "#2"
	seedCell(t, store, older, now.Add(-30*time.Minute))
	seedCell(t, store, prod, now.Add(-20*time.Minute))
	seedCell(t, store, newer, now.Add(-10*time.Minute))
	seedCell(t, store, df, now.Add(-15*time.Minute))

	// In-flight prod cell (no disposition) must be excluded.
	inflight := prod
	inflight.source = prod.source + "#inflight"
	inflight.disposition = ""
	seedCell(t, store, inflight, now.Add(-5*time.Minute))

	docs, err := store.GetRecentBySubject(ctx, "", "billing-service", 10, now.Add(-1*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 3 {
		t.Fatalf("expected 3 disposition-bearing billing-service cells, got %d: %+v", len(docs), docs)
	}
	// Oldest-first.
	for i := 1; i < len(docs); i++ {
		if docs[i].Timestamp.Before(docs[i-1].Timestamp) {
			t.Fatalf("results not oldest-first: %v before %v", docs[i].Timestamp, docs[i-1].Timestamp)
		}
	}
	for _, d := range docs {
		if d.Service != "billing-service" {
			t.Fatalf("wrong service leaked in: %s", d.Service)
		}
		if d.Disposition == "" {
			t.Fatalf("in-flight cell leaked in: %+v", d)
		}
	}

	// topN cap keeps the newest N.
	capped, err := store.GetRecentBySubject(ctx, "", "billing-service", 2, now.Add(-1*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(capped) != 2 {
		t.Fatalf("expected topN=2 cap, got %d", len(capped))
	}
	// The newest of the three (`newer`, -10m) must survive the cap.
	foundNewest := false
	for _, d := range capped {
		if d.ID == newer.source+"-cell" {
			foundNewest = true
		}
	}
	if !foundNewest {
		t.Fatalf("topN cap dropped the newest cell; got %+v", capped)
	}
}

// TestCellSearchByEmbedding_PartitionIsolation proves the partition gate holds with
// the source guard disabled (source == ""), the case a templated platform
// message across two partitions' deployments produces (SearchByEmbedding's own
// doc comment): the dev/prod billing-service messages collide above 0.85
// (TestRealMessages_TemplatedCollision) — here one is tagged partition-b, the
// other partition-a, both real candidates for either query. Only the partition filter
// keeps each query on its own partition's cell.
func TestCellSearchByEmbedding_PartitionIsolation(t *testing.T) {
	store := newRawStore(nil)
	ctx := context.Background()
	now := time.Now()
	since := now.Add(-2 * time.Hour)

	dev := find(t, "app/dev/dev/billing-service")
	prod := find(t, "app/prod/prod/billing-service")
	seedCellPartition(t, store, dev, now.Add(-2*time.Minute), "partition-b", "-partition-b")
	seedCellPartition(t, store, prod, now.Add(-1*time.Minute), "partition-a", "-partition-a")

	query := embed(t, prod.source, prod.message)

	partitionBHit, err := store.SearchByEmbedding(ctx, "partition-b", query, since, 0.85, "")
	if err != nil {
		t.Fatal(err)
	}
	if partitionBHit == nil || partitionBHit.Document.Partition != "partition-b" {
		t.Fatalf("partition-b query = %+v, want the partition-b-tagged (dev) cell", partitionBHit)
	}

	partitionAHit, err := store.SearchByEmbedding(ctx, "partition-a", query, since, 0.85, "")
	if err != nil {
		t.Fatal(err)
	}
	if partitionAHit == nil || partitionAHit.Document.Partition != "partition-a" {
		t.Fatalf("partition-a query = %+v, want the partition-a-tagged (prod) cell", partitionAHit)
	}
}

// TestGetRecentBySubject_PartitionIsolation proves two partitions running a
// same-named service never share GetRecentBySubject history: each partition's
// query returns only its own cells, even though both cells match on Service.
func TestGetRecentBySubject_PartitionIsolation(t *testing.T) {
	store := newRawStore(nil)
	ctx := context.Background()
	now := time.Now()

	prod := find(t, "app/prod/prod/billing-service")
	seedCellPartition(t, store, prod, now.Add(-10*time.Minute), "partition-b", "-partition-b")
	seedCellPartition(t, store, prod, now.Add(-5*time.Minute), "partition-a", "-partition-a")

	partitionBDocs, err := store.GetRecentBySubject(ctx, "partition-b", "billing-service", 10, now.Add(-1*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(partitionBDocs) != 1 || partitionBDocs[0].Partition != "partition-b" {
		t.Fatalf("partition-b query = %+v, want exactly the partition-b-tagged cell", partitionBDocs)
	}

	partitionADocs, err := store.GetRecentBySubject(ctx, "partition-a", "billing-service", 10, now.Add(-1*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(partitionADocs) != 1 || partitionADocs[0].Partition != "partition-a" {
		t.Fatalf("partition-a query = %+v, want exactly the partition-a-tagged cell", partitionADocs)
	}
}

// TestGetRecentBySubject_EmptyPartitionCellsVisibleToBoth proves an untagged
// (instance-global) cell — written before partition tagging existed, or on a
// deployment with no partition registry — stays visible to every partition's
// query, alongside that partition's own tagged cells only.
func TestGetRecentBySubject_EmptyPartitionCellsVisibleToBoth(t *testing.T) {
	store := newRawStore(nil)
	ctx := context.Background()
	now := time.Now()

	prod := find(t, "app/prod/prod/billing-service")
	seedCellPartition(t, store, prod, now.Add(-15*time.Minute), "", "-global")
	seedCellPartition(t, store, prod, now.Add(-10*time.Minute), "partition-b", "-partition-b")
	seedCellPartition(t, store, prod, now.Add(-5*time.Minute), "partition-a", "-partition-a")

	partitionBDocs, err := store.GetRecentBySubject(ctx, "partition-b", "billing-service", 10, now.Add(-1*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(partitionBDocs) != 2 {
		t.Fatalf("partition-b query = %+v, want the global cell plus its own", partitionBDocs)
	}
	for _, d := range partitionBDocs {
		if d.Partition != "" && d.Partition != "partition-b" {
			t.Fatalf("partition-b query leaked a foreign-partition cell: %+v", d)
		}
	}

	partitionADocs, err := store.GetRecentBySubject(ctx, "partition-a", "billing-service", 10, now.Add(-1*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(partitionADocs) != 2 {
		t.Fatalf("partition-a query = %+v, want the global cell plus its own", partitionADocs)
	}
	for _, d := range partitionADocs {
		if d.Partition != "" && d.Partition != "partition-a" {
			t.Fatalf("partition-a query leaked a foreign-partition cell: %+v", d)
		}
	}
}
