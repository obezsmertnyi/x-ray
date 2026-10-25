# ADR-053: Graduate MemoryCell to v1 and Align the Memory Store with the Core

**Status:** Implemented
**Date:** 2026-10-24
**Builds on:** [ADR-020](020-associative-memory-architecture.md) (kinds), [ADR-023](023-hnsw-lock-free-rebuild.md) (index rebuild), [ADR-030](030-hybrid-search-scoring.md) (hybrid search), [ADR-037](037-scope-as-privacy-axis.md) (scope)

## Context

X-Ray defined `MemoryCell` as `xray.ai/v1alpha1`. The Triage core vendored those
types, graduated them to `xray.ai/v1` on 2026-09-20, and has run the memory
store in production since, improving it in ways X-Ray never saw. Two problems
followed.

- **One CRD name, two versions.** Both projects install `memorycells.xray.ai`.
  The core's CRD serves only `v1`, X-Ray's only `v1alpha1`, so a cluster that
  runs both cannot hold both: the second `helm install` replaces the first CRD.
  The core's CRD notes already say the schema is X-Ray's to define and that a
  field X-Ray lacks is stripped by the next X-Ray apply.
- **The store drifted.** The core's copy of the search and index code gained
  fixes X-Ray did not have, several of them correctness fixes, not tuning.

## Decision

**`MemoryCell` is `xray.ai/v1`, and X-Ray takes the core's memory-store
improvements that do not depend on the core's tenants or issues.**

### API

- `api/v1alpha1` becomes `api/v1`; the CRD serves the single version `v1`. The
  schema is the core's, so one CRD file serves both projects.
- New field `spec.origin`: `observed`, `derived`, `human` or `verified`. A cell
  without one is derived.

### Provenance

- A write is **human** when its credential names a person (an OAuth-issued MCP
  token with a subject, a voice session of a signed-in user) and **derived**
  otherwise (the shared MCP secret names nobody). The server never accepts an
  origin from the caller.
- A derived write cannot replace a human cell. A person may replace a derived one.
- `upsert_memory` takes `evidence_refs`, the ids a cell was drawn from.
- `refute_memory` retires a cell that is wrong (status `stale`), records the
  reason and evidence under `data.refuted`, and retires every active derived
  cell that cites it by evidence ref or link, one hop. Observed, human and
  verified cells are never cascaded.

### Store

- **Bounded query-embedding cache** in front of every search embed (LRU, 4096,
  singleflight). The embedder's own exact-text cache stops growing at 20,000
  entries; it held every text embedded for the life of the process.
- **Dimension guard.** The store knows its embedding dimension; a vector of
  another length is refused on insert and on query instead of corrupting the
  graph. A stored embedding of the wrong length is re-embedded at sync.
- **Lazy rebuild on delete.** hnsw has no incremental delete. A delete marks the
  index dirty and the next read rebuilds once, so a retention sweep costs one
  rebuild. Deletes also now reach the index at all: they did not before, so a
  deleted cell kept taking top-k slots until restart.
- **Kind-filtered search exact-scans that kind** instead of over-fetching
  neighbours and dropping the wrong kinds, which returned nothing for a kind
  that was a small share of the corpus.
- The per-subject index no longer reads its entry map outside its lock.

### Search

- **Distinctiveness weighting.** Each query token is weighted by a smoothed
  inverse document frequency over the candidate set. The match bonus and the
  miss penalty scale by it, and for a query of three or more tokens a cell that
  fails the count-based coverage gate is still admitted when the tokens it
  matched carry at least 35% of the query's weight. "What is wrong with the
  bigquery export" finds the cell that names BigQuery.
- A cell's id is searchable (auto-generated ids are left out), and a query that
  is exactly an id returns that cell first.
- Fuzzy matching skips words whose length already rules them out and gives up
  the edit-distance DP as soon as it cannot succeed.
- **Recall shaping**, on by default (`--search-recall-shaping`): near-duplicate
  facts collapse to the best-ranked one (reported in `collapsed`), and the pool
  is re-ranked by an active-watch-item boost and a mild recency decay
  (half-life 7 days, floor 0.85). The pool is over-fetched to `limit*4` (at most
  60) so the re-rank has something to promote.

### Writes

