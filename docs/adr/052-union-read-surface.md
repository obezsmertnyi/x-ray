# ADR-052: One Read Surface Over Authored and Derived Memory

**Status:** Proposed (phase 0 implemented)
**Date:** 2026-08-31
**Builds on:** [ADR-037](037-scope-as-privacy-axis.md) (scope: public/private), [ADR-043](043-code-graph.md) (the graph), [ADR-045](045-xray-code-snapshots-per-project-graphs.md) (snapshots, per-map routing), [ADR-051](051-mcp-tools-general-agentic-memory.md) (memory tool surface)

## Context

X-Ray exposes **two MCP tool sets** that do the same job over different stores:

| | authored | derived |
|---|---|---|
| tools | `search_memory`, `get_memory`, `list_memory`, `recent_memory`, `fetch_memory`, `upsert_memory`, `delete_memory` | `search_graph`, `search_graph_docs`, `get_graph_node`, `get_graph_impact`, `get_graph_risk`, `get_graph_stats`, `update_code_graph` |
| unit | `MemoryItem` | graph node |
| store | MemoryCell CRs → etcd | `.graph.gob.gz` snapshots |
| relations | `Links []ThoughtLink{toId, relation, weight}` | `EdgeInfo{kind, source, target}` |
| written by | a person or an agent | re-derived from files |

