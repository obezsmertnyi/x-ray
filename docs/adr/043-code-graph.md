# ADR-043: x-ray-code — A Tree-sitter Code Graph as a Standalone MCP Service

**Status:** Implemented
**Commit:** —
**Date:** 2026-06-06
**Author:** Denys Vasyliev

---

## Context

X-Ray knows the *narrative* of a system — ADRs, contributors, events — but
nothing about the *structure* of the code that implements it. A user cannot ask
"who calls `VoiceManager.loadSessionConfig`?" or "what breaks if I change
`internal/vectorstore`?", and an external coding agent has no token-cheap,
structural source of truth to consult before editing.

We want X-Ray to become a code-aware companion for a repository: run it against
a codebase and the agent can talk about the repo, the project, and the code —
and external coding agents (e.g. Claude Code) can use the same graph as their
primary structural + semantic source, replacing repeated Grep/Read sweeps.

The reference implementation, the Python `code-review-graph` MCP server, parses a
repo with Tree-sitter into a structural graph and serves impact analysis +
semantic search over it. On this repo it produces ~10³ nodes — small enough that
no external database is warranted.

> **Note:** This ADR documents the **as-built** design. An earlier draft proposed
> representing code nodes as `MemoryCell` CRs inside X-Ray's shared memory store.
> That was prototyped and rejected (see *Alternatives*) in favour of a separate
> service, because code and prose want different indexes, lifecycles, and
> retrieval surfaces, and because Tree-sitter requires CGO that must stay out of
> the statically-linked `xray` image.

---

## Decision

Build **`x-ray-code`** (`tools/xray-code`): a **standalone Go binary** that parses
one or more repositories into an **in-memory graph of code, docs, and Kubernetes
manifests**, embeds every node for semantic search, and serves the result over
**its own MCP server**. It is a **separate Go module** so its CGO / Tree-sitter
grammar dependencies never enter the `xray` server binary. X-Ray's voice/chat
layer and external coding agents consume it as a remote MCP.

The graph is **in-memory, rebuilt on start** (no SQLite, no CRs): at 10³–10⁴
nodes a full parse is cheap and BFS/impact/centrality are trivial in process.
Incremental updates keep a running graph current without a full rebuild.

### Architecture

```
repos ──► parse (Tree-sitter + doc/yaml ingesters) ──► in-mem graph
                                                          │  cross-file call resolution
                                                          ▼
                                            embeddings (nomic via llama.cpp)
                                                          │
                                                          ▼
                                            MCP tools  ◄── X-Ray voice / coding agents
```

### Parsing — two tiers (broad coverage, deep where it pays)

- **Tier 1 — generic tags (any language).** Vendored Tree-sitter `tags.scm`
  queries (`parser/queries/`) extract symbols (functions, classes, methods,
  types) + call/reference edges with **one generic code path**, for: TypeScript /
  TSX, JavaScript, Python, Java, Rust, Ruby, C, C++, PHP, C#. A new language is
  searchable by vendoring its query — no Go code. (The stock TypeScript query was
  too thin for React/Next, so it is enriched in-repo.)
- **Tier 2 — rich extractor (per language).** A hand-written cursor walk with
  receiver-type inference and resolved edges. Currently **Go**. Highest fidelity
  call graph.
- **Docs.** Markdown (ADRs, todos, READMEs) → one `Doc` node per file + a
  `Section` node per `##` heading, embedded for semantic retrieval.
- **K8s manifests.** YAML (`---`-split, Helm-template-tolerant line scan) → one
  `Manifest` node per resource (`Kind/name`) + reference edges to container
  images, secrets, and configmaps.
- **Build/deploy config.** Dockerfiles, Makefiles, and non-manifest config YAML
  (`.ko.yaml`) → one `Build` node per file (matched by basename, not extension,
  since these are typically extensionless). Dockerfile `FROM` lines become
  `references` edges to base images. Non-manifest YAML that the K8s parser
  rejects (no `kind:`) falls back to a `Build` node instead of vanishing — this
  closes a coverage gap where the whole image/snapshot build layer was invisible
  to search.

### Graph, identity, resolution

- **Node identity** is repo-namespaced: `repo//path::Symbol` (file/doc nodes keyed
  by `repo//path`). This lets **many repos share one graph, index, and MCP
  endpoint** with no collisions.