- **Write-side dedup**, on by default (`--memory-write-dedup`): an upsert that
  restates a `stable_fact`, `operational_pattern`, `watch_item` or `hypothesis`
  lands on the cell it repeats (status `merged`, tags unioned). Cells must share
  kind, scope and subject; a model-written restatement is never merged into a
  human cell. Similarity is the core's: Jaccard over title+content tokens at
  0.3, 0.8 under eight tokens, 0.9 containment. One change: a short token with a
  digit counts, so "Q3 plan" and "Q4 plan" stay two cells.
- `consolidate_memory` reports the same clusters over what is already stored and,
  with `apply`, retires the restatements (status `superseded`, listed on the
  keeper's `supersedes`).

### Neighbourhood

`GET /v1/memory/neighbors?id=` returns a cell's nearest cells and the links among
them that mark sub-clusters (a pair is kept only when the two are tighter to each
other than to the centre; scale-free, no threshold).

### Writing rules and lifecycle

- A write through MCP or voice needs a kind from the vocabulary and, for a
  `stable_fact`, `confidence=high`. A kind filter must name a searchable kind
  (`system_configuration` is never indexed, so it is not one). `hypothesis` and
  `unknown` decay by default (24 hours, 7 days) and a merged restatement
  renews them.
- `expire_memory` (out of date, status `stale`), `supersede_memory` (write the
  replacement with `supersedes`, retire the old one) and `get_memory_context`
  (spreading activation over links: 0.6 per hop times link weight, depth 3, at
  most 8, shared cells only).
- A cell is embedded as `kind scope subject title content`, cut to 1800 bytes.
  No cluster holds stored vectors, so the format change costs nothing.

### Convergence across replicas

The core's mesh gossips hit and change events between replicas. X-Ray has no
mesh and does not need one for changes: every replica already watches the
cells. A cell seen through the watch updates the keyword index and, when its
state is new to the pod, is announced to live clients; a deleted or retired cell
leaves the keyword and vector indexes and is announced as deleted. A pod
recognises the watch event of its own write and does not announce it twice.
Search-hit pulses have no such source and stay on the pod that served the
search. If a semantic search's top window is rejected whole on lookup (every hit
is a cell gone elsewhere), the search ranks every cell exactly. The embed and
index phases of a semantic search are timed separately.

### Not taken

Everything tied to the core's tenants, issues, patterns and reflection:
tenant visibility and grants, concern-state re-ranking and note anchors, the
mind-map kind, near-duplicate and retention rules that read tenant policy. The
core moved its event stream from SSE to WebSocket; X-Ray keeps SSE until the web
UI changes with it.

## Consequences

- No cluster runs X-Ray yet, so `v1alpha1` is dropped without a conversion path
  or a migration.
- A derived write to a human cell fails, and a restated fact merges. Recall
  shaping and write dedup are switchable.

## Tests

`TestResolveOrigin`, `TestUpsert_OriginFromContext`, `TestMCP_OriginFollowsTheCredential`,
`TestRefute_RetiresCellAndCascadesToDerivedCitations`, `TestUpsert_WriteDedupMergesARestatedFact`,
`TestUpsert_WriteDedupBoundaries`, `TestConsolidateMemory`,
`TestSearch_CollapsesNearDuplicatesAndPrefersActiveWatchItems`,
`TestKeywordSearch_RareEntityOutranksFiller`, `TestKeywordSearch_ShortQueryStaysStrictAND`,
`TestEditDistanceWithin_MatchesEditDistance`, `TestSearch_ExactIDMatchGoesFirst`,
`TestSubjectIndex_DeleteIsLazyAndNeverReturned`, `TestSubjectIndex_KindFilterIsNotStarvedByAPopulousKind`,
`TestMemoryCellStore_DimensionGuard`, `TestPersist_WritesOriginAndEvidenceAndKeepsThemWhenTheItemHasNone`,
`TestMemoryNeighbors_ReturnsSharedNeighboursNearestFirst`, `TestValidateWrite`,
`TestSupersede`, `TestExpire`, `TestContext_SpreadsActivationOverLinks`,
`TestCellSyncer_FollowsCellsWrittenElsewhere`, `TestCellSyncer_ItsOwnWriteIsNotAnnouncedTwice`,
`TestMemoryCellStore_SearchExhaustiveRanksEveryEntry`, `TestMemoryCellStore_SearchReportsItsPhaseSplit`,
`TestCellEmbedText`.