They diverged for real reasons, but the divergence has leaked into every
consumer. An agent must learn two vocabularies for one question ("what do we
know about X"), and a skill must commit to a store before it knows where the
answer lives. The voice layer already pays this tax twice, in `voice_tools.go`
and `voice_codetools.go`.

Underneath, the two units are the same shape: **an embedded node with content
and typed relations**. `MemoryItem` and `NodeInfo` differ in field names and in
what identifies them, not in kind. Both are searched by cosine over an HNSW
index built from the same `--embedding-endpoint` / `--embedding-model` /
`--embedding-dims` flags (`cmd/xray/main.go:235` and `:640`), so within one
process their scores are already comparable.

What is *not* the same is durability. Authored content exists only where it was
written. Derived content is a cache of files and is regenerated wholesale by the
next `recall snapshot`.

## Decision

**Unify the read surface. Keep writes split.**

Introduce a `Source` interface that both stores implement, register every
available source in one registry, and expose union read tools that fan out and
merge. Writes stay separate, because the asymmetry is real: a write into a
derived store is discarded by the next re-derivation.

### The union node

```go
type Node struct {
    ID        string       // item ID, or repo-namespaced qn
    Origin    string       // "authored" | "derived"   ← never inferred from Kind
    Source    string       // "memory" | map label (e.g. "xray", "ccmem")
    Kind      string       // native vocabulary, unchanged
    Title     string       // item.Title | node.Name
    Content   string       // item.Content | node.Body, falling back to node.Content
    Locator   *Locator     // {File, Line} for derived; nil for authored
    Relations []Relation   // {Relation, TargetID, Weight}
    Score     float32
}
```

`Origin` is an explicit field, not a function of `Kind`. The two kind
vocabularies (`stable_fact`/`event`/… vs `Func`/`Doc`/`Manifest`) do not collide
today, and that coincidence must not become load-bearing.

### The Source interface

```go
type Source interface {
    Name() string
    Origin() string
    Search(ctx context.Context, q Query) ([]Node, error)
    Get(ctx context.Context, id string) (*Node, error)
    Related(ctx context.Context, id string) ([]Relation, error)
}
```

`MemoryStore` and the recall `Registry` each get an adapter. Neither store
changes internally.

### Tool mapping

| union tool | subsumes | notes |
|---|---|---|
| `search` | `search_memory`, `search_graph`, `search_graph_docs` | `origin`, `source`, `kind` are filters; `search_graph_docs` becomes `kind=Doc,Section` |
| `get` | `get_memory`, `get_graph_node` | returns relations both ways |
| `related` | (new) | 1-hop over `Links` or edges, uniformly |
| `impact` | `get_graph_impact` | derived-only at first; see Open Questions |
| `stats` | `get_graph_stats` | per source |

Writes keep their names and their asymmetry: `upsert_memory` / `delete_memory`
author a cell; `update_code_graph` re-derives from files. Listing tools
(`list_memory`, `recent_memory`, `fetch_memory`) stay authored-only — they are
time- and scope-ordered queries with no derived meaning.

## Consequences

### The hazard that must not be got wrong

Authored items enforce privacy by scattered guard, not by a chokepoint:
`isPrivateItem` is referenced **21 times in `memory.go` alone** and **24 times
across `internal/xray`**, ten of those as the guard
`isPrivateItem(kind, scope) && subject == ""`. Derived nodes have no ACL at all.
A union search that applies one shared filter **after** merging will leak
private cells.

Filtering therefore stays *inside* each `Source.Search`, and the union layer is
forbidden from post-filtering by scope. The `Query` carries the caller's subject
and each source decides for itself. A test must assert that a caller with no
subject cannot see a private cell through the union tool, mirroring the existing
per-call-site tests.

### Score comparability

Merging by raw cosine is only sound when every source embeds with the same
model and dimensionality. That holds in-process today, but nothing enforces it.
The registry must compare embedding fingerprints at registration and refuse — or
mark as non-mergeable, ranking within source only — any source whose fingerprint
differs. The snapshot loader already has this check
(`engine/registry.go:102`); reuse it rather than inventing a second one.

### Back-compat

The current names are bound by configured MCP clients, the voice tool builders,
four installed skills, and CLAUDE.md files in other repos. Old names stay
registered as thin aliases over the union implementation for at least one
release, and are removed only once nothing observed calls them.

### What this does not fix

An agent still cannot write durable memory through the derived path. `search`
becoming uniform makes that gap *more* visible, not less — which is correct: it
is a real property of a cache, and hiding it behind a uniform `upsert` would
mean silent data loss on the next snapshot.

## Phases

0. **Unblock CGO-free doc updates** — **DONE**.
   `engine/parse.go:145` already routes Markdown and YAML to pure-Go extractors
   before Tree-sitter, but `registry_nocgo.go` refuses *every* update. Partition
   paths: handle doc-shaped files, report code files as skipped. This makes a
   static binary a writable memory server for Markdown corpora (`ccmem`), with
   no CGO image and no toolchain in the cluster.
1. **Model + adapters.** Add `Node`, `Source`, the registry, and the two
   adapters. No tool changes. Pure addition, fully testable.
2. **Union tools alongside the old ones.** Register `search`/`get`/`related`/
   `stats`; keep every existing name working. Ship the privacy and fingerprint
   tests here, not later.
3. **Migrate consumers.** Voice tool builders collapse to one path; the four
   bundled skills and CLAUDE.md move to union names; old names marked deprecated.
4. **Retire aliases** once call logs show nothing using them.

Phase 0 is worth doing whether or not the union proceeds.

## Open questions

- **`impact` across origins.** `ThoughtLink` has `relation` and `weight`;
  `EdgeInfo` has `kind` and direction. A cross-origin traversal needs a shared
  relation vocabulary, or must stay within one origin per hop. Deferred: ship
  `impact` derived-only, revisit once `related` shows whether cross-origin hops
  are actually wanted.
- **An authored region in recall.** The only way to a genuinely single tool set
  including writes is a partition of a map that `snapshot` never regenerates.
  That is a feature with its own durability story (backup, replication,
  conflict) and belongs in its own ADR, not this one.

## Alternatives rejected

- **Unify writes too.** A uniform `upsert` would succeed and then lose the write
  on the next re-derivation. An API that silently discards data is worse than
  two honest APIs.
- **Federate the two MCP servers.** There is no proxying between them today and
  adding it would put a network hop and a second failure mode inside what is
  currently an in-process call. The union belongs below the tool layer, not
  between two servers.
- **Rename the graph tools to memory tools (or vice versa).** Renaming without a
  shared model buys a consistent vocabulary over two still-incompatible
  surfaces, and breaks every configured client for it.
