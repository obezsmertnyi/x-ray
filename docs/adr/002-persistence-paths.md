# ADR-002: Persistence Paths — What Survives a Pod Restart

**Status:** Implemented  
**Superseded:** see "Revision history" below  
Several tools write memory items. They share one rule: a write **survives a pod
restart (and is visible to other replicas)** if and only if its handler flushes
to a `MemoryCell` CR via `CellSyncer` — `Persist()` for upserts, `Remove()` for
deletes — and a `CellSyncer` (k8s client) is wired (`syncer != nil`). When k8s
is unavailable (local `--dev`), `syncer == nil` and every write is in-memory
only.

`CellSyncer.Persist()` does **not** filter by scope or kind: it persists
whatever `MemoryItem` it is handed, including private `note` items. So a private
voice note is written to a CR just like a public cell.

## Write paths

### MCP `upsert_memory` → persists to CR

1. `mcpserver.go`: `upsert_memory` calls `memory.Upsert()` → item in memory map.
2. If `syncer != nil`: `go syncer.Persist()` → creates/updates the CR. The
   error is **logged** (`Warn`), not awaited.
3. **Survives restart**: yes — CR is loaded by `CellSyncer.SeedAll()` at startup.
4. Image handling: `image_data` is downscaled to 512×512 JPEG, embedded as a
   `![thumbnail](data:...)` prefix in `content`, stored in the CR's
   `spec.content`.

### MCP `delete_memory` → removes CR

1. `mcpserver.go`: `delete_memory` calls `memory.Delete()` → removes from map.
2. If `syncer != nil`: `go syncer.Remove()` → deletes the CR. Error is logged.

### Voice `upsert_memory` → persists to CR

1. `voice_tools.go` (`defUpsertMemory`): `memory.Upsert()`, then if
   `syncer != nil` `go syncer.Persist()`. Error is suppressed (`//nolint:errcheck`).
2. Private writes are forced to the caller's `subject = userHash` (ADR-028/037).
3. **Survives restart**: yes (when `syncer != nil`).

### Voice `note_observation` → persists to CR

1. `voice_tools.go` (`defNoteObservation`): `memory.Upsert()` of a private
   `note` (scope=private, subject=userHash), then if `syncer != nil`
   `go syncer.Persist()`. Error is suppressed.
2. **Survives restart**: yes (when `syncer != nil`). *This corrects the original
   ADR, which claimed `note_observation` never persisted.*
3. This tool is controlled by the `system_configuration` cell tags — remove
   `note_observation` from tags to disable it.

### Voice `promote_note` → persists new cell; deletes source in-memory only

1. `voice_tools.go` (`defPromoteNote`): reads the source note, creates a new
   cell via `memory.Upsert()` + `go syncer.Persist()`, then `memory.Delete(noteID)`
   and `go syncer.Remove(noteID)`.
2. Because `note_observation` persists the source note to a CR, the source must
   be removed from **both** the in-memory store and the CR — otherwise the source
   CR is orphaned and reseeds on the next `SeedAll()`/reconcile. Fixed 2026-06-14
   (see "Resolved issue" below).

### Voice `delete_memory` → removes CR

1. `voice_tools.go` (`defDeleteMemory`): owner-gated (private scope +
   `subject == userHash`), then `memory.Delete()` and if `syncer != nil`
   `go syncer.Remove()`. Error is suppressed.

## Path: `CellSyncer` (CR → memory) — read-only from memory's perspective

1. At startup: `SeedAll()` lists all `MemoryCell` CRs, calls `syncCell()` for each.
2. `syncCell()`:
   - If CR has `spec.embedding`: `memory.SeedFromDoc()` with the stored vector —
     no embedder API call.
   - If no embedding and an embedder is configured: `cells.Index()`, patch the
     embedding back to the CR, then `memory.seedItemDirect()`.
   - If no embedding and no embedder: `memory.seedItemDirect()` (keyword mode).
3. At runtime: `CellSyncer.Reconcile()` handles CR create/update/delete via
   controller-runtime.
4. `CellSyncer.Remove()` deletes a CR by item ID.

## CR name derivation

`cellResourceName(id)`:
1. Lowercases the ID, replaces non-alphanumeric chars with `-`.
2. Appends `-` + first 8 hex chars of SHA1 of the original ID.
3. Example: ID `aa6556af6f2e1486` → CR name `aa6556af6f2e1486-5bd2fd3a`.

## Summary table

| Write path | In-memory | CR persisted | Survives restart |
|---|---|---|---|
| MCP `upsert_memory` (syncer present) | yes | yes | yes |
| MCP `upsert_memory` (no k8s) | yes | no | no |
| MCP `delete_memory` (syncer present) | yes (deleted) | yes (deleted) | — |
| Voice `upsert_memory` (syncer present) | yes | yes | yes |
| Voice `note_observation` (syncer present) | yes | yes | yes |
| Voice `promote_note` (syncer present) | new: yes / source: deleted | new: yes / source: removed | new: yes / source: gone |
| Voice `delete_memory` (syncer present) | yes (deleted) | yes (deleted) | — |
| Any write (no k8s, `--dev`) | yes | no | no |
| `CellSyncer.SeedAll` (startup) | yes | source | yes |

## Implications for horizontal scaling (multiple replicas)

The CR is the only cross-replica medium; the in-memory store and HNSW index are
**per-pod**. Two consequences:

1. **Eventual consistency, not immediate.** A write on pod A is visible to pod B
   only after `Persist()` lands the CR *and* B's `CellSyncer` watch fires and
   re-indexes. "Write on A, read on B" is not read-your-writes across replicas.
2. **Best-effort durability.** Every flush is a detached goroutine whose error is
   logged (MCP) or suppressed (voice) and never awaited. If a pod dies between
   `memory.Upsert()` and the CR write completing, that write is lost and exists
   on no replica.

Runtime state that is **never** persisted and stays strictly per-pod: the SSE
hub (`memory-hit`/`memory-change` only reach clients on the same replica) and
`MemoryStore` stats/usage counters. Browser OAuth sessions are persisted and
replica-safe via the `system_configuration` CR status HMAC (ADR-047) — they are
*not* a scaling blocker.

## Resolved issue

~~`promote_note` should call `syncer.Remove(noteID)` after deleting the source
note in-memory, to delete the orphaned source-note CR.~~ **Fixed 2026-06-14**:
`defPromoteNote` now calls `go syncer.Remove(noteID, namespace)` after
`memory.Delete(noteID)` (mirroring `defDeleteMemory`), so promoting a note no
longer leaves its original CR behind. Test: `TestPromoteNote_DeletesSourceNote`.

## Revision history

- **2026-06-14**: Corrected `note_observation` (now persists to CR), added the
  voice `upsert_memory` / `promote_note` / `delete_memory` and MCP
  `delete_memory` paths, and added the horizontal-scaling implications section.
  Same-day follow-up: fixed the `promote_note` orphaned-CR bug (it now calls
  `syncer.Remove`) and moved it from "Open issue" to "Resolved issue".