- **Two-phase cross-file call resolution, on declared evidence only** (amended
  2026-09-03). The parser emits *unresolved* call edges carrying hints that name
  the call's shape (`bare`, `pkg`, `recvType`, `recvVar`) and its evidence: the
  receiver's package of origin (`recvPkg`) and, for a chained receiver, the
  steps from a bound root (`chain` with `rootType`/`rootPkg`, or `rootFunc` for a
  value a package-local function returned). To make that evidence available the
  Go extractor binds receivers, parameters, closure parameters, `var`/`:=`
  locals (including `x, err := f()`) and range variables, and records on each
  node what a chain is followed on: a struct's field types (`Fields`) and a
  function's single result type (`Result`, with `(T, error)` read as `T`, and
  slices/maps/channels recorded as their element and key). A linker
  (`graph/link.go`) builds a per-repo symbol table — nodes, plus the `imports`
  edges and an inferred module path so it knows which import paths are the
  repo's own packages — and rewrites each edge by the one rule its hints
  select, resolving only where the evidence points and only when exactly one
  declaration matches there: `pkg.Func()` into the repo package that import
  names; a chain by walking field and result types step by step (a foreign
  package ends it as external); a receiver of known origin in that package; a
  receiver of an unqualified type in the caller's own package; a bare `foo()`
  in the caller's own package. **There is no name-matching fallback.** The
  original "unique method name across the repo" last resort was removed after
  it was measured to link an `internal/xray` test's call to the k8s client's
  promoted `Update()` to `internal/recall/engine`'s `Indexer.Update`, and
  `req.Header.Set` on a `net/http` request to a local `SecretStore.Set` 56
  times. Fewer edges resolve than before — every one now rests on a declared
  type, import or package — and `LinkStats.ResolvedByRule` reports which rule
  produced each, so a regression in evidence shows up as a shift between rules.
  Known limits, all "external, not guessed": interface method calls (interface
  method specs are not nodes and Go has no `implements` edges), promoted
  methods from embedded fields, values reached through a type assertion or a
  func-typed field, and build-tagged twin declarations (`cgo`/`!cgo` files
  declaring the same name are ambiguous within their package).
- **Impact radius** is a bidirectional BFS that traverses **only real graph
  nodes** — unresolved bare targets (`Get`, `Store`) never become hubs, so blast
  radius stays precise. It follows dependency edges only (calls, imports,
  inherits, implements, references), never `contains`: a File, Doc or Section
  seed therefore comes back alone, and the reply says so (`note`), as it names
  seeds that are not nodes of the map (`unknown`). Likewise a search tool over
  a map that holds none of its kinds — `search_graph` on an agent-memory map of
  Doc/Section nodes — searches every kind the map has and says so in a `note`,
  instead of a bare `count: 0`: the tool promises "code, services, or another
  domain", and a document collection is that other domain.

### Search — brute-force cosine over nomic embeddings

- Nodes are embedded via the existing nomic model on a llama.cpp-compatible
  `/v1/embeddings` endpoint (default `http://localhost:8090`, 256-d), reusing
  X-Ray's `internal/vectorstore.Embedder`.
- Search is an **exact brute-force cosine scan**. At 10³–10⁴ nodes it is
  sub-millisecond and the query-embedding round-trip dominates; recall is 100%.
  The `Index.Search` interface hides this so HNSW can swap in past ~10⁵ nodes
  (e.g. many repos) without touching callers.
- Embeddings are **cached by node text**, so an incremental rebuild only embeds
  genuinely changed nodes.

### MCP tools (lean by design)

| Tool | Purpose |
|------|---------|
| `search_code` | Code + manifests by meaning — "how / what's deployed" |
| `search_docs` | Documentation prose by meaning — "why / what" |
| `get_node` | Full content of one node (qn) from a search hit |
| `query_graph` | Callers/callees of a symbol (filter by relation) |
| `get_impact_radius` | Blast radius of changing symbols |
| `get_code_graph_stats` | Node/edge counts, languages, embedded count |

Search is **split into code vs docs** so the model routes by intent (separate,
well-named tools beat one overloaded tool for reliable tool selection). Replies
are **intentionally lean** — short previews, default 5 results, max 20 — and
`query_graph` caps returned edges; the full body of a single node is fetched on
demand via `get_node`. This bounds the tokens an MCP result injects into the
caller's context (a doc section can be 2 KB; uncapped × 10 results would flood).

### Incremental updates + hooks

The `serve` process holds the live graph; thin CLI subcommands drive it over HTTP
so editor/agent hooks keep it current without a full rebuild:

- `xray recall update <files…>` → `POST /update`: re-parse only the changed files,
  rebuild the graph (re-resolving dependents via the per-repo symbol table),
  re-embed only changed nodes. ~10–70 ms vs ~8–25 s full rebuild.
- `xray recall status` → `GET /status`; `xray recall build` → `POST /reindex`.
- `.claude/settings.json` wires PostToolUse (Edit/Write/MultiEdit) →
  `hooks/post-edit.sh` (extracts changed paths, calls `update`) and SessionStart →
  `status`. Both are non-blocking (exit 0 if the server is down).

