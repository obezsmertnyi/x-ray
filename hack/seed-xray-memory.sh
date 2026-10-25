#!/usr/bin/env bash
# Seed X-Ray's own knowledge into its memory over MCP, using the standard
# schema: kind + tags + scope (no domain "type" field — domain typing is a tag
# convention the UI maps, the backend stays standard).
#
# kinds:  stable_fact (facts/blog) · system_identity (projects/team, +project/
#         contributor tag) · recent_change (news/changes) · event
# scope:  public (shared, on the graph) | private (isolated to subject)
#
# Usage: XRAY_MCP_URL=http://localhost:8083/mcp XRAY_MCP_TOKEN=... ./hack/seed-xray-memory.sh
# Re-run after `kubectl delete mcell --all -n xray` for a clean set.
set -euo pipefail

B="${XRAY_MCP_URL:-http://localhost:8083/mcp}"
TOK="${XRAY_MCP_TOKEN:-dev-admin-token-xray}"
HDR=(-H "content-type: application/json" -H "accept: application/json, text/event-stream" -H "authorization: Bearer ${TOK}")

SID="$(curl -s -D - -o /dev/null -X POST "$B" "${HDR[@]}" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"seed","version":"1"}}}' \
  | tr -d '\r' | awk -F': ' 'tolower($1)=="mcp-session-id"{print $2}')"
[ -n "$SID" ] || { echo "no session id — is the MCP server up at $B?" >&2; exit 1; }
HDR+=(-H "mcp-session-id: ${SID}")

