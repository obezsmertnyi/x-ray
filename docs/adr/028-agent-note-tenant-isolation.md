# ADR-028: Agent Note Tenant Isolation

**Status:** Implemented  
**Date:** 2026-05-25  

## Context

`agent_note` items are personal — created by the voice session for a specific authenticated user and scoped to them via `subject=userHash` (SHA-256[:16] of the raw SSO ID). However, the current implementation has no enforcement layer: every code path that serves memory items must individually opt-in to subject filtering. Several paths don't, causing personal notes to leak across users.

Audit findings (see todo `agent-note-tenant-isolation.md` for full detail):

- `GET /v1/memory?q=` (UI search) — passes no subject; `KeywordSearch` returns agent_notes alongside shared knowledge
- `GET /v1/memory/summary` — graph load returns all agent_notes from all users
- SSE broadcast — `PublishChange` sends full agent_note to all connected clients; frontend adds any received agent_note to local state without checking subject
- MCP `search_knowledge(kind="agent_note")` — ignores `subject` parameter entirely
- MCP `upsert_memory(kind="agent_note")` — accepts any subject from caller with no identity validation

## Decision

Enforce tenant isolation at the **storage layer** for `agent_note`, not at each call site.

### 1. Exclude agent_note from general search and list paths

`KeywordSearch` and `List` treat `agent_note` identically to `system_configuration`: excluded from results unless `kind="agent_note"` is **explicitly** requested. This is already done in `listMap` (line 739) but missing in `keywordSearchMap` (line 474) and `keywordSearchCache` (line 389). Fix both to match `listMap`.

### 2. Strip agent_note from REST endpoints not designed for personal data

`GET /v1/memory` and `GET /v1/memory/summary` are public shared-knowledge endpoints. They must never return `agent_note` items regardless of query params.

`GET /v1/memory/{id}` is also a shared endpoint. It returns 404 for any `agent_note`, even to the owner — this prevents IDOR enumeration through a public surface.

Owners read their notes exclusively through the **owner-scoped endpoints**:
- `GET /v1/user/notes` — lists all notes for the authenticated session (enforces `subject == stableUserID(session)`)
- `GET /v1/user/notes/{id}` — fetches a single note; returns 404 if not owned by the caller

This means: `/v1/memory/{id}` blanket-denies personal notes; owners access them via `/v1/user/*` endpoints.

The fix: in `registerMemoryRoutes`, strip `agent_note` from list/summary results via post-filter, and return 404 unconditionally from the `{id}` endpoint for agent_notes.

### 3. Fix SSE — filter agent_note events by subject before broadcasting

Two options considered:

**Option A:** Backend filters — SSE hub becomes user-aware, routes events only to the session that owns the note.  
**Option B:** Frontend filters — SSE broadcasts carry `subject`; frontend checks `subject === userHash` before updating state.

**Decision: Option B** — SSE hub stays stateless (no session tracking). The backend already includes `subject` in the SSE payload. Frontend must check `evt.item.subject === userHash` before adding to `userNotes` state. This is a one-line fix in `page.tsx`.

### 4. Fix MCP search_knowledge — always pass subject for agent_note

Remove the `kind="agent_note"` special case in `mcpserver.go`. Always call `memory.Search(ctx, query, kind, subject, limit)`. The `subject` parameter is required in the tool schema — callers must supply it.

> **Amendment (ADR-030 follow-up):** Making `subject` *unconditionally* required broke shared-knowledge search — blog posts (`subject` = author name) and contributors (`subject` = person name) were unreachable, so a required `subject=xray` returned nothing for them. `subject` is now **conditionally required**: optional for shared kinds (empty searches across all subjects), but still **required when `kind=agent_note`**, rejected otherwise (`{"error":"subject is required when kind=agent_note"}`). The tenant boundary is preserved — the storage layer does not scope `agent_note` by itself (it only hides notes when `agent_note` is not the requested kind), so the call-site guard is what prevents an unscoped note dump. The voice `search_memory` path applies the same rule: it forces the shared search to drop `kind=agent_note` and serves notes exclusively from the `userHash`-scoped merge. Tests: `TestMCPSearchKnowledge_AgentNoteRequiresSubject`, and the unscoped-storage documentation case in `TestSearchMemoryNoAgentNoteLeakAcrossUsers`.

### 5. Fix MCP upsert_memory — disallow cross-user subject on agent_note via MCP

The MCP server is authenticated by a bearer token representing an agent (service account), not an individual user. An agent should not be able to create notes attributed to arbitrary users. For `kind="agent_note"`, the `subject` field must be non-empty and is accepted as-is (the agent is trusted to know which user it is acting for). This is acceptable because the MCP token is a privileged credential.

If stricter isolation is needed in future (per-user MCP tokens), that is deferred.

## Consequences

- `agent_note` items never appear in `/v1/memory` or `/v1/memory/summary` responses — simplifies the shared knowledge graph and eliminates the REST leak
- SSE events for agent_notes are still broadcast (no server-side fan-out needed) but filtered client-side — simpler than user-aware SSE hub
- MCP `search_knowledge` with `kind="agent_note"` now requires a valid `subject` to return results — consistent with all other kind searches
- Voice session behaviour unchanged — `defGetAgentNotes` and `defSearchMemory` already pass `userHash` correctly

## Implementation

**Tests:**
- `TestSearchMemoryNoAgentNoteLeakAcrossUsers`
- `TestKeywordSearchMapSubjectEquality`
- `TestDeleteMemory_OwnershipCheck`
- `TestServer_MemoryList_NoAgentNote`
- `TestServer_MemorySummary_NoAgentNote`
- `TestServer_MemoryGet_AgentNote_Returns404` — shared endpoint blanket-denies agent_notes
- `TestServer_UserNotes_GetByID_UnauthenticatedReturns404` — unauthenticated request gets 404 even if subject matches stableUserID("")
- `TestServer_UserNotes_GetByID_OwnerCanRead` — owner reads via `/v1/user/notes/{id}`; non-owner gets 404
- `TestMCPUpsertMemory_AgentNote_RequiresSubject`
