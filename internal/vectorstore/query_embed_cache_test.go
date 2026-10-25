package vectorstore

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeQueryBackend is a queryEmbedBackend test double: counts calls, can
// return a fixed error, and can optionally gate/announce each call for
// concurrency tests.
type fakeQueryBackend struct {
	calls atomic.Int64

	mu     sync.Mutex
	byText map[string]int

	vecFn func(text string) []float32
	err   error

	// entered fires (non-blocking) each time EmbedQuery actually runs — lets
	// a test know the leader call has started.
	entered chan struct{}
	// gate, if non-nil, blocks EmbedQuery until closed.
	gate chan struct{}
}

func (f *fakeQueryBackend) EmbedQuery(_ context.Context, text string) ([]float32, error) {
	f.calls.Add(1)
	f.mu.Lock()
	if f.byText == nil {
		f.byText = make(map[string]int)
	}
	f.byText[text]++
	f.mu.Unlock()

	if f.entered != nil {
		select {
		case f.entered <- struct{}{}:
		default:
		}
	}
	if f.gate != nil {
		<-f.gate
	}
	if f.err != nil {
		return nil, f.err
	}
	if f.vecFn != nil {
		return f.vecFn(text), nil
	}
	return []float32{1, 2, 3, 4}, nil
}

func (f *fakeQueryBackend) callCount() int64 { return f.calls.Load() }

// TestQueryEmbedCache_HitReturnsCopyAndOneBackendCall covers the hit path:
// a repeated query is served from the cache (backend called once), the two
// vectors are deep-equal, and mutating a returned slice can never corrupt
// the cached entry — every Get hands out a fresh copy.
func TestQueryEmbedCache_HitReturnsCopyAndOneBackendCall(t *testing.T) {
	backend := &fakeQueryBackend{vecFn: func(string) []float32 { return []float32{1, 2, 3, 4} }}
	cache := newQueryEmbedCache(backend, 0, 0)
	ctx := context.Background()

	v1, err := cache.Get(ctx, "hello")
	if err != nil {
		t.Fatalf("Get #1: %v", err)
	}
	v2, err := cache.Get(ctx, "hello")
	if err != nil {
		t.Fatalf("Get #2: %v", err)
	}
	if got := backend.callCount(); got != 1 {
		t.Fatalf("expected 1 backend call for a repeated query, got %d", got)
	}
	if !reflect.DeepEqual(v1, v2) {
		t.Fatalf("expected equal vectors, got %v vs %v", v1, v2)
	}

	// Mutate the second call's returned slice; a subsequent Get must return
	// the original, unmutated values — proves the cache never hands out a
	// slice backed by its own storage.
	v2[0] = 9999
	v3, err := cache.Get(ctx, "hello")
	if err != nil {
		t.Fatalf("Get #3: %v", err)
	}
	if got := backend.callCount(); got != 1 {
		t.Fatalf("expected still 1 backend call after caller mutation, got %d", got)
	}
	if v3[0] != 1 {
		t.Fatalf("cache entry corrupted by caller-side mutation: v3=%v", v3)
	}

	stats := cache.Stats()
	if stats.Hits != 2 || stats.Misses != 1 || stats.Entries != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

// TestQueryEmbedCache_LRUEvictsOldest exercises eviction at a deliberately
// tiny capacity (injectable via newQueryEmbedCache, unlike the production
// constant).
func TestQueryEmbedCache_LRUEvictsOldest(t *testing.T) {
	backend := &fakeQueryBackend{vecFn: func(text string) []float32 { return []float32{float32(len(text)), 0, 0, 0} }}
	cache := newQueryEmbedCache(backend, 2, 0) // capacity 2 for a cheap eviction test
	ctx := context.Background()

	mustGet := func(text string) {
		t.Helper()
		if _, err := cache.Get(ctx, text); err != nil {
			t.Fatalf("Get(%q): %v", text, err)
		}
	}

	mustGet("a")
	mustGet("b")
	if got := backend.callCount(); got != 2 {
		t.Fatalf("expected 2 backend calls after 2 distinct misses, got %d", got)
	}

	mustGet("b") // touch b so a is the LRU victim
	if got := backend.callCount(); got != 2 {
		t.Fatalf("expected b to be a cache hit, backend calls = %d", got)
	}

	mustGet("c") // over capacity(2): evicts the least-recently-used entry, "a"
	if got := backend.callCount(); got != 3 {
		t.Fatalf("expected 3 backend calls after inserting c, got %d", got)
	}

	mustGet("b") // still resident
	if got := backend.callCount(); got != 3 {
		t.Fatalf("expected b to still be cached, backend calls = %d", got)
	}

	mustGet("a") // evicted earlier — must re-fetch
	if got := backend.callCount(); got != 4 {
		t.Fatalf("expected a to have been evicted (fresh backend call), backend calls = %d", got)
	}

	if stats := cache.Stats(); stats.Entries != 2 {
		t.Fatalf("expected 2 resident entries at capacity 2, got %d", stats.Entries)
	}
}

// TestQueryEmbedCache_SingleflightCollapsesConcurrentMisses proves N
// concurrent identical queries collapse into exactly one backend call. The
// fake backend's gate holds the leader inside EmbedQuery until every other
// goroutine has had a chance to register as a singleflight follower for the
// same key — the same rendezvous-by-sleep idiom
// golang.org/x/sync/singleflight's own TestDoDupSuppress uses, for the
// identical reason (there is no observable "a follower has registered"
// event to synchronize on instead).
func TestQueryEmbedCache_SingleflightCollapsesConcurrentMisses(t *testing.T) {
	backend := &fakeQueryBackend{
		vecFn:   func(string) []float32 { return []float32{1, 2, 3, 4} },
		entered: make(chan struct{}, 1),
		gate:    make(chan struct{}),
	}
	cache := newQueryEmbedCache(backend, 0, 0)
	ctx := context.Background()

	const n = 20
	var startWg, doneWg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, n)

	startWg.Add(n)
	doneWg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer doneWg.Done()
			startWg.Done()
			<-start
			v, err := cache.Get(ctx, "same query")
			if err != nil {
				errs <- err
				return
			}
			if len(v) != 4 {
				errs <- errors.New("unexpected vector length")
			}
		}()
	}
	startWg.Wait() // every goroutine is alive and about to call Get
	close(start)

	select {
	case <-backend.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the (sole) leader to enter the backend call")
	}
	time.Sleep(100 * time.Millisecond) // let the other n-1 goroutines register as followers
	close(backend.gate)

	doneWg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("goroutine error: %v", err)
	}

	if got := backend.callCount(); got != 1 {
		t.Fatalf("expected exactly 1 backend call for %d concurrent identical queries, got %d", n, got)
	}
	if stats := cache.Stats(); stats.Entries != 1 {
		t.Fatalf("expected 1 resident entry, got %d", stats.Entries)
	}
}

