# ADR-016: MCP Server Tool Reference

**Status:** Implemented  
## Context

X-Ray exposes an MCP (Model Context Protocol) server on `:8083` for external LLM
agents to access and write memory. This ADR documents all available tools, their
parameters, and key behaviours so integrators do not need to read the source.

## Tools

### `get_knowledge_context`

Load knowledge items by subject or fetch a single item by ID.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `subject` | string | — | Subject name (e.g. `xray`) — returns all items for the subject |
| `id` | string | — | Item ID — returns that single item |

- Pass `id` to fetch one item; pass `subject` to list all items for a subject.
- At least one of `id` or `subject` should be provided.
- Returns full item content including `imageData` if present.
- Large subjects (40+ cells) will return large payloads — prefer `search_knowledge` for targeted queries.

### `search_knowledge`

Search the knowledge base by keyword or semantic query.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `query` | string | yes | Search terms |
| `subject` | string | conditional | Optional subject filter. **Required when `kind=agent_note`** (notes are tenant-isolated by `subject=userHash`). Leave empty to search shared knowledge across all subjects. |
| `kind` | string | — | Filter: `stable_fact`, `recent_change`, `system_identity`, `event`, `agent_note` |
| `limit` | number | — | Max results (default 10) |

- **Empty `subject` searches across all subjects** — required so per-author blog posts (`subject` = author name) and contributors (`subject` = person name) are reachable. A required `subject=xray` previously hid them.
- `kind=agent_note` with an empty `subject` is **rejected** (`{"error":"subject is required when kind=agent_note"}`): an unscoped note search would return every user's notes (ADR-028). The storage layer does not scope `agent_note` by itself — the tenant boundary is enforced at this call site.
- For non-note kinds: uses semantic HNSW search when an embedder is configured, keyword fallback otherwise.
- `system_configuration` is never returned.

### `upsert_memory`

Create or update a memory item.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `id` | string | — | Existing item ID to update; omit to create new |
| `kind` | string | yes | `stable_fact`, `recent_change`, `system_identity`, `event`, `agent_note` |
| `scope` | string | yes | `global`, `domain`, `service`, or custom |
| `title` | string | yes | Short title |
| `content` | string | yes | Full body in Markdown |
| `subject` | string | — | Author or subject tag |
| `confidence` | string | — | `high`, `medium`, `low` |
| `event_date` | string | — | RFC3339 date — required for `kind=event` |
| `tags` | array | — | String tags |

- `system_configuration` is rejected.
- `agent_note` items are ephemeral — stored in-process only, not persisted to CRs, lost on pod restart.
- Images must be embedded as a `![thumbnail](data:image/jpeg;base64,...)` prefix in `content` — use `scripts/upsert_memory.py` for upserting with images (MCP direct call drops image data on content update).
- When a `CellSyncer` is wired, non-`agent_note` items are also persisted to `MemoryCell` CRs asynchronously.

### `delete_memory`

Delete a memory item by ID.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `id` | string | yes | Item ID to delete |

- Returns `{"error":"not found"}` if ID does not exist.
- If a `CellSyncer` is wired, also removes the corresponding CR asynchronously.

## Authentication

Bearer token auth is enforced when `--mcp-token-secret` is configured.
Pass `Authorization: Bearer <token>` on all requests.
If no token secret is set, the server is unauthenticated.

## Kind × Behaviour Matrix

| `kind` | Persisted to CR | Searchable via semantic | Subject-filtered | Shown in UI |
|--------|----------------|------------------------|-----------------|-------------|
| `stable_fact` | yes | yes | yes | yes (Blog) |
| `recent_change` | yes | yes | yes | yes (News) |
| `system_identity` | yes | yes | yes | yes (Projects/Team) |
| `event` | yes | yes | yes | yes (Events) |
| `agent_note` | **no** | keyword only | **no** | **no** |
| `system_configuration` | yes | **blocked** | yes | **no** |
