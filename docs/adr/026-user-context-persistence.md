# ADR-026: User Context Persistence: Pinned Panels & Agent Notes

**Status:** Implemented  
**Date:** 2026-05-24  

## Context

After introducing the floating panel system (ADR-025), two gaps remained:

1. **Pinned panels were ephemeral** — closing the browser lost them; users had to re-pin items each session.
2. **Agent notes were invisible** — notes written by X-Ray during voice sessions (kind=`agent_note`) existed in memory only and were not surfaced anywhere in the UI.

## Decision

### Pinned panel persistence — localStorage (per user)
Pinned item IDs are saved to `localStorage` under the key `xray.pinned.<username>` whenever the `pinned` state changes. On login (when `username` becomes available), IDs are restored and panels are laid out in a grid starting top-left. No backend change is required.

Rationale: pinned state is personal and device-local. Cross-device sync adds complexity with no clear benefit — if the user wants items on a new device, they re-pin. localStorage survives page refreshes and tab restores.

### Agent notes as graph nodes
A new backend endpoint `GET /v1/user/notes` returns `agent_note` items for the logged-in user (resolved via session cookie or bearer token), filtered by `subject=userHash` (SHA-256[:16] of the raw SSO ID). The frontend fetches this on login and passes the notes as `extraItems` to `MemoryGraph`. Notes appear as orange nodes in the inner ring (tier 0.40, top sector) of the Voronoi graph, distinct from all other kinds.

Rationale: agent notes are personal context. Showing them on the graph makes them discoverable without a dedicated panel or sidebar. The inner ring placement signals "personal / ephemeral" vs outer-ring "shared knowledge".

**Important:** Notes are only returned by `/v1/user/notes` if their `subject` matches the authenticated user's `userHash` (SHA-256[:16] of the raw SSO ID). Notes with no subject are invisible to all users via this endpoint. A note that appears pinned on the graph for a user is not necessarily returned by `/v1/user/notes` — it may be visible solely because its item ID is saved in `localStorage` under `xray.pinned.<username>` (pinned panel persistence). The pin restores the panel on login regardless of subject scoping.

