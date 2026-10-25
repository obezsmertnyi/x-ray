# The code graph

`xray graph` is the derived half of X-Ray's memory. It parses repositories, documentation and Kubernetes manifests into one semantic graph (symbols, call edges, imports, doc sections), snapshots the graph to disk, and serves it over MCP. An agent asks structural questions instead of grepping: who calls this, what breaks if I change it, where was this decided. The pipeline is described in [graph-pipeline.md](graph-pipeline.md).

It runs on a laptop with no cluster and no database, or in a cluster next to the memory server. Embeddings come from an OpenAI-compatible `/v1/embeddings` endpoint you run (llama.cpp, Ollama, vLLM or LM Studio). Nothing else leaves the host.

| Question | Grep | `xray graph` |
|---|---|---|
| Where is X implemented? | many hits, most irrelevant | ranked by meaning |
| Who calls X? | textual matches, including comments | resolved call edges |
| What breaks if I change X? | none | `get_graph_impact` blast radius |
| Is this change risky? | none | `get_graph_risk` LOW, MEDIUM or HIGH |
| Why was it done this way? | none | `search_graph_docs` over ADRs and READMEs |

## Two builds

Only the Tree-sitter parser needs a C toolchain, so CGO is the boundary. The parse sub-commands are stubbed in the static build and exit with a message pointing at the CGO build.

| | static (`make build`) | CGO (`make graph-build`) |
|---|---|---|
| `serve-snapshots`, `servicemap`, `search`, `impact`, `services`, `update`, `status`, `build`, `install`, `keygen`, `dump <snapshot>` | yes | yes |
| `serve`, `snapshot`, `dump <repo-dir>` (these parse) | no | yes |

The static build is what runs in a cluster. The CGO build is for workstations and for the CI job that builds snapshots. The container image with the parser is published as `ghcr.io/triageagent-dev/xray:<version>-parser`.

## Quickstart

```bash
make graph-build                                   # bin/xray-parser
bin/xray-parser graph serve -repo myapp=/path/to/myapp
```

`serve` parses and embeds on start, holds the graph in memory and re-indexes on edit, listening on `127.0.0.1:8084`. Register it with the agent, for Claude Code in `.mcp.json`:

```json
{ "mcpServers": { "xray": { "type": "http", "url": "http://localhost:8084/mcp" } } }
```

`xray graph install -dir <repo>` drops four skills (explore-codebase, review-changes, debug-issue, refactor-safely) and a post-edit hook into the repository's `.claude/`, so the graph re-indexes itself as files change.

## Snapshots

Build the graph once, offline, into a portable `<label>.graph.gob.gz` (embeddings and the HNSW index included), then serve it from anywhere. No parser, no re-embedding, fast cold start.

```bash
bin/xray-parser graph snapshot -repo myapp=/path/to/myapp -snapshot-dir ~/.xray-maps
bin/xray graph serve-snapshots -snapshot-dir ~/.xray-maps
```

One `serve-snapshots` process serves many maps. A search with no `map` is routed by meaning to the best-matching one. `-writable` lets the server edit a worked copy, flush it back on eviction and shutdown, and keep agent notes (below).

**In the memory server.** `xray --mental-maps-dir <dir>` loads the same snapshots into the memory server, so one endpoint answers both `search_memory` and `search_graph`, and the voice layer gets code tools. A `MemoryCell` that carries a graph in `data.graph` registers as a map without a restart. For a graph too large for a cell (about 480 nodes), `data.graphRef` holds a digest-pinned OCI reference instead, say a `make maps-image` image, and the server pulls it, with the credentials of `--registry-pull-secret` for a private registry, and registers the same way. An encrypted snapshot opens with `--mental-maps-snapshot-key`. Cell: `kind: mental_map`, `subject: <map label>`, `data: {graphRef: {mime: text/plain, bytes: "ghcr.io/you/maps@sha256:..."}}`.

**Maps image.** `make maps-image MAPS="a b"` packs snapshots into a local OCI image tarball (a scratch image with `/maps/*.graph.gob.gz`). Push it to a registry and point a `mental_map` cell at it by digest (`data.graphRef`): the memory server pulls it and registers the map. The target never pushes; it prints the command to do so.

**Catalog maps.** `xray graph servicemap -in catalog.json -label name` builds a map from people, services or records instead of code. Pass `-summary` so the map can be routed to: a catalog has no README to describe it. `xray graph services <snapshot>` goes the other way and derives a service catalog from a code map.

## Encrypted snapshots

A snapshot is gzip over gob, and `strings` reads every field back. When maps travel as OCI images, `xray graph keygen -out snapshot.key` makes an age identity and prints its public recipient. Builders pass the recipient (`-snapshot-recipient`, `$XRAY_SNAPSHOT_RECIPIENT`); servers hold the identity (`-snapshot-key`, `$XRAY_SNAPSHOT_KEY`, or `--mental-maps-snapshot-key` on the memory server).