### Build & deployment

- **CGO required.** go-tree-sitter compiles and statically links the tree-sitter
  C runtime + grammars, so the binary needs `CGO_ENABLED=1` to build but only
  **glibc** at runtime. `make recall-build` builds the local binary;
  `make recall-image` / `recall-push` build a multi-stage image
  (`deploy/recall/Dockerfile`: golang+CGO → `chainguard/glibc-dynamic`). `ko`
  stays for the static `xray` image; it is not used here (it is CGO-free).

  > **Amended 2026-09-03:** this was originally a **separate module**
  > (`tools/xray-code`, own `go.mod`) producing its own binary. It is now one
  > repo, one binary, two modes — `xray` is the server, `xray recall …` is this.
  > The parser stays behind `//go:build cgo`, so the static `make build` binary
  > is unaffected. The targets and paths in this bullet are the current ones;
  > `xray-code-*` names elsewhere in this ADR are historical.
- **Two run modes:** (1) **local** — the code-graph/memory MCP for Claude Code
  agents (`xray-recall-local` in `.mcp.json`, served on :8084); (2) **in-cluster** —
  the image deployed as an MCP service with the existing nomic embedder sidecar
  (Helm chart is a follow-up).

---

## Consequences

**Positive**
- Code/docs/manifests are semantically searchable and structurally traversable
  across many repos from one process, one index, one MCP endpoint.
- Clean separation: CGO/grammar deps never enter the static `xray` image; code
  graph churn (per-commit) never pollutes the curated memory graph.
- Generic-tags Tier 1 gives broad language coverage cheaply; rich Tier 2 buys
  precision where it matters (Go today).
- Incremental update + embedding cache make hook-driven freshness ~360× cheaper
  than a rebuild.
- Lean, intent-split MCP tools keep result tokens bounded for the voice model.

**Negative / risks**
- Call resolution is heuristic; common-name methods stay external until Tier-2
  coverage grows (per-language extractors are real work).
- Brute-force cosine is exact but linear; many-repo scale (≥~10⁵ nodes) will need
  the HNSW swap behind `Index.Search`.
- In-memory only: a restart re-parses (fast) but loses nothing durable — there is
  no persistence story yet (acceptable; the repo on disk is the source of truth).
- The local mode depends on a running llama.cpp embeddings endpoint.

**Alternatives rejected**
- *Code nodes as MemoryCell CRs in the shared store* (the original draft) —
  prototyped (~770 cells ingested via the reconciler), then rejected: code + prose
  share one HNSW index (cross-contaminated ranking), overload `ThoughtLink`
  semantics, and inflate the namespace with per-commit churn.
- *Embedded SQLite mirror of code-review-graph* — unnecessary at 10³ nodes.
- *ko / CGO-free build* — go-tree-sitter requires CGO; grammars are excluded under
  `CGO_ENABLED=0`, yielding a zero-language parser.

---

## Implementation Notes

Module `tools/xray-code` (own `go.mod`, `replace` → parent for
`internal/vectorstore`):

| Path | Role |
|------|------|
| `parser/` | Tree-sitter parsing: rich Go extractor + generic `tags.scm` (`parser/queries/`) |
| `graph/` | In-memory graph, two-phase call resolution (`link.go`), impact BFS |
| `index/` | Cache-aware embeddings + brute-force cosine search |
| `docs/`, `k8s/`, `buildconfig/` | Markdown, Kubernetes-manifest, and build/deploy-config (Dockerfile/Makefile/non-manifest YAML) ingesters |
| `mcpserver/` | MCP tool definitions |
| `indexer.go` | Stateful indexer: full build + incremental `Update`/`Reindex`/`Status` |
| `serve.go` | MCP server + `/update` `/status` `/reindex` endpoints |
| `client.go` | Thin CLI clients (`update`/`status`/`build`) for hooks |
| `deploy/recall/Dockerfile` | Multi-stage CGO image |

Tests: `graph/link_test.go` covers each resolution rule and, for each, the
shape that must stay external (a same-named method in an unimported package, a
receiver from outside the repo, a bare call into another package, a chain
through an interface-typed or foreign field, a tied module vote);
`parser/extract_go_test.go` pins the hints and declared-type evidence the Go
extractor emits per call shape (bare, pkg, typed, origin-only, chained,
collection element/key, range variable, closure parameter, local-helper
result); `mcpserver/results_test.go` covers the reply shaping shared by the
live, read-only and direct tool surfaces. Follow-ups: Helm chart with git-clone
initContainer; per-language Tier-2 extractors; HNSW swap for many-repo scale.
