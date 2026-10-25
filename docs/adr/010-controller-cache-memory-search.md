# ADR-010: Controller-Runtime Manager and Cache-Backed Memory Search

**Status:** Implemented  
**Todo:** [memory-search-and-controller](../todo/memory-search-and-controller.md)  

## Context

Before this change:

- `main.go` built a raw `client.Client` and a standalone `cache.New` cache. CR updates were
  delivered via a hand-wired `informer.AddEventHandler` with only `UpdateFunc` — new and deleted
  CRs were silently ignored after startup.
- All read paths in `MemoryStore` (`KeywordSearch`, `List`, `ListSummary`, `AllItems`, etc.)
  iterated a duplicate `map[string]*MemoryItem` maintained in process. At ~100 items this is fine;
  at 10k+ items with kilobyte-sized content the O(n) scan becomes a bottleneck and the map is a
  second copy of truth that can drift from the actual CRs.
- `SeedAll` ran in a background goroutine — HTTP started immediately, so early requests saw a
  partially populated store.

## Decisions

### 1. controller-runtime Manager

`main.go` creates a `ctrl.NewManager` with a namespace-scoped cache. `CellSyncer` is registered
via `SetupWithManager`, which gives:

- A proper reconcile loop with exponential backoff on errors.
- `AddFunc`, `UpdateFunc`, and `DeleteFunc` all handled — CRs created or deleted while the pod
  is running are picked up without restart.
- A single cache shared by the Secret watch, MCP token informer, and the memory read path.

The built-in metrics and health probe servers are disabled (`BindAddress: "0"`) — X-Ray runs
its own HTTP server.

### 2. Field indexes

Four field indexes are registered on `AwarenessMemoryCell` before `Manager.Start()`:

| Index field    | Used by                          |
|----------------|----------------------------------|
| `spec.kind`    | `List`, `ListSummary`, `KeywordSearch` |
| `spec.subject` | `List`, `ListSummary`, `KeywordSearch`, `AllItems` |
| `spec.status`  | future: direct status-filtered list |
| `spec.id`      | `GetItem`, `getByID` (semantic search result resolution) |

The cache stores an in-memory inverted map per index; `MatchingFields` queries hit the index
directly instead of scanning every object. Combined kind+subject filtering narrows the working
set before content substring matching, keeping keyword search O(k) where k is the matching
subset, not O(n) over all CRs.

### 3. Cache-backed read path with ephemeral map merge

`MemoryStore.SetCacheReader(r, namespace)` is called after `WaitForCacheSync`. All read methods
follow the pattern:

1. Query the cache with `MatchingFields` for kind/subject.
2. Merge in ephemeral entries from `s.items` (items written via `Upsert` that are not yet in a CR —
   `agent_note`, in-flight MCP writes).
3. Fall back to full `s.items` map scan on cache error or when no reader is wired (local dev,
   no kubeconfig).

The items map is **not removed** — it is the write buffer for items that exist in memory before
`CellSyncer.Persist` has written them to a CR, and for `agent_note` items which are never
persisted. `SeedFromDoc` and `seedItemDirect` remain to seed the HNSW index during embedding
backfill.

### 4. HTTP server blocked on WaitForCacheSync

`mgr.GetCache().WaitForCacheSync(mgrCtx)` is called on the process-lifetime context before
`httpServer.ListenAndServe`. There is no timeout: the pod stays unready (connection refused on
the HTTP port) until the API server delivers the initial list. Kubernetes readiness probes will
not route traffic until the server is up.

This is correct because:
- An arbitrary timeout (e.g. 90 s) causes partial-data serving when the API server is slow.
- The manager context (`mgrCtx`) is already cancelled by SIGTERM, so the block is not permanent
  — a pod that cannot reach the API server will be restarted by Kubernetes liveness probes.

`SeedAll` (embedding backfill) runs in a goroutine after sync and uses the same `mgrCtx`, so
it is cancelled cleanly on shutdown mid-embed.

## Consequences

**Positive:**
- New/deleted CRs picked up immediately without restart.
- Keyword search on kind+subject is now O(k) via cache index, not O(n) full scan.
- Single shared cache for Secret watch, token informer, and memory reads — no duplicate watch connections.
- First request after pod start sees the full CR list.
- Proper retry/backoff for reconcile failures.

**Negative / tradeoffs:**
- `WaitForCacheSync` means startup takes as long as the initial LIST round-trip to the API server
  (typically <1 s in-cluster, ~2–3 s cold). Acceptable; no previous guarantee was better.
- Items written via `Upsert` (not yet persisted to a CR) live only in `s.items`. They appear in
  search results via the map-merge path but will be lost on pod restart unless `Persist` has run.
  This is unchanged from before.
- `s.items` map is still present; full removal (ADR future) requires eliminating the HNSW seed
  path and routing all writes through the reconciler.

## Amendments

### 2026-05-26 — Trigram-first result ranking (`162d2ff`)

Both `keywordSearchMap` and `keywordSearchCache` truncated results at the limit in raw iteration order (Go map / cache-list order). When multiple items keyword-matched a query, the trigram-best candidate could be pushed out by weaker matches depending on iteration order — producing non-deterministic results across identical voice queries.

Fix: each search path now does a two-pass collect (trigram hits bucket first, plain keyword hits second) followed by deterministic relevance scoring (`rankKeywordHits`) before the limit is applied. Score tiers: exact-title match > title contains all tokens > content match > recency/ID tie-break.

## Relationship to other ADRs

- **ADR-005** (CellSyncer): `Reconcile` and `SetupWithManager` existed but were never called from
  `main.go`. This ADR activates them. `SeedAll` now serves only as an embedding backfill pass; the
  cache is the authoritative in-memory view.
- **ADR-002** (Persistence paths): MCP `upsert_memory` → `Upsert` → `s.items` → `Persist` → CR
  path is unchanged. The reconciler will pick up the CR and confirm it into the cache on the next
  watch event.
