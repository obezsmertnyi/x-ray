# ADR-037: Scope as the Privacy Axis — `agent_note` Replaced by `note` + scope

**Status:** Implemented
**Date:** 2026-05-31
## Context

Personal notes were originally a distinct memory `kind`, `agent_note`. Privacy
was enforced by **kind-based special cases** scattered across the codebase:
"if `kind == "agent_note"`, hide it from shared search / SSE / list endpoints,
and scope it to the writing user." This had two problems:

- **Privacy was tangled with content type.** Whether a cell is personal is
  orthogonal to *what* it is. A user might want a private fact, a private event —
  not only a private "note". Keying privacy off one kind couldn't express that.
- **The special cases drifted.** Every read path
  (`GET /v1/memory`, `/summary`, `/{id}`, keyword search, SSE broadcast, MCP
  search) needed its own `kind == "agent_note"` guard. Miss one and a note leaks.

## Decision

**Privacy is a `scope` axis, orthogonal to `kind`. `agent_note` is retired in
favor of `kind=note` + `scope=private`.**

- `scope` is two-valued: **`private`** (isolated; `subject` identifies the owner)
  and **`public`** (shared knowledge, visible on the graph and in user-facing
  search). A note can be public (e.g. a system announcement); a personal note is
  just `scope=private`.
- A **single predicate**, `isPrivateItem(kind, scope)` =
  `scope == "private" || kind == "system_configuration"`, replaces all the
  scattered `kind == "agent_note"` checks. Every guard keys off this one
  function — read endpoints, keyword indexing, SSE broadcast, MCP search.
- **Voice writes are always private.** `note_observation` upserts
  `kind=note, scope=private, subject=userHash`; personal notes are served only
  via `/v1/user/notes`, ownership-checked against the caller's userHash.
- ~~The UI synthesizes `kind=agent_note` for the per-user note stream as a display
  alias; `category-config.ts` (ADR-035) aliases it back to `note`/Notes.~~
  **Superseded by the Addendum below — the UI now uses `kind=note` directly; the
  `agent_note` alias and `KIND_ALIASES` are removed.**

## Consequences

- **Privacy is expressible per cell** without inventing a kind per content type.
- **One guard, not N.** Adding a read path means calling `isPrivateItem` once;
  there is no per-kind leak surface.
- **Public notes are intentional.** A `note` with `scope != private` is shared by
  design (it is indexed and broadcast). The write paths that must stay personal
  (voice) force `scope=private`; callers that set scope (MCP/admin) are trusted
  for it. A *legacy* `agent_note` with a non-private scope is a migration
  artifact, not a leak — such cells are re-scoped at the data source.
- **`system_identity` tagging is mandatory.** Because privacy and category are
  both now data-driven (scope + tags), the `project`/`contributor` tag is
  required and gates the UI category (see ADR-035); untagged identities fall to
  "Other".

## Tests

Covered by the agent-note tenant-isolation suite (ADR-028) re-expressed in terms
of scope, and the per-path `isPrivateItem` guards exercised in
`internal/xray/*_test.go`.

## Addendum (2026-05-31): owner-scoped SSE delivery; `agent_note` UI alias removed

Two parts of the original decision are revised:

### 1. Private notes are SSE-delivered to their owner (not "never broadcast")

The original `isPrivateItem` SSE guard skipped *all* broadcasting for private
items. The reasoning was sound — the shared `/v1/memory/events` stream has no
notion of users, so any event on it reaches every authenticated client and would
leak the note's content. But the cost was that a newly created/updated note never
appeared live in its owner's UI; it surfaced only on a manual refetch.

The hub is now **user-aware**. Each SSE connection registers under its owner's
`userHash`, resolved from the session at `GET /v1/memory/events`
(`callerIdentity` → `stableUserID`). `Hub.PublishChangeTo(userHash, evt)`
delivers an event only to channels owned by that hash; an empty hash (auth
disabled, `--dev`) falls back to a full broadcast so the single local user still
sees updates.

`MemoryStore.Upsert` and `Delete` route accordingly:

- **Shared items** (`!isPrivateItem`) → `PublishChange` (broadcast to all), unchanged.
- **Private notes** (`scope=private`, non-empty `subject`) → `PublishChangeTo(subject, …)`,
  reaching only the owner. `subject` is the `userHash`, so no extra lookup is needed.
- **`Delete`** captures the item's scope/subject before removal and owner-scopes the
  delete event for private items — broadcasting even a bare ID would tell every
  client a note existed and was removed.
- `system_configuration` has no owner and is never surfaced, so it is published nowhere.

Content isolation is preserved: a private note's body travels only down its
owner's channel. Tests: `TestHub_PublishChangeTo_OwnerOnly`,
`TestHub_PublishChangeTo_EmptyHashBroadcasts`, `TestMemoryStore_PrivateNoteOwnerScoped`.

### 2. The `agent_note` UI alias is removed; the UI uses `kind=note`

The original decision kept the UI synthesizing `kind=agent_note` as a display
alias, mapped back to `note` by `KIND_ALIASES` in `category-config.ts`. This
required the alias to undo itself everywhere and re-introduced the very
kind-vs-privacy entanglement ADR-037 set out to remove. It also masked two live
bugs in the SSE note listeners (matching the non-existent `agent_note` kind, and
comparing the note's `subject` hash against the raw SSO `userID`), so notes never
updated live even before the owner-scoped fix above.

`agent_note` is removed from the frontend entirely. Graph nodes, the item panel,
fetch routing (`fetchMemoryItem` → `/v1/user/notes`), and the subject-hiding
header check all key off the canonical `kind=note` directly. The `notes` category
already resolves from `kind=note`, so `KIND_ALIASES` is deleted. `agent_note` now
appears nowhere in the codebase except the `get_agent_notes` *voice tool name*,
which is an unrelated identifier (the Gemini tool the model calls to list a user's
notes), not a memory kind.