The key reads and the recipient writes. A map read encrypted is re-encrypted to the identity's own public key on every flush, and the flush refuses rather than write plaintext. The identity encrypts nothing else, so a plaintext map and the notes map stay plaintext. An encrypted map found with no matching key is skipped with a warning that names the flag. This protects the file at rest, not the MCP API: the running server holds every map in memory.

## MCP tools

All read tools carry the read-only, non-destructive and idempotent annotations. Replies are lean: 5 pointers by default and at most 20, and `limit` is the only size control.

| Tool | Arguments | Purpose |
|---|---|---|
| `search_graph` | `query`, `limit`, `map` | Find code and manifests by meaning |
| `search_graph_docs` | `query`, `limit`, `map` | Find documentation prose by meaning |
| `get_graph_node` | `qn` | Full body of one node and its relations both ways (`calls`, `called_by`, `imports`) |
| `get_graph_impact` | `seeds`, `depth`, `max_nodes` | Blast radius of changing symbols |
| `get_graph_risk` | `qn`, `depth`, `max_nodes` | LOW, MEDIUM or HIGH change-risk for one symbol |
| `get_graph_stats` | `map` | Node and edge counts, languages, embedded count, router summary |
| `list_skills`, `get_skill` | none, `name` | Operating procedures carried as `Doc` nodes in a map |
| `update_code_graph` | `paths` | Re-index changed files |
| `recall` | `query`, `limit` | Read agent notes back, newest first or ranked by `query` |
| `remember` | `text`, `title`, `attrs`, `ttl` | Write one note into the notes map; it expires after `-memory-ttl` (7 days) unless `ttl` says otherwise. Needs `-writable` |
| `forget` | `qn` | Remove one note. Refuses anything that is not a note. Needs `-writable` |

Node names are qualified by map: `repo//path::Symbol`.

## Access

Both servers listen on loopback unless told otherwise (`127.0.0.1:8084` for `serve`, `127.0.0.1:8085` for `serve-snapshots`), and every request but `/healthz` passes three checks.

- **Host.** On a loopback address only `localhost`, `127.0.0.1` and `::1` are accepted; `-allowed-hosts` adds names. This stops a web page from rebinding its own hostname to the loopback address and driving the server from the browser.
- **Origin.** A browser request from another origin is refused unless it is in `-allowed-origins`. MCP clients, curl and the hook send no Origin and pass.
- **Token.** `-auth-token-file` (or `$XRAY_AUTH_TOKEN_FILE`) requires `Authorization: Bearer <token>`. Without it nobody is asked for a credential, so a server given a wider `-addr` logs a warning.

```bash
openssl rand -hex 32 > ~/.config/xray/token
bin/xray-parser graph serve -repo myapp=/path/to/myapp -addr :8084 -auth-token-file ~/.config/xray/token
export XRAY_AUTH_TOKEN=$(cat ~/.config/xray/token)   # read by the hook and update/status/build
```

`update_code_graph` and `POST /update` index only files inside a served repository. A path that leaves one, through `../` or a symlinked directory the build did not follow, comes back in `unowned` and is never read.

## Command line

| Command | Does |
|---|---|
| `update <file>...`, `status`, `build` | Thin HTTP clients onto a running server; `update` re-indexes changed files in roughly 10 to 70 ms |
| `search <query>` | What `search_graph` does, straight over the snapshots with no server; needs the embedder. `-project`, `-kind code\|docs`, `-limit`, `-server URL` |
| `impact <qn>...` | `get_graph_impact` over a snapshot, with no server and no embedder |
| `dump <target>` | A repository directory is parsed; a snapshot is read. `-header`, `-vectors`, `-routing`, `-json` |
| `keygen` | Make an age identity for encrypted snapshots |
| `version` | Print the version |

## What it indexes

- **Code.** Go with resolved call, import and inherit edges; TypeScript, TSX, JavaScript, Python, Java, Rust, Ruby, C, C++, PHP, C#, Kotlin and Swift through a generic tags extractor that stores symbol bodies and call edges.
- **Documentation.** Markdown: one `Doc` node per file and a `Section` node per second-level heading.
- **Manifests.** YAML, Helm templates included: one `Manifest` node per document, with reference edges to images, Secrets and ConfigMaps. It reads files only, with no cluster access.
- **Infrastructure.** Compose files, Dockerfiles, application config and SQL DDL feed the service map that `xray graph services` derives.

## In the cluster

There is one chart, `xray`. The memory server takes maps as cells that carry a graph (`data.graph`) or point at one in a registry (`data.graphRef`), or as files in `--mental-maps-dir`; the voice model and every MCP client then reach them on the same endpoint as the memory tools. For a snapshot server on its own, run `xray graph serve-snapshots` from the same image.
