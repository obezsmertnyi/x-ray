# ADR-029: Collective Consciousness — Shared SSE Graph Highlighting

**Status:** Implemented
**Date:** 2026-05-26
## Context

When multiple users are simultaneously exploring the knowledge graph or talking to X-Ray via voice, the X-Ray Voronoi graph highlights nodes in real time as memory items are accessed. This highlighting is driven by `MemoryHitEvent` messages broadcast over a single shared SSE hub to all connected browser tabs.

The question arose: is this cross-user visibility a side effect to fix, or a feature to keep? And does it create any privacy or context-bleed issues in the voice layer?

## Decision

**Keep the shared broadcast as-is.** The collective consciousness effect — where all viewers see the same memory activations light up simultaneously — is desirable behaviour that makes the shared knowledge graph feel alive. It reflects X-Ray's nature as a *shared* intelligence, not a private per-user tool.

## Consequences

### What is shared (intentional)

- `MemoryHitEvent` carries `itemIds`, `query`, and `kind` and is broadcast from `internal/xray/hub.go` to every SSE subscriber, regardless of which user or voice session triggered it.
- The `hitCounts` accumulator (`Hub.hitCounts`) is global across all users — demand ranking of graph nodes reflects collective access patterns, not per-user activity.
- Any SSE client that parses the raw event stream can read the `query` string from another user's search.

### What is NOT shared (private)

Voice session context is fully isolated per user:

| Concern | Isolation mechanism |
|---------|---------------------|
| Gemini Live session | One `genai.Session` per WebSocket connection |
| System prompt | Built per-session; injects `ghUser` for the specific caller |
| `agent_note` search results | Scoped to caller's `userHash` (SHA-256[:16] of SSO ID) |
| `get_agent_notes` | Filtered to caller's `userHash` |
| `pinnedIDs` | Per-session field under `pinnedMu` mutex |
| Write permission | Checked against `writeAllowedUsers` per session |
| `note_observation` / `delete_memory` | Enforced to creator's `userHash` at write and delete time |

There is no Gemini context bleed between users. Voice sessions are fully independent.

### The one genuine privacy consideration

The `query` string in `MemoryHitEvent` is visible to all SSE subscribers. The UI currently uses only `itemIds` for node highlighting and ignores `query` for display, but any client parsing the raw `data:` line can see what another user searched for.

If search query privacy becomes a requirement, the fix is to strip `query` from the broadcast payload and keep it only in the server-side `hitCounts` accounting. This is a one-line change in `hub.go:Publish`. It has not been done because the query label enriches the graph experience for all viewers.

## Alternatives Considered

- **Per-user SSE streams** — would eliminate cross-user visibility entirely but destroy the collective consciousness effect and add session-aware routing complexity.
- **Strip `query` from broadcast** — preserves node highlighting while hiding search terms. Low-cost option if privacy requirements change.
- **Attribute events to a user** — adding an anonymised `userID` field to `MemoryHitEvent` would let the UI distinguish "my" highlights from others'. Not done; adds complexity and may feel surveillance-like.
