package vectorstore

import (
	"container/list"
	"context"
	"sync"
	"sync/atomic"

	"golang.org/x/sync/singleflight"
)

// defaultQueryEmbedCacheCapacity bounds the in-process query-embedding LRU:
// 4096 entries ≈ 4 MB at 256-dim float32 (4096 × 256 × 4 bytes). A compiled
// constant: the cache needs no tuning.
const defaultQueryEmbedCacheCapacity = 4096

// queryEmbedBackend is the minimal embedding surface queryEmbedCache needs.
// Satisfied by *Embedder. Kept narrow so the cache is testable with a fake
// that doesn't need to implement EmbedDocument/EmbedBatch.
type queryEmbedBackend interface {
	EmbedQuery(ctx context.Context, text string) ([]float32, error)
}

// QueryEmbedCacheStats is a point-in-time counter readout for observability.
type QueryEmbedCacheStats struct {
	Hits    int64
	Misses  int64
	Entries int
}

// queryEmbedCache is an in-process, exact-match LRU cache for QUERY
// embeddings ONLY. It must never be used to wrap EmbedDocument/EmbedBatch —
// documents are embedded once by design (at index time), so caching them
// would only waste memory. Query embeddings are different: the same search
// text is often repeated (a user re-running a search, a dashboard polling
// the same concern's RAG lookup), and each repeat otherwise pays the
// embedder's full round trip again (~17ms for the llama.cpp sidecar).
//
// The wrapped backend's model + output dims are fixed for the backend's
// lifetime, so a cached vector never goes stale within this process — there
// is no TTL, only LRU eviction at capacity.
//
// Keys are the query text AFTER the same maxInputChars truncation
// Embedder.embed applies, so two queries that are identical only after
// truncation still share one entry (see truncateKey).
//
// Concurrent lookups for the same not-yet-cached key collapse into one
// backend call via golang.org/x/sync/singleflight, keyed identically to the
// cache itself.
type queryEmbedCache struct {
	backend  queryEmbedBackend
	capacity int
	maxChars int // mirrors Embedder.maxInputChars; <=0 disables key truncation

	group singleflight.Group

	mu    sync.Mutex
	ll    *list.List               // front = most recently used
	items map[string]*list.Element // key -> *list.Element holding *queryCacheEntry

	hits   atomic.Int64
	misses atomic.Int64
}

// queryCacheEntry is the list.Element payload.
type queryCacheEntry struct {
	key string
	vec []float32
}

// newQueryEmbedCache builds a query-embedding cache in front of backend.
// capacity <= 0 falls back to defaultQueryEmbedCacheCapacity — production
// call sites pass 0 (or the constant); tests pass a small explicit value to
// exercise eviction cheaply. maxChars <= 0 disables key truncation (every
// key is used verbatim).
func newQueryEmbedCache(backend queryEmbedBackend, capacity, maxChars int) *queryEmbedCache {
	if capacity <= 0 {
		capacity = defaultQueryEmbedCacheCapacity
	}
	return &queryEmbedCache{
		backend:  backend,
		capacity: capacity,
		maxChars: maxChars,
		ll:       list.New(),
		items:    make(map[string]*list.Element, capacity),
	}
}

// truncateKey mirrors Embedder.embed's maxInputChars truncation so two
// queries identical after truncation key to the same cache entry.
func (c *queryEmbedCache) truncateKey(text string) string {
	if c.maxChars > 0 && len(text) > c.maxChars {
		return text[:c.maxChars]
	}
	return text
}

// Get returns the embedding for text, serving it from the LRU on a hit or
// computing + inserting it via the backend on a miss. The returned slice is
// always a fresh copy — callers may mutate it freely without corrupting the
// cached entry or another caller's copy.
func (c *queryEmbedCache) Get(ctx context.Context, text string) ([]float32, error) {
	key := c.truncateKey(text)

	if vec, ok := c.lookup(key); ok {
		c.hits.Add(1)
		return copyVec(vec), nil
	}
	c.misses.Add(1)

	// singleflight collapses concurrent identical misses into one backend
	// call; followers share the leader's result (each gets its own copy
	// below).
	v, err, _ := c.group.Do(key, func() (any, error) {
		// Re-check: another goroutine may have inserted this key between our
		// lookup above and singleflight admitting us as the leader for it.
		if vec, ok := c.lookup(key); ok {
			return vec, nil
		}
		vec, err := c.backend.EmbedQuery(ctx, text)
		if err != nil {
			return nil, err
		}
		c.insert(key, vec)
		return vec, nil
	})
	if err != nil {
		return nil, err
	}
	return copyVec(v.([]float32)), nil
}

// lookup returns the entry's raw (un-copied) vector on a hit and marks it
// most-recently-used. Internal only — every path that can hand this slice to
// a caller outside the cache MUST copy it first (see Get).
func (c *queryEmbedCache) lookup(key string) ([]float32, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	c.ll.MoveToFront(el)
	return el.Value.(*queryCacheEntry).vec, true
}

// insert adds or refreshes key's entry and evicts the least-recently-used
// entry if the cache is now over capacity.
func (c *queryEmbedCache) insert(key string, vec []float32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		el.Value.(*queryCacheEntry).vec = vec
		c.ll.MoveToFront(el)
		return
	}
	el := c.ll.PushFront(&queryCacheEntry{key: key, vec: vec})
	c.items[key] = el
	if c.ll.Len() > c.capacity {
		oldest := c.ll.Back()
		if oldest != nil {
			c.ll.Remove(oldest)
			delete(c.items, oldest.Value.(*queryCacheEntry).key)
		}
	}
}

// Stats returns a point-in-time counter readout.
func (c *queryEmbedCache) Stats() QueryEmbedCacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	entries := c.ll.Len()
	return QueryEmbedCacheStats{
		Hits:    c.hits.Load(),
		Misses:  c.misses.Load(),
		Entries: entries,
	}
}

// copyVec returns a defensive copy of v — never hand a caller a slice backed
// by the cache's own storage.
func copyVec(v []float32) []float32 {
	out := make([]float32, len(v))
	copy(out, v)
	return out
}