// TestQueryEmbedCache_TruncationSharesEntry proves two queries that are
// identical only after maxInputChars truncation key to the same entry — the
// cache must never call the backend twice for them.
func TestQueryEmbedCache_TruncationSharesEntry(t *testing.T) {
	backend := &fakeQueryBackend{vecFn: func(string) []float32 { return []float32{1, 2, 3, 4} }}
	const maxChars = 5
	cache := newQueryEmbedCache(backend, 0, maxChars)
	ctx := context.Background()

	textA := "hello world A" // truncates to "hello"
	textB := "hello world B" // also truncates to "hello", differs only after char 5

	if _, err := cache.Get(ctx, textA); err != nil {
		t.Fatalf("Get(A): %v", err)
	}
	if _, err := cache.Get(ctx, textB); err != nil {
		t.Fatalf("Get(B): %v", err)
	}
	if got := backend.callCount(); got != 1 {
		t.Fatalf("expected 1 backend call for two queries identical after truncation, got %d", got)
	}
	if stats := cache.Stats(); stats.Hits != 1 || stats.Misses != 1 || stats.Entries != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

// TestQueryEmbedCache_BackendErrorNotCached proves a failed embed is never
// cached — a subsequent identical query must retry the backend, not replay
// a cached error (or an empty vector).
func TestQueryEmbedCache_BackendErrorNotCached(t *testing.T) {
	wantErr := errors.New("boom")
	backend := &fakeQueryBackend{err: wantErr}
	cache := newQueryEmbedCache(backend, 0, 0)
	ctx := context.Background()

	if _, err := cache.Get(ctx, "q"); !errors.Is(err, wantErr) {
		t.Fatalf("Get #1: expected backend error, got %v", err)
	}
	if _, err := cache.Get(ctx, "q"); !errors.Is(err, wantErr) {
		t.Fatalf("Get #2: expected backend error again (nothing cached), got %v", err)
	}
	if got := backend.callCount(); got != 2 {
		t.Fatalf("expected 2 backend calls (an error must not be cached), got %d", got)
	}
	if stats := cache.Stats(); stats.Entries != 0 {
		t.Fatalf("expected 0 resident entries after only errors, got %d", stats.Entries)
	}
}
