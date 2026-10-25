# ADR-005: CellSyncer — CR to Memory Sync

**Status:** Implemented  
## Role

`CellSyncer` (`internal/xray/cellsync.go`) is the bridge between `AwarenessMemoryCell` Kubernetes CRs and the runtime `MemoryStore`. It runs in two directions:

- **CR → Memory**: at startup and on CR changes (via controller-runtime).
- **Memory → CR**: when MCP `upsert_memory` or `delete_memory` is called (via `Persist()` / `Remove()`).

## Startup: `SeedAll(namespace)`

Called once at startup. Lists all `AwarenessMemoryCell` CRs in the namespace. For each:
- Skips if `spec.status == "stale"` or `"superseded"`.
- Calls `syncCell()`.

## `syncCell(cell)`

1. If `spec.embedding` is present and valid: calls `memory.SeedFromDoc()` with the decoded vector — **no embedder API call**, fast.
2. If embedding is missing or invalid and embedder is configured: calls `cells.Index()` to embed, patches `spec.embedding` and `status.indexed=true` back to the CR, then calls `memory.seedItemDirect()`.
3. If no embedder (keyword mode): calls `memory.seedItemDirect()` — adds to items map, no HNSW indexing.

## Runtime: `Reconcile(req)`

Registered as a controller-runtime reconciler for `AwarenessMemoryCell`. Triggered on create/update/delete. Calls `syncCell()` for the changed CR. Retries after 30s on error.

## `Persist(it, namespace)`

Writes a `MemoryItem` to the cluster as an `AwarenessMemoryCell` CR.
- CR name: `cellResourceName(id)` = sanitised ID + `-` + 8-char SHA1 suffix.
- On create: sets labels `triageagent.dev/managed=true` and `triageagent.dev/kind=<kind>`. Sets `status.indexed=true` and `status.lastIndexedAt`.
- On update: preserves existing `spec.embedding` so the reconciler won't re-embed unnecessarily. Updates `spec.updatedAt`.
- Runs in a goroutine from MCP handler — fire and forget with warn log on error.

## `Remove(id, namespace)`

Deletes the CR by computing `cellResourceName(id)`. No-op if CR not found.

## CR name format

`cellResourceName(id)`:
- Lowercases, replaces non-alphanumeric with `-`, trims leading/trailing dashes.
- Appends `-` + first 8 chars of SHA1(original id).
- Max base length: 244 chars (leaving room for suffix within 253-char k8s name limit).
- Example: `aa6556af6f2e1486` → `aa6556af6f2e1486-5bd2fd3a`.

## Graceful degradation

If no k8s client is available (local dev, no kubeconfig): `CellSyncer` is nil. MCP still works — items are in-memory only, not persisted to CRs. `NewMCPServer()` accepts `syncer=nil` and simply skips `Persist()`/`Remove()` calls.
