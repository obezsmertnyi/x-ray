# ADR-023: HNSW Lock-Free Graph Rebuild via Pointer Swap

**Status:** Implemented  
## Context

X-Ray uses a per-subject HNSW graph (`subjectIndex`) for semantic memory search. The graph supports three operations with very different cost profiles, measured at 128 dims (production `gemini-embedding-001` MRL) on Apple M3:

### Operation costs

| Operation | 100 nodes | 1k nodes | 5k nodes | 10k nodes |
|-----------|-----------|----------|----------|-----------|
| Insert (incremental) | 429 µs | 406 µs | 393 µs | 382 µs |
| Search (top-10) | 20 µs | 65 µs | 109 µs | 119 µs |
| Delete (full rebuild) | 7.6 ms | 193 ms | 1.6 s | 4.0 s |

Insert is O(log n) — flat in practice because HNSW's probabilistic layering bounds neighbor comparisons. Search is sub-linear (100× more nodes → 6× slower). Delete requires a full graph rebuild because HNSW has no node removal — dangling edges cannot be repaired in place.

### Memory per graph

| nodes | total alloc | per-node |
|-------|------------|---------|
| 100 | 2.3 MB | 23 KB |
| 500 | 17.6 MB | 36 KB |
| 1000 | 38.5 MB | 39 KB |
| 5000 | 212 MB | 44 KB |
| 10000 | 460 MB | 47 KB |

Per-node cost grows slowly (23→47 KB) as the graph gets denser — higher HNSW layers accumulate more neighbor links. Raw vector is 512 bytes (128 × float32); the rest is graph edges (M=16 per layer, M0=32 at layer 0).

### Current rebuild holds the write lock

`rebuildLocked` is always called under `s.mu.Lock()`, blocking all concurrent `search` calls (which need `s.mu.RLock()`) for the entire rebuild duration:

| nodes | reader blocked (under-lock) | reader blocked (swap) |
|-------|----------------------------|-----------------------|
| 100 | 11 ms | 42 ns |
| 1000 | 190 ms | 125 ns |
| 5000 | 1.6 s | 83 ns |
| 10000 | 3.96 s | **167 ns** |

### Update creates ghost nodes

HNSW `Insert` with an existing key adds a second node rather than replacing the old one. The old vector stays in the graph as a ghost — still matched by search, returning the updated doc via `entries[id]` but with stale proximity. This matters when mcell content changes (new content → new embedding → different vector space position). The ghost lives until the next full rebuild.

## Decision

Replace the under-lock rebuild with an out-of-lock build + pointer swap:

```
1. Snapshot entries under brief RLock  (~ns)
2. Build new HNSW graph outside any lock  (~4s at 10k)
3. Lock, swap s.index = newGraph, Unlock  (~167ns)
```

Readers are blocked only for step 3 — the pointer assignment. Step 2 runs concurrently with live search on the old graph.

**Memory trade-off**: during step 2, both old and new graphs exist simultaneously. Peak memory is ~2× single-graph size (920 MB at 10k). At X-Ray's current scale (~42 mcells) peak is ~2 MB — irrelevant. The trade-off is acceptable at any realistic X-Ray scale.

**When to trigger a rebuild**:
- Delete: always rebuild (ghost-free correctness)
- Reflection pass `applyOps`: bulk-collect all expired IDs, delete from `entries`, one rebuild per subject index touched — amortises cost across all expirations in the pass
- Update (content change): rebuild to evict ghost node — only occurs on first reconcile after content change, before new embedding is cached back to CR

## Implementation

In `internal/vectorstore/memorycellstore.go`:

1. Add `rebuildSwap()` — snapshots entries under `RLock`, builds new graph outside lock, swaps under `Lock`
2. `MemoryCellStore.Delete` removes from entries under `Lock`, then calls `rebuildSwap` outside the lock

Note: `BulkDelete` (batch remove + single rebuild) will be added when `ReflectionManager.applyOps` is implemented — adding it before the caller exists is dead code.

## Consequences

- Search latency during rebuild: 42–167 ns lock contention (was 4 s)
- Peak memory during rebuild: 2× single graph (~920 MB at 10k, ~2 MB at 42 nodes)
- Ghost nodes on content update: eliminated when rebuild is triggered on delete/expire
- Ghost nodes still possible for the one reconcile cycle between content change and embedding cache — accepted, same as current behaviour
