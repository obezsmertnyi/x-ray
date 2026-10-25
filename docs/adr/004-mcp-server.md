# ADR-004: MCP Server — Tools, Auth, and Persistence

**Status:** Implemented  
## Overview

The MCP server (`internal/xray/mcpserver.go`) is an optional HTTP server (default `:8083`, disabled if `--mcp-address` is empty) exposing four tools via the Model Context Protocol for external LLM agents.

## Tools

### `get_memory_context`
- Params: `subject` (required)
- Returns all active items for the subject via `memory.AllItems(subject)`.
- Excludes `system_configuration` items.
- No search — full dump of all items for the subject.
- The old name `get_knowledge_context` remains registered as a deprecated alias for backward compatibility.

### `search_memory`
- Params: `subject` (required), `query` (required), `kind` (optional), `limit` (optional, default 10)
- Calls `memory.Search(ctx, query, kind, subject, limit)`.
- **Requires non-empty query** — empty query returns nil (see ADR-001).
- In semantic mode: uses HNSW vector search. In keyword mode: substring match on title/content.
- The old name `search_knowledge` remains registered as a deprecated alias for backward compatibility.

### `upsert_memory`
- Params: `kind`, `scope`, `title`, `content` (all required); `id`, `subject`, `confidence`, `event_date`, `image_data`, `image_mime`, `tags` (optional).
- Calls `memory.Upsert()` → item in memory map.
- If `image_data` present: downscales to 480×320 JPEG, embeds as `![thumbnail](data:...)\n\n` prefix in `content`. `image_data` field is then cleared (thumbnail lives in content only).
- If `syncer != nil`: calls `syncer.Persist()` in a goroutine → CR written to cluster. **Persists across pod restarts.**
- If `syncer == nil` (no k8s): in-memory only, lost on restart.

### `delete_memory`
- Params: `id` (required)
- Calls `memory.Delete(id)` → removes from memory map.
- If `syncer != nil`: calls `syncer.Remove()` in a goroutine → deletes CR from cluster.

## Authentication

- If `--mcp-token-secret` is set, the Secret's `token` field is loaded and watched for changes.
- Every request checks `Authorization: Bearer <token>`. Returns 401 if missing or wrong.
- If token is empty string, auth is skipped (open access).
- Token is stored in an `atomic.Value` and hot-reloaded from the k8s Secret watcher — no restart needed.

## Image handling detail

The MCP server does **not** store `image_data` as a separate CR field. Instead:
1. Downscales image to max 480×320, re-encodes as JPEG quality 82.
2. Prepends `![thumbnail](data:image/jpeg;base64,<b64>)\n\n` to `content`.
3. Clears `image_data` and `image_mime` before calling `Upsert()`.
4. The thumbnail travels inside `spec.content` in the CR — survives updates without re-uploading.
5. `stripThumbnail()` removes the prefix before embedding so thumbnail bytes don't pollute the vector.

## Use `scripts/upsert_memory.py` for images

The Python script uses the same embedding approach. Calling `upsert_memory` MCP tool directly with `image_data` works for new items but on **content-only updates** the thumbnail is preserved from `content` — do not pass `image_data` again unless you want to replace it.