### Pin bug fix — id passed from panel
`onPin` callback signature changed from `(x, y)` to `(id, x, y)` where `id` is the currently-viewed item inside the panel (which may differ from the panel's initial `id` after internal navigation via SectionList). This ensures the correct item is pinned regardless of in-panel browsing.

### Magnet snap on drag
When dragging a panel within 20px of another panel's edge, the dragged panel snaps to align (left-to-right, right-to-left, or top-aligned). Implemented by querying `[data-panel]` elements at drag time and applying snap thresholds. No state needed — purely positional.

### Agent note CR persistence (follow-up)

Agent notes were initially ephemeral — lost on pod restart. Three changes made them durable:

1. **`defNoteObservation` calls `syncer.Persist`** after writing to the in-memory store, creating a `MemoryCell` CR with `kind=agent_note` and `subject=userHash`.
2. **`syncCell` always loads `agent_note` CRs into `s.items`** at startup (without HNSW indexing), regardless of whether a k8s cache reader is present. Previously this was gated on `!hasReader` — but `Upsert` always writes/reads `s.items`, never the k8s cache reader. Without seeding, search (via reader) could find a note but `Upsert` would create a duplicate because the ID wasn't in `s.items`.
3. **`SeedAll` counts `agent_note` cells** as seeded in the startup log (previously silently skipped alongside `system_configuration`).

Agent notes are intentionally excluded from the HNSW vector index and the keyword trigram index — they are found only via explicit `List("agent_note", ...)` or through the personal note merge in `search_memory`.

### Personal notes included in `search_memory`

`search_memory` now merges agent notes for the authenticated user alongside shared knowledge results. When `kind=""` (default) or `kind="agent_note"`, a second `KeywordSearch` pass runs with `subject=user` and results are deduped before returning. The merge is bounded (minimum 3 note slots, capped at `limit − shared_results`).

This was the natural consequence of having a stable `userHash` identity in the voice session: notes written for the user can now be recalled by name, just like any other memory item.

### `delete_memory` gating — user-scoped, not write-privileged

`delete_memory` was originally placed in the `canWrite` block (gated by `writeAllowedUsers`). This was wrong: deleting a personal note is a user-scoped action, not a knowledge-base write. It is now in the `userHash != ""` block alongside `get_agent_notes` and `get_pinned_items` — any authenticated user can delete their own notes without appearing in `writeAllowedUsers`.

`delete_memory` also now calls `syncer.Remove` to delete the MemoryCell CR, so notes do not reappear after pod restart.

The tool description steers the model to call `get_agent_notes` first to obtain the note ID before calling `delete_memory`, preventing it from searching with `search_memory` (which may not surface notes if the query doesn't keyword-match).

### Agent note subject fix

`defNoteObservation` and `defGetAgentNotes` were using `subject` (the org subject passed via `?subject=` query param — always empty from the frontend voice WebSocket) instead of `userHash` (SHA-256[:16] of the authenticated user's SSO ID). Notes were stored with blank subject, so `/v1/user/notes` never matched them and the graph stayed empty. Fixed by passing `userHash` to all note tools.

### Live graph update via SSE

`memory.go` was skipping `PublishChange` for `agent_note` on upsert, so the SSE stream never notified the frontend. Fixed: SSE fires for `agent_note` upserts (keyword index still skipped). `page.tsx` subscribes to SSE change events and patches `userNotes` state immediately — notes appear as graph nodes without page reload.

## Consequences

- **No backend schema change** — `agent_note` items already existed; only a new read endpoint is added.
- **localStorage is browser-local** — pins lost on different browser/device (accepted trade-off).
- **Notes persist as CRs** — survive pod restarts; `subject=userHash` scopes them to the individual user.
- **Notes visible on graph live** — appear as nodes immediately after Sym creates them via SSE; visible on all subsequent logins from CR.
- **Notes appear in `search_memory`** — Sym can recall personal notes by keyword alongside shared knowledge.
- **`delete_memory` for all users** — any authenticated user can delete their own notes; write privilege not required.
- **`/v1/user/notes`** returns `[]` (not 401) when auth is disabled, so the endpoint is always safe to call.

## Revert path

To revert to pre-ADR-026 state:
1. Remove the `GET /v1/user/notes` handler from `internal/xray/server.go` (added in `registerAuthRoutes`).
2. Remove `fetchUserNotes` from `ui/lib/api.ts`.
3. Remove `extraItems` prop from `MemoryGraph` and the `allItems` memo; restore `filteredItems` to use `items` directly.
4. Remove `agent_note` entries from `KIND_COLOR`, `KIND_TIER`, `KIND_LABEL`, `KIND_ANGLE` in `memory-graph.tsx`.
5. Revert `onPin` signature back to `(x, y)` in `memory-item-panel.tsx` and `page.tsx`.
6. Remove the localStorage save/restore effects and helper functions from `page.tsx`.
7. Remove `data-panel=""` attribute and snap logic from `memory-item-panel.tsx`.
8. Revert `defNoteObservation` to not call `syncer.Persist` (remove syncer/namespace params); restore `subject` param.
9. Revert `syncCell` to return early for `agent_note` (restore original `system_configuration || agent_note` guard).
10. Revert `defSearchMemory` to single-pass search (remove user param and agent_note merge block).
11. Revert `defGetAgentNotes` param from `userHash` back to `subject`.
12. Move `defDeleteMemory` back into the `canWrite` block; remove `syncer.Remove` call.
13. Revert `memory.go` `Upsert` to skip `PublishChange` for `agent_note`.
14. Remove SSE `agent_note` subscription effect from `page.tsx`.
