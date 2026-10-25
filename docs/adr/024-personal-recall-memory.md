# ADR-024: Personal Recall Memory (`kind=recall`)

**Status:** Accepted  

## Context

Voice sessions are stateless across restarts — `agent_note` items are ephemeral and lost when the pod restarts. There is no mechanism for X-Ray to remember things about a specific person across sessions: their preferences, context they've shared, or observations the model has made about them.

A personal memory layer scoped to the authenticated user enables X-Ray to build a durable, evolving picture of each person it talks to, without polluting the shared knowledge graph.

## Decision

Introduce a new memory kind: **`recall`**.

- `subject` is the authenticated username (GitHub login or Azure UPN short form)
- Lives in-memory by default — not synced to a MemoryCell CR unless explicitly persisted
- Can be persisted on demand (via voice tool or reflection loop)
- A reflection loop reads all `recall` items for a subject, synthesises a summary, and writes a consolidated `recall` back — compacting memory over time

## Consequences

- `recall` items are excluded from X-Ray pages (same as `agent_note`)
- `search_memory` scopes `recall` results to the authenticated user's subject automatically
- Reflection loop details (trigger, frequency, summary format) are deferred — see follow-up todo
- Persistence policy (who can persist, CR vs. other storage) is deferred

## Ideas

- **Opt-in memorisation**: X-Ray only creates a `recall` item when the user explicitly asks ("remember this", "keep that in mind"). No passive observation recording.
- **User-owned deletion**: Any authenticated user can delete their own `recall` cells via voice (`"forget that"`, `"clear what you know about me"`). No `writeAllowedUsers` gate needed for self-deletion — ownership is implied by `subject == authenticated user`.

## Open Questions

- Reflection trigger: voice-initiated, end-of-session, or threshold-based?
- Persistence target: MemoryCell CR per user, or dedicated storage?
- Who can persist: `writeAllowedUsers` only, or any authenticated user for their own recalls?
