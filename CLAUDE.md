# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

**X-Ray** is an agentic memory substrate for local and Kubernetes-native use, written in Go. It keeps authored memory (typed items, `MemoryCell` objects in a cluster) and derived memory (a code graph, `xray graph`), searches both by keyword and by meaning (HNSW), and serves them over MCP, REST and Gemini Live voice, with OAuth-based authentication (GitHub, Entra).

**The web UI is part of the binary.** It lives in `ui/` (a Next.js static export), `make ui` builds it and stages it in `internal/webui/web/`, and `//go:embed` compiles it into `xray`, which serves it at `/` on the same origin as the API. `make build UI=0` builds without it (no Node needed) and the binary then serves the API alone. The code graph has its own, smaller embedded page at `/ui` (`xray graph serve`).

**Key distinguishing characteristics:**
- No dependency on Runway or LiveKit — those integrations have been removed
- Single binary `xray` — API, controller, voice, MCP and the code graph (`xray graph`) in one binary; the web UI embedded (`ui/`, Node 22 to build it). Two builds: static (`make build`) and CGO with the Tree-sitter parser (`make graph-build`)
- Kubernetes-native but gracefully degrades without k8s (no CRDs, no reconcilers required)
- Module: `github.com/triageagent-dev/x-ray/v2`

## Project Structure

```
xray/
├── cmd/xray/                # Main binary entry point
│   ├── main.go              # Server initialization, k8s Secret watch, voice setup
│   └── graph*.go            # `xray graph <sub>`: serve, snapshot, serve-snapshots, search, impact, ...
├── internal/
│   ├── xray/                # Core server logic
│   │   ├── server.go        # HTTP API routes (Huma framework), session mgmt, SSE
│   │   ├── voice.go         # Gemini Live voice session management
│   │   ├── voice_tools.go   # Memory tools wired to Gemini Live
│   │   ├── hub.go           # SSE hub broadcasting memory-hit and memory-change events
│   │   ├── memory.go        # In-memory store with HNSW semantic search
│   │   ├── cellsync.go      # Seeds HNSW from MemoryCell CRs
│   │   ├── auth.go          # Session store & OAuth session management
│   │   ├── mcpserver.go     # Optional MCP server for external tool access
│   ├── codegraph/           # Code graph (`xray graph`): parser, engine, graph, index, mcpserver, httpguard, ...
│   ├── webui/               # Embeds the UI export, serves it with a per-document CSP
│   ├── config/              # Configuration & Kubernetes Secret watchers
│   ├── vectorstore/         # HNSW indexing & embeddings (Vertex AI or gateway)
│   └── version/             # Version injection via ldflags
├── pkg/memory/              # Public library: the memory store, in the caller's process
├── pkg/codegraph/           # Public library: the code graph, in the caller's process
├── pkg/xray/                # Public Go SDK: a typed MCP client, and the Memory and CodeGraph interfaces both implement
├── ui/                      # Web UI: Next.js static export, embedded into the binary
├── api/v1/            # MemoryCell CRD (xray.triageagent.dev/v1, short: mcell)
├── charts/                  # Helm charts
│   ├── xray/                # Main deployment chart
│   └── xray-graph/          # code-graph snapshot server chart
├── docs/adr/                # Architecture Decision Records
├── scripts/                 # upsert_memory.py and other tooling
├── Makefile                 # Build targets (build, push, run, lint, test)
├── .ko.yaml                 # ko build config (used by Makefile push)
└── go.mod / go.sum          # Go module definition
```

## Architecture Highlights

### Backend (Go)

