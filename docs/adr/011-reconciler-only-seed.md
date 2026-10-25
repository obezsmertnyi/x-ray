# ADR-011: Reconciler-Only HNSW Seeding

**Status:** Implemented  
## Context

After ADR-010 wired the controller-runtime cache as the read layer for `MemoryStore`,
two seed helpers remained:

- **`SeedFromDoc(doc, vec, rawContent, _)`** — wrote the item to `s.items` *and* called
  `cells.SeedFromEmbedding` to load a pre-computed vector into the HNSW index.
- **`seedItemDirect(cell)`** — wrote the item to `s.items` without HNSW indexing (keyword-only mode).

Both were called from `syncCell` in `CellSyncer`. Their purpose was to keep `s.items` in sync
with the CRs so that `Get`/`List`/`KeywordSearch` had data to return. With the cache reader
wired, `s.items` is no longer the source of truth for CR-backed items — the cache is. Writing
into `s.items` from the reconciler was redundant and maintained a stale duplicate.

## Decision

Remove `SeedFromDoc` and `seedItemDirect` from `MemoryStore`. Rewrite `syncCell` to:

1. **Keyword-only mode** (`s.memory.cells == nil`): no HNSW to maintain; call `markIndexed`
   to stamp `status.indexed=true` on the CR and return. The cache reflector already holds the item.

2. **Semantic mode, embedding present**: decode the stored vector and call
   `cells.SeedFromEmbedding` directly — no items map involved.

3. **Semantic mode, no embedding**: call `cells.Index` (Vertex AI), encode the vector, patch
   it back to `spec.embedding` on the CR, update status. The reconciler will fire again on
   the patch event and hit path 2.

Extract a `markIndexed` helper to avoid duplicating the status patch boilerplate.

## Consequences

**Positive:**
- `syncCell` is ~40 lines shorter with a single clear responsibility per path.
- No more dual-write: CR → cache (via reflector) is the only authoritative path.
- `s.items` is now purely the write buffer for ephemeral `Upsert` calls (agent_note,
  MCP writes not yet persisted to a CR). It is never populated from CR sync.
- The `triagev1alpha1` import is removed from the `cellsync_test.go` vectorstore dependency.

**Neutral:**
- `s.items` map and `mu sync.RWMutex` remain in `MemoryStore` for the ephemeral write path.
  Full removal requires routing all writes through the reconciler (future work).

## Relationship to other ADRs

- **ADR-010**: established the cache as read layer; this ADR removes the now-redundant
  items map writes from the sync path.
- **ADR-005**: `syncCell` description updated — the function no longer calls any `MemoryStore`
  seed methods; it only interacts with `cells` (HNSW) and the k8s API.