n=0
# seed <kind> <scope> <title> <tags-csv> <content>
seed() {
  local kind="$1" scope="$2" title="$3" tags_csv="$4" content="$5"
  local args
  args=$(python3 -c '
import json,sys
tags=[t for t in sys.argv[4].split(",") if t]
print(json.dumps({"kind":sys.argv[1],"scope":sys.argv[2],"title":sys.argv[3],
  "tags":tags,"subject":"xray","confidence":"high","content":sys.argv[5]}))' \
  "$kind" "$scope" "$title" "$tags_csv" "$content")
  curl -s -X POST "$B" "${HDR[@]}" \
    -d "{\"jsonrpc\":\"2.0\",\"id\":$((++n)),\"method\":\"tools/call\",\"params\":{\"name\":\"upsert_memory\",\"arguments\":$args}}" \
    | tr -d '\r' | python3 -c 'import sys,json; d=json.load(sys.stdin); r=d.get("result",{}).get("content",[{}])[0].get("text",""); print("  ✓",json.loads(r).get("title", r) if r.startswith("{") else r)' 2>/dev/null \
    || echo "  ? $title (parse)"
}

echo "Seeding X-Ray memory → $B"

# ── Projects (system_identity + project tag) ─────────────────────────────────
seed system_identity public "X-Ray" "project,knowledge,voice" \
"X-Ray is an agentic controller runtime that thinks, remembers, and speaks: a single Go binary embedding a backend API and a Next.js UI, with HNSW semantic memory, GitHub/Entra OAuth, a Gemini Live voice layer, and an optional MCP server. Kubernetes-native, degrades gracefully without k8s. Module: github.com/triageagent-dev/x-ray."

# ── Facts / blog (stable_fact) ───────────────────────────────────────────────
seed stable_fact public "Architecture — backend" "architecture,go" \
"Go backend: one net/http server on :8080 (Huma/OpenAPI). internal/xray: server.go (routes, sessions, SSE), voice.go (Gemini Live), voice_tools.go (memory tools), memory.go (in-memory HNSW store), cellsync.go (MemoryCell CR sync), mcpserver.go (MCP), auth.go (OAuth sessions). vectorstore/ holds embeddings + HNSW."

seed stable_fact public "Architecture — frontend & graph" "architecture,ui" \
"Frontend: Next.js 16, static-exported and embedded in the binary. The index is a D3 Voronoi memory graph (radial placement by kind, live SSE hit highlighting). Native /k/<kind> routes list memory; a cosmetic config maps kinds/tags to display categories. Voice UI at /avatar."

seed stable_fact public "Memory model — kind, tags, scope" "architecture,memory" \
"Memory cells carry: kind (stable_fact/system_identity/recent_change/event/note), a flat tags list, and scope (public=shared/on the graph, private=isolated to subject — ADR-040). Domain categories (Projects, Team) are a UI mapping over kind+tag (e.g. system_identity + project tag → Projects); the backend stays a standard generic store. Personal notes are kind=note, scope=private, subject=userHash."

seed stable_fact public "Voice layer (Gemini Live)" "architecture,voice" \
"VoiceManager opens a Gemini Live audio session on the /v1/voice WebSocket; system prompt + tool allowlist load from a system_configuration cell at session start. Memory tools (search_memory, note_observation, upsert_memory, …) run server-side; results return via SendToolResponse. Vertex AI (--gcp-project) or Google AI (--gemini-api-key)."

seed system_identity public "MCP server" "service,mcp" \
"Optional MCP server on :8083 (--mcp-address): search_knowledge, get_knowledge_context, list_memory, upsert_memory, delete_memory. Bearer-token (fails closed) or OAuth 2.1 (ADR-034). External agents — including Claude Code — read and write X-Ray memory through it."

# ── How-to (stable_fact tagged howto) ────────────────────────────────────────
seed stable_fact public "How to build and run" "howto,build" \
"make build builds the full binary with embedded UI; make run runs the backend; make run-ui runs the Next.js dev server (NEXT_PUBLIC_API_URL=http://localhost:8080). make test runs go test ./...; make lint runs gofmt + go vet. Pass --dev to disable auth locally."

seed stable_fact public "How to contribute (ADR workflow)" "howto,contributing" \
"ADRs live in docs/adr/, work items in docs/todo/, each with a README status table. Before implementing an ADR, read its decision section and verify against the code. When implementing, do it in ONE commit: code + CHANGELOG entry + ADR header (Status: Implemented, Commit) + matching todo marked Done + both README tables updated. PRs: one logical change; go test and make lint must pass."

seed stable_fact public "How to deploy" "howto,deploy" \
"Deploy via Helm: helm upgrade --install xray ./charts/xray -n xray --set image.tag=<version>. make push builds+pushes the image and updates Chart.yaml. Key flags: --gcp-project (Vertex/voice), --embedding-endpoint, --mcp-address, --mcp-token-secret, --public-url (required in prod), --otel-endpoint."

# ── Recent changes / state (recent_change) ───────────────────────────────────
seed recent_change public "Scope isolation (ADR-040)" "state,security" \
"Memory now has an enforced public/private scope axis (ADR-040): private cells are isolated to their subject — excluded from shared search and the public graph, returned only when the query is scoped to the owner. Replaced the old agent_note kind: personal notes are kind=note + scope=private + subject=userHash."

seed recent_change public "MCP OAuth 2.1 (ADR-034)" "state,mcp,security" \
"X-Ray is its own co-located OAuth 2.1 authorization server for MCP: dynamic client registration, PKCE authorize→token, audience-bound RS256 JWTs with per-tool scopes (memory.read/write). The static --mcp-token-secret remains a full-scope fallback; the endpoint fails closed when no credential is configured."

seed recent_change public "Public-exposure hardening (ADR-032)" "state,security" \
"Security hardening pass: MCP auth fails closed (503, constant-time compare); voice WebSocket validates Origin (anti Cross-Site WebSocket Hijacking); image endpoint allowlists raster MIME + nosniff (stored XSS); OAuth state bound to the browser via signed cookie (login CSRF); security headers (CSP/X-Frame-Options/HSTS); per-key rate limiting + --max-concurrent-voice cap; request-body + item-size limits; default log level moved to info."

seed recent_change public "Per-user token quota + OTEL metrics" "state,voice,observability" \
"Per-user daily Gemini Live token quota (ADR-031): sessions are rejected at the WebSocket upgrade once an authenticated user exceeds a per-UTC-day cap; usage persists to the system_configuration CR and survives restarts; the HUD shows the caller their own quota. OpenTelemetry GenAI metrics (ADR-007) are exposed on a Prometheus /metrics endpoint (TTFT, TPOT, tool/session/search duration) for latency SLOs."

seed recent_change public "Unified hybrid search (ADR-030)" "state,search" \
"Search was unified into one lexical scorer plus Reciprocal Rank Fusion for semantic: the trigram index is a ranking signal, not a hard veto; coverage (not raw score) decides confidence, so a partial name match skips semantic fusion. Several fuzzy-match bugs found via live MCP testing were fixed (rune-based edit distance, CamelCase mishears, category-word query noise)."

# ── ADRs / todos / how the project records decisions (stable_fact) ────────────
seed stable_fact public "Architecture Decision Records (ADRs)" "process,adr" \
"X-Ray's design history is in docs/adr/ as numbered ADRs, each with a Status (Reference/Accepted/Proposed/Implemented/Superseded); docs/adr/README.md is the index. Work items live in docs/todo/ with their own status table. The core runtime (ADRs 001–034) covers the memory store, voice, MCP, OAuth, hybrid search, and public-exposure hardening."

seed stable_fact public "Implementation state — overall" "state" \
"Implemented: the core runtime (ADRs 001–034) — HNSW memory store, Gemini Live voice, MCP server, GitHub/Entra OAuth, hybrid search (ADR-030), public-exposure hardening (ADR-032), per-user token quota (ADR-031), OTEL metrics (ADR-007). Scope public/private isolation (ADR-040) is in. Memory is kind + tags + scope; domain categories (Projects/Team) are a UI mapping over kind+tag, not a backend field."

# ── Conventions for working in this repo (stable_fact tagged convention) ─────
seed stable_fact public "Deploy order: commit, push, helm" "convention,deploy" \
"Deploy in this order: 1) git commit (so the version string embeds the fix commit hash); 2) make push (builds the image, prints the version e.g. v1.13.4-c7406cf); 3) helm upgrade xray charts/xray --reuse-values -n xray --set image.tag=<version>. Always pass --set image.tag explicitly — --reuse-values does NOT pick up the new Chart.yaml tag, so omitting it deploys a stale image. Never push before committing."

seed stable_fact public "Kubernetes namespace is xray" "convention,deploy" \
"Every X-Ray k8s resource lives in the xray namespace: Helm release, pod, ServiceAccount, RBAC, MemoryCell CRs, Secret watch, MCP token secret. kubectl/helm commands use -n xray."

seed system_identity public "Contributor / team-member format" "person,convention" \
"To add a team member to the Team view, create kind=system_identity with a 'contributor' tag, subject = the person's full name (not 'xray'), scope=public. The UI filters the Team view by the contributor tag; Projects use the same kind with a 'project' tag instead. (Domain category = kind + tag; the backend stays a generic store.)"

seed stable_fact public "Single commit: code + changelog + ADR/todo" "convention,contributing" \
"Never split a code change from its CHANGELOG.md entry — they go in one commit. When implementing an ADR, the same commit updates: the code, CHANGELOG.md, the ADR header (Status: Implemented + Commit), the matching docs/todo file (Done), and both docs/adr/README.md and docs/todo/README.md tables (status, commit, test names)."

seed stable_fact public "Run voice evals with /run-evals" "convention,evals" \
"Evaluate X-Ray voice sessions with the /run-evals skill, which maps to scripts/eval_voice_sessions.py (Phoenix traces + Vertex AI as judge). Do not use google-agents-cli-eval for X-Ray — that targets ADK agents, a different system."

seed stable_fact public "Upsert with an image: use upsert_memory.py" "convention,memory" \
"For a memory upsert that includes an image thumbnail, use scripts/upsert_memory.py — a direct MCP upsert_memory call drops image data on a content update. Embed the thumbnail as a data URI in the content (![thumbnail](data:image/jpeg;base64,...)); downscale with sips first."

seed stable_fact public "Release cadence" "process,release" \
"Versions are computed from git tags as v<major>.<minor>.<commits-since-tag>-<short-commit>; make push stamps the image and updates Chart.yaml. The CHANGELOG.md [Unreleased] section holds in-flight work."

echo "Seeded $n cells."