**HTTP Server + Huma API Framework**
- Single `net/http` server on `:8080` (configurable via `--http-address`)
- API routes in `internal/xray/server.go` using [Huma](https://github.com/danielgtaylor/huma/v2) for OpenAPI auto-generation
- CORS enabled for all origins

**Session Lifecycle**
1. **OAuth Login** → `/v1/auth/github` or `/v1/auth/entra` → session cookie (24h expiry)
2. **Voice** → `GET /v1/voice` (WebSocket upgrade) → Gemini Live bidirectional audio session

Session carriers (all resolved by one function, `sessionFromRequest` in `auth.go`): the `xray_session` cookie (same-origin browser), `Authorization: Bearer <token>` (MCP clients, and a UI on another origin), and `Sec-WebSocket-Protocol: bearer, <token>` — read **only** on an upgrade request, because a browser cannot set headers on a WebSocket handshake and `/v1/voice` would otherwise be unreachable for a bearer-authenticated UI. A login may hand its token to a UI on another host via `/v1/auth/github?ui=<origin>`, but only for origins listed in `--ui-callback-origins` (empty by default = feature off; exact scheme+host+port match, checked at login so an unapproved origin is never bound to a live login).

Public endpoints (no auth required): `/v1/auth/*`, `/v1/version`, `/healthz`, `/manifest.json`, `/sw.js`. Everything else under `/v1/*` (including `/v1/voice` and all `/v1/memory*` routes) requires a valid session via `requireSession` (ADR-027). Auth is bypassed only in `--dev` mode (no OAuth provider configured), which is fatal at startup in production.

**Voice Layer (Gemini Live)**
- `VoiceManager` in `voice.go` manages active sessions; lazily initialises the genai client
- On `/v1/voice` WebSocket upgrade: loads system prompt + tool allowlist from `system_configuration` memory cell (subject=xray), then opens a `genai.Session` to Gemini Live
- Two concurrent goroutines per session: one reading from browser, one reading from Gemini
- `--voice-name` (default: `Kore`) and `--voice-model` flags override CR-based config
- Supports Vertex AI (`--gcp-project`) or Google AI API (`--gemini-api-key`)
- `gemini-3.8-live` is served on Vertex only in the multi-regions `eu`/`us`: run voice there with `--voice-location` (default `--gcp-location`, which keeps the embeddings location). Affective dialog is never sent to a `gemini-3*` model. Per answer 6 tool calls, per session 40 (the core's bounds); `end_session` is answered by hanging up, not by a tool response (ADR-058)
- With sign-in off (`--dev`) every request without a session is the built-in user `dev` (ADR-057); a session opens with the user's newest private notes and session memories in its prompt
- Model: `gemini-3.8-live` (both Vertex AI and Google AI backends). Not hardcoded — the `--voice-model` flag default sets the baseline, and `system_configuration.voiceModel` (CR) overrides per session. (The former Google AI model `gemini-2.0-flash-live-001` was retired 2025-12-09.)
- Tool calls (e.g. `search_memory`) execute in parallel goroutines and results are sent back via `SendToolResponse`

**Memory & Vector Search**
- In-memory store of memory cells (type `MemoryItem`) keyed by ID
- **Keyword mode** (default): substring matching on title + content
- **Semantic mode** (with `--embedding-endpoint` or `--gcp-project`): HNSW graph indexed via Vertex AI or OpenAI-compatible gateway
- `search_memory` voice tool and `search_memory` MCP tool scope results to subject (e.g., "xray"), and also merge global (empty-subject) results
- Optional: seed from `MemoryCell` CRs (`mcell`) in k8s namespace via `CellSyncer`
- A `mental_map` cell carries a code/service graph inline (`data.graph`) or points at an OCI image by digest (`data.graphRef`, ADR-056); `--mental-maps-dir` turns the map registry on, `--registry-pull-secret` names the pull credentials
- Memory changes (upsert/delete) are broadcast via SSE as `MemoryChangeEvent` (op=upsert|delete)
- Memory search hits are broadcast via SSE as `MemoryHitEvent` — used by X-Ray UI to highlight active nodes

**SSE Event Stream**
- `GET /v1/memory/events` — single SSE stream carrying both `MemoryHitEvent` and `MemoryChangeEvent`
- X-Ray pages and Voronoi graph subscribe to this stream for live updates without page refresh

**Kubernetes Integration** (Graceful Degradation)
- Optional k8s client for Secret watching and MemoryCell seeding
- If kubeconfig unavailable (local dev): warns and continues without k8s features
- Watches for changes to:
  - the MCP token Secret named by `--mcp-token-secret` (no default; only watched when MCP is enabled)

**MCP Server** (Optional)
- Separate HTTP server on `:8083` (configurable via `--mcp-address`; leave empty to disable)
- Exposes `get_memory`, `list_memory`, `search_memory`, `fetch_memory`, `recent_memory`, `upsert_memory`, `delete_memory`, `refute_memory`, `consolidate_memory`, `expire_memory`, `supersede_memory` and `get_memory_context` tools (read scope `xray:memory.read`, write scope `xray:memory.write`)
- Bearer token auth (from Secret) if `--mcp-token-secret` set
- Images in `upsert_memory` are automatically downscaled to 512×512 JPEG
- Provenance (ADR-053): a write is `human` when the credential is an OAuth JWT with a subject (or a signed-in voice session), `derived` otherwise; the server sets it, the caller cannot. A derived write never replaces a human cell. An `upsert_memory` that restates a `stable_fact`/`operational_pattern`/`watch_item`/`hypothesis` merges into the cell it repeats (`status: merged`)
- Useful for external LLM agents to access or write X-Ray memory

**Memory kinds** (`upsert_memory`) — with no `ui.categories` configured the UI shows:

| UI category | `kind` | When to use |
|-------------|--------|-------------|
| **Concerns** | `active_concern` | Open problems being tracked |
| **Patterns** | `operational_pattern` | Recurring behaviour worth remembering |
| **Facts** | `stable_fact` | Durable facts |
| **Identity** | `system_identity` | Services, repos, people |
| **Events** | `event` | Time-bound events; carries `event_date` (RFC3339) |
| **Notes** | `note` | Free notes; `scope=private` with `subject` as the owner |

- Also valid: `recent_change`, `watch_item`, `hypothesis`, `unknown`. Server-written: `episodic` (voice session memory), `system_configuration`, `mental_map`.
- Privacy is `scope` (`public` or `private`), not a kind. `agent_note` no longer exists (ADR-037). Voice writes are always `note`/`private` with `subject` = hash of the user.
- A deployment overrides the categories with `ui.categories` in the `system_configuration` cell; a category can select by `tag` (for example `project`, `contributor`).
- `system_configuration` is reserved — filtered from search unless explicitly requested; holds voice and UI settings (subject=xray)
- Use `scripts/upsert_memory.py` when upserting with an image (MCP direct call drops image data on update)

### Frontend — `ui/`, embedded in the binary

The web UI is a **pure client** of this API: a static Next.js export (`ui/`) that
`internal/webui` embeds and serves at `/`, with a script-src hash per HTML
document (no `'unsafe-inline'` for scripts). The server's `withUI` sends `/v1/*`,
`/healthz`, `/manifest.json`, `/sw.js`, `/mcp` and the OAuth endpoints to the
API and everything else to the UI.

Same origin is required, and embedding is what gives it for free:
- `/v1/voice`'s WebSocket URL is built browser-side from `window.location.host`.
- The `xray_session` cookie is set with no `Domain`, so it is host-only.
- `voice.go`'s `CheckOrigin` rejects a WebSocket whose `Origin` != `--public-url`.

So `--public-url` is the address users reach, and it is also the OAuth callback
base. For a UI on a host that cannot receive the cookie at all,
`--ui-callback-origins` allowlists origins a login may hand its session token to.

The UI speaks this server's own API, `/v1`, by default (voice at `/v1/voice`,
live events as server-sent events at `/v1/memory/events`, per-user notes, no
tenants). The same sources build for a Triage deployment with
`NEXT_PUBLIC_API_PREFIX=/v2`; `lib/api.ts` (`endpoints`) is the one place the
two differ, and `ui/__tests__/dialect.test.ts` pins the `/v1` side.

`ui/` conventions: path alias `@/*` → `ui/`; vitest + testing-library; the graph
is `components/memory-graph.tsx` (a D3 Voronoi over `/v1/memory/summary`, bounded
preview, search always queries the server), and a recall lights it through
`lib/opto.ts`. Run `make ui-test` before committing UI changes.

## Common Commands

**Build & Run**

```bash
make build             # Build the static xray binary: the web UI embedded (make ui), API, MCP, voice, graph read side
make build UI=0        # The same without the web UI (no Node needed)
make ui                # Build the web UI and stage it for embedding
make ui-dev            # UI hot reload on :3000 against a running server (no voice, no live events)
make ui-test           # Type-check, lint and test the web UI
make graph-build       # Build the CGO binary bin/xray-parser (adds `xray graph serve|snapshot|dump <dir>`)
make run               # Run locally with the web UI on :8080, sign-in off
make image             # Build container image locally via ko.local
make push              # Build and push image to registry (updates Chart.yaml appVersion)
```

**Quality**

```bash
make lint              # gofmt + go vet
make fmt               # gofmt -w (format code)
make test              # go test ./...
```

**Cleanup**

```bash
make clean             # Remove bin/
```

**Version Info**

```bash
make version           # Show computed version string
```

## Build & Deployment

**Build System**
- `Makefile` computes version from git tags: `v<major>.<minor>.<commits-since-tag>-<short-commit>`
- ldflags inject version, commit, and build date into binary
- `.ko.yaml` config used for OCI image builds (leverages ko tool for efficient builds)

**Docker Image**
- Base image: `cgr.dev/chainguard/static:latest` (minimal distroless)
- No CGO, statically linked binary

**Helm Chart** (charts/xray/)
- Deployment with configurable replicas, image, and resource limits
- Service (ClusterIP) on port 80
- Ingress template with optional GCP pre-shared cert annotation
- RBAC ServiceAccount for k8s client access
- Mounts Secrets as env vars (MCP token)

**Deployment Flags** (set in values.yaml `args`)
- `--http-address`: HTTP listen address (default `:8080`)
- `--log-level`: debug/info/warn/error
- `--namespace`: Kubernetes namespace for Secret watch (default `xray`)
- `--embedding-endpoint`: OpenAI-compatible gateway for embeddings (optional)
- `--embedding-model`: Embedding model name (default `bge-small-en-v1.5`)
- `--semantic-score-gap`: semantic relevance cutoff, absolute cosine gap below the top hit (default `0`, no cutoff; `0.02` was the previous default)
- `--search-lexical-shortcircuit`: let a full-coverage keyword hit skip semantic retrieval (default `false`; always fuses keyword and semantic results by RRF)
- `--gcp-project`: GCP project for Vertex AI embeddings + Gemini Live voice
- `--gcp-location`: GCP location (default `europe-west4`)
- `--gemini-api-key`: Google AI API key for Gemini Live (alternative to `--gcp-project`)
- `--voice-model`: Override Gemini Live model name
- `--voice-name`: Gemini prebuilt voice name (default `Kore`)
- `--mcp-address`: MCP server listen address (default `:8083`; empty to disable)
- `--mcp-token-secret`: k8s Secret containing MCP Bearer token
- `--mcp-list-memory-max-limit`: hard ceiling on `list_memory`'s `limit` param, regardless of caller request (default 20, ADR-051)
- `--mcp-search-memory-max-limit`: hard ceiling on `search_memory`'s `limit` param, regardless of caller request (default 10, ADR-051)
- `--search-recall-shaping`: collapse near-duplicate facts in `Search` and re-rank by state (default true, ADR-053)
- `--memory-write-dedup`: merge a restated fact into the cell it repeats on upsert (default true, ADR-053)

## Key Dependencies

**Backend:**
- `github.com/danielgtaylor/huma/v2`: REST API framework with OpenAPI
- `github.com/mark3labs/mcp-go`: MCP (Model Context Protocol) server
- `github.com/fogfish/hnsw`: HNSW vector indexing (semantic memory)
- `github.com/rs/zerolog`: Structured logging
- `google.golang.org/genai`: Vertex AI embeddings + Gemini Live voice
- `github.com/gorilla/websocket`: WebSocket for voice sessions
- Kubernetes client libraries (controller-runtime, client-go)

**Frontend (`ui/`):**
- `next@16`: App Router, static export
- `tailwindcss@3.3`: Utility CSS
- `d3`: Voronoi memory graph visualization
- `lucide-react`: Icon library

## Memory Cell CRD (Kubernetes)

X-Ray can sync memory from `MemoryCell` custom resources (`xray.triageagent.dev/v1`, short: `mcell`, defined in `api/v1/`):
- Each cell is an independent k8s object with embedded vector embedding
- Fields: kind, scope, subject, title, content, confidence, status, tags, links
- Scope values (ADR-037): `public` (shared — visible on the graph and in user-facing search; the default for normal memory) or `private` (isolated to its `subject` — a person's userHash, or a reserved owner like `agent`/`system`; hidden from everyone else). `system_configuration` cells are always treated as private regardless of scope.
- Status tracks: `indexed`, `lastIndexedAt`, `conditions`
- CellSyncer loads cells at startup and watches for changes
- Embeddings are cached in CR storage — startup is fast on second run (no re-embed)
- Useful for persistent multi-session memory across pod restarts

## Local Development Workflow

**1. Server and UI**
```bash
make run                 # :8080, --dev (auth disabled), web UI at /
make build && ./bin/xray --dev
```

**2. Backend + devenv (envtest control plane + MemoryCell CRD)**
```bash
make devenv    # Terminal 1: blocks
make run-dev   # Terminal 2: against devenv with local Ollama embeddings
```

**3. UI hot reload** — `make run` in one terminal, `make ui-dev` in another.
`npm run dev` on :3000 proxies `/v1` to the server but cannot proxy a WebSocket
upgrade, so voice does not work there: use `make run` and the embedded UI for it.

## Testing & Debugging

- `go test ./...` for the server and libraries, `make ui-test` for the web UI
- Key debug logs available at `--log-level=debug`
- MCP server useful for debugging memory search (external LLM agents can test tool availability)
- Voice session logs show tool calls and results at debug level
- SSE stream at `/v1/memory/events` useful for observing live memory activity

## Notes for Future Work

- Memory is in-process only (no persistence across pod restarts unless seeded from k8s CRDs)
- MCP server (`--mcp-address`) optional; disabled by leaving empty
- Embedding model selection (`--embedding-model`) shared across Vertex AI and gateway modes
- Version embedding requires `.ko.yaml` ldflags template (Makefile `push` target updates this correctly)
- Voice session config (system prompt, enabled tools, voice name, model) is loaded from `system_configuration` memory cell at session start — no restart needed to update
- **TODO: Voice tool content budget** — see `docs/adr/018-voice-tool-content-budget.md`. Files to touch: (1) `api/v1/memorycell_types.go` — add `MaxItemContentChars int` to `VoiceSessionConfig`; (2) `cmd/xray/main.go` — add `--voice-item-content-limit` flag, pass to `NewVoiceManager`; (3) `internal/xray/voice.go` — add `maxItemContent int` to `VoiceManager`, return from `loadSessionConfig` (CR wins over flag when non-zero); (4) `internal/xray/voice_tools.go` — pass limit into `buildVoiceTools` → `defSearchMemory`, apply truncation cap, reword `list_memory` description to require `kind` and steer model toward `search_memory`; (5) `charts/xray/values.yaml` — add `voice.itemContentLimit: 500`.
- **DONE: OTEL tracing added to voice sessions** — see `docs/adr/007-otel-tracing-voice-sessions.md`. Files to touch: (1) create `internal/observability/otel.go` with `ConfigureOTel(endpoint, projectName string)`; (2) add `--otel-endpoint` and `--otel-project` flags + call `ConfigureOTel` in `cmd/xray/main.go` var block before `NewServer`; (3) add `tracing.endpoint` / `tracing.projectName` to `charts/xray/values.yaml`; (4) add `voice.session` and `llm.gemini_realtime` spans in `internal/xray/voice.go`; (5) add `tool.<name>` spans in `internal/xray/voice_tools.go`. Reference: `../triage-agent/internal/observability/otel.go` and `../triage-agent/internal/jucce/session.go`.

<!-- code graph MCP tools -->
## MCP Tools: the X-Ray code graph

**IMPORTANT: this repo can be indexed by its own code graph (`internal/codegraph`,
ADR-043, ADR-054). When the `xray` MCP server is configured, PREFER its tools
over Grep/Glob/Read when exploring the codebase.** The graph is faster, cheaper
(fewer tokens), and gives structural context (callers, callees, blast radius)
that file scanning cannot. It also indexes documentation (ADRs/todos/READMEs)
and Kubernetes manifests.

### When to use graph tools FIRST

- **Find implementation**: `search_graph` (functions, classes, types, files, K8s manifests) instead of Grep
- **Find rationale/decisions**: `search_graph_docs` (ADRs, todos, READMEs) instead of reading whole docs
- **Read one result in full**: `get_graph_node` (search returns lean pointers; fetch the full body for the one node you need)
- **See a node's relations** (what it depends on + what depends on it, both directions): `get_graph_node` returns them alongside the node's content
- **Understand impact**: `get_graph_impact` instead of manually tracing edges
- **Refresh after edits (if no hook ran)**: `update_code_graph` with the changed file paths

Fall back to Grep/Glob/Read **only** when the graph doesn't cover what you need.

### Key Tools

| Tool | Use when |
|------|----------|
| `search_graph` | Find code/manifests by meaning — the "how / what's deployed" |
| `search_graph_docs` | Find docs by meaning — the "why / what" (decisions, conventions) |
| `get_graph_node` | Full content of one node (qn) PLUS its relations both ways (calls / called_by / imports) |
| `get_graph_risk` | LOW/MEDIUM/HIGH change-risk triage for one symbol |
| `get_graph_impact` | Blast radius of changing one or more symbols |
| `get_graph_stats` | Node/edge counts, languages, embedded count, and the map's router summary + source (explicit/readme/stats) |
| `update_code_graph` | Re-index changed files into the live graph (after edits, if no hook ran) |
| `recall`, `remember`, `forget` | Agent notes in the notes map (`remember` and `forget` need `-writable`) |

Search tools take `query`, optional `limit`, and optional `map` (aliases `repo`/`project`) to scope which graph is searched; omit `map` and the query is routed semantically. All read tools are read-only; only `update_code_graph`, `remember` and `forget` mutate.

### Retrieval pattern (do this)

1. **Search lean to locate**: `search_graph` / `search_graph_docs` — cheap
   pointers ranked by score (qn, file:line, short preview). Replies are lean by
   design; `limit` is the only size knob (default 5). Don't raise `limit` just
   to *find* something.
2. **Fetch the one hit**: `get_graph_node <qn>` for the full body. For a **Doc**
   qn this returns the **whole document in one call** — don't fetch its sections
   one by one.
3. **Traverse** with `get_graph_node` (its relations) or `get_graph_impact` (blast radius) using a qn from a hit.
4. `get_graph_stats` is a diagnostic for counts — not a warm-up before every task.

### Notes

- Search replies are intentionally lean (short previews, default 5 results); use `get_graph_node` for full detail.
- A `Doc` search hit is a section map (`is_summary: true`), not prose — read it with one `get_graph_node` call.
- Qualified names are repo-namespaced: `repo//path::Symbol` (e.g. `xray//internal/xray/voice.go::VoiceManager.loadSessionConfig`). Pass `repo` to scope a search when multiple repos are served.
- Run it with `make graph-serve` (or `CGO_ENABLED=1 go run ./cmd/xray graph serve -repo xray=$(pwd)`). This is the SAME binary as the server, built with CGO so it can parse (see `docs/graph.md`, `docs/graph-pipeline.md`). Needs a llama.cpp-compatible embeddings endpoint (default `http://localhost:8090`).
