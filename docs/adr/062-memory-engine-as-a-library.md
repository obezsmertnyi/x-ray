# ADR-062: The Memory Engine Is a Library

**Status:** Implemented

## Context

The server held its memory engine in `internal/xray`: the item store, the keyword
and trigram indexes, the hybrid search with its rerank and recall shaping, the
live event hub and the sync from `MemoryCell` objects. A second program that needed
the same behaviour could not import it, and had kept its own copy, so a fix to
one never reached the other. A review of both copies (`docs/analysis/memory-search-review.md`)
found the same defects in each: punctuation left in query tokens, filler words
counted as evidence, a score gap applied inside the vector store before
visibility, a tie between the ranking channels broken by map order.

## Decision

The engine moves to public packages, and the server and the Go SDK run on them:

| Package | Holds |
|---|---|
| `pkg/memstore` | `Item`, `Store`, `Searcher`, tags, private ownership, event time, near-duplicate detection, the kind vocabulary |
| `pkg/memstore/index` | the HNSW cell index, the embedder, the query cache |
| `pkg/memstore/hub` | the live event hub, with a `Transport` to carry events between replicas |
| `pkg/memstore/cellsync` | the decision for a cell: keep, delete at its deadline, forget; the migration of a private cell to the owner tag |
| `pkg/memstore/memtest` | fixtures for a host's tests |

There is one engine. What the server needs beyond the baseline is a switch on the
engine, with a test, and every switch defaults to the engine's own behaviour
(`Config.DefaultTTL`, `RenewOnMerge`, `SkipIDText`, `Vocabulary.FactConfidenceOptional`,
`cellsync.Options.KeepRetired`, and on the searcher `SubjectRouting`,
`SourceRelation`, `AlwaysFuse`, `NoShaping`). `internal/xray.MemoryStore` is an
adapter that keeps the server's method set and the wire shape of `MemoryItem`.

A private item's owner is a `user:<hash>` tag. The cell's `subject` names the
instance that holds it. `cellsync.MigratePrivateOwner` converts a cell written
under the older rule, in which `subject` was the owner, and is idempotent. The
MCP and REST `subject` argument means what it always did.

A cell past its retention deadline is deleted when the deadline falls
(`cellsync.Decide`) instead of at the next sweep. `--retention-sweep-interval`
remains as a backstop.

## Consequences

- A host imports the engine instead of copying it. A fix lands once.
- The search fixes of the review apply to the server: punctuation stripping,
  conversational stop words, a lexical answer is confident only when the matched
  words carry three quarters of the query's weight, no score gap inside the vector
  store, three-channel rank fusion with a deterministic tie-break, provenance,
  confidence and event-date weighting, and a deterministic order for equal
  traversal scores.
- The server no longer reads the cluster for keyword search. Its store is resident
  and fed by the CellSyncer, and a mirrored item carries no embedding.
- HNSW (`fogfish/hnsw`) collapses two nodes with identical vectors; a test pins it.
