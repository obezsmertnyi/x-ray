# ADR-044: Session Recap Continuity — Cross-Call Context via Private Recap Notes

**Status:** Implemented — by episodic session memories and the recall block a session opens with (ADR-057)
## Context

Voice sessions are independent. Each `/v1/voice` upgrade builds a fresh
`genai.Session` with no memory of prior calls; the only per-user context that
re-enters a new session is the `"You are speaking with X"` line appended in
`VoiceManager.ServeHTTP` (`internal/xray/voice.go`). Gemini Live session
resumption (ADR-039) reconnects a *dropped* connection within a single
conversation — it does not carry context across separate calls, and resumption
handles expire.

In practice a user may talk to X-Ray several times in one day — a morning call,
a follow-up after lunch, an end-of-day check-in. Today each of those starts cold:
Sym cannot say "earlier you asked me to look into X" or "you mentioned you were
blocked on Y". The user has to re-establish context every time, which is exactly
the friction a persistent assistant should remove.

The building blocks already exist and do not need to be invented:

- **Private per-user notes** — `kind=note`, `scope=private`, `subject=userHash`
  (SHA-256[:16] of the SSO ID). Written by the `note_observation` voice tool
  (`defNoteObservation`), persisted as a `MemoryCell` CR via `syncer.Persist`,
  and tenant-isolated at the storage layer (ADR-026, ADR-028, ADR-037).
- **On-demand recall** — `get_agent_notes` (`defGetAgentNotes`) lists the
  caller's private notes (`memory.List("note", scopePrivate, userHash, 50)`),
  newest content included, with IDs.
- **Durability** — private-note CRs survive pod restarts and re-seed at startup
  (ADR-026), so context persists across days and deployments, not just within a
  pod lifetime.

What is missing is not storage or a recall path — it is a **convention and a
prompting steer**: get the model to write a short conversation recap *before the
call ends*, and to *read recent recaps at the start of the next call*.

## Decision

Introduce a lightweight **session recap** convention layered on the existing
private-note machinery. No new memory kind, no new storage, no new persistence
path — a recap is an ordinary `kind=note`, `scope=private`, `subject=userHash`
cell, distinguished only by a `session-recap` tag and a title convention.

### 1. The model writes a recap before ending the call

Steer the model (via the `system_configuration` system prompt and the
`end_session` / `note_observation` tool descriptions) to, near the end of a
conversation, call `note_observation` with a concise recap of the call before
calling `end_session`:

- **title**: `Session recap — <short topic>` (e.g. `Session recap — HNSW rebuild`)
- **content**: 1–4 sentences — what the user wanted, what was discussed or
  decided, and any open thread to pick up next time. Markdown, short.

This rides entirely on `defNoteObservation` as it exists today — same
`Upsert(ctx, "", "note", scopePrivate, title, content, userHash, ...)` call,
same `syncer.Persist`. Recaps are private to the caller and tenant-isolated by
`userHash` exactly like every other note (ADR-028). No tool signature changes
are required for the minimal version.

This matches the user's stated intent: *the model notes a short summary before
the end of the call*, rather than a server-side transcript summarizer. It keeps
capture in the model's judgement (it knows what mattered) and costs nothing when
a call has nothing worth remembering.

### 2. Recaps re-enter the next session on demand

The next session does **not** auto-inject prior recaps into the system prompt
(deliberate — see Alternatives). Instead the model pulls them when relevant via
the existing `get_agent_notes` tool, whose description is extended to mention
recaps explicitly:

> Call this … to recall what was discussed in **previous conversations with this
> user** (notes titled "Session recap — …"), and when the user refers to an
> earlier call.

Because `get_agent_notes` already returns the caller's private notes scoped to
`userHash`, recaps surface there with zero new code. The system prompt gains a
short steer: at the start of a conversation, if the user references something
earlier or you need their recent context, call `get_agent_notes` first.

### 3. (Optional, deferred) Recap tagging and a scoped recall tool

If recaps clutter the general `get_agent_notes` output or the user wants
recaps separated from working observations, two additive, backward-compatible
refinements are available:

- Tag recap notes `session-recap` (the `note_observation` handler already
  accepts the cell's `tags`; a recap-specific steer or a thin
  `note_session_recap` wrapper sets it).
- Add a `get_recent_conversations` tool that lists only `session-recap`-tagged
  private notes, newest first, capped to the last N. This is a filtered view
  over the same `memory.List` call — no new storage.

These are explicitly **not** part of the minimal decision; the minimal version
is prompt-only + the `get_agent_notes` description tweak.

## Alternatives considered

- **Auto-inject the last recap into the system prompt at session start.**
  Mirrors ADR-021's knowledge-digest injection and the `"You are speaking with
  X"` line — Sym would have context with no tool call. Rejected for the default
  per the user's choice (*on-demand via a tool*): it spends prompt tokens on
  every session even when the prior call is irrelevant, and risks stale context
  bleeding into unrelated conversations. The injection point
  (`ServeHTTP`, right after the identity line) is documented here so this remains
  a one-block change if we later want a compact head-injection (the "both"
  option).

- **Server-side transcript summarizer on session close.** A background pass
  reads the Gemini transcript when the WebSocket closes and writes the recap
  automatically (an application of ADR-021's reflection loop). Rejected for the
  default: it duplicates the reflection machinery, requires capturing/persisting
  transcripts, and removes the model's judgement about what is worth keeping. The
  model-writes-its-own-recap approach is cheaper and keeps capture where the
  context already lives.

- **New `kind=recall` (ADR-024) instead of a tagged `note`.** `recall` was
  proposed but never implemented; ADR-037 consolidated personal memory onto
  `kind=note` + `scope=private`. Adding a parallel kind now would fragment the
  personal-memory model. A tag on `note` gets the same separation without a new
  kind.

## Consequences

- **No schema or storage change.** Recaps are `kind=note`, `scope=private`
  cells. They inherit CR persistence (ADR-026), tenant isolation (ADR-028),
  exclusion from X-Ray pages and shared search (ADR-037), and SSE behaviour —
  all for free.
- **Continuity across calls and restarts.** A recap written in the morning is
  recalled (on demand) in the afternoon, and survives a pod restart because the
  note CR is re-seeded at startup.
- **Cost is bounded and opt-out by nature.** One extra short `note_observation`
  call near the end of a call; the model skips it when there is nothing to
  remember. Recall is a single `get_agent_notes` call only when relevant — no
  per-session prompt-token tax (the trade-off of choosing on-demand over
  auto-injection: Sym may occasionally fail to look before the user has to
  remind it).
- **Recaps appear alongside observations in `get_agent_notes`** until/unless the
  optional `session-recap` tagging + scoped tool (§3) is added. Acceptable for
  the minimal version.
- **Deletion already works.** Users delete their own recaps via the existing
  `delete_memory` user-scoped path (ADR-026) — "forget our last conversation"
  needs no new tool.

## Implementation sketch (for the follow-up todo)

Minimal (prompt-only) version — no Go changes strictly required:

1. **`system_configuration` cell (subject=xray), `VoiceSessionConfig.SystemPrompt`**
   — add two steers: (a) near the end of a call, write a one-line
   `note_observation` titled `Session recap — <topic>`; (b) at the start of a
   call, if the user refers to an earlier conversation or you need their recent
   context, call `get_agent_notes` first.
2. **`internal/xray/voice_tools.go` — `defGetAgentNotes`** — extend the tool
   `Description` to mention "previous conversations / session recaps" so the
   model knows recaps live there.

Optional refinements (additive, behind §3):

3. Tag recap notes `session-recap` (recap-specific steer or a thin
   `note_session_recap` wrapper that sets `tags`).
4. Add `defGetRecentConversations` — a filtered `memory.List("note",
   scopePrivate, userHash, N)` returning only `session-recap`-tagged notes,
   newest first; register it in `buildVoiceTools` under the `userHash != ""`
   block alongside `defGetAgentNotes`, and add it to the `writeTool` allowlist
   bypass map.

If we later adopt auto-injection (the "both" option): in `ServeHTTP`, after the
`"You are speaking with X"` line (`internal/xray/voice.go`), load the latest
recap(s) for `userHash` and append a `\n\n=== RECENT CONVERSATIONS ===\n…`
block — the same injection pattern as ADR-021's knowledge digest.

## Open questions

- **Recap freshness window.** Recall all recaps, or only the last N / last 24–48h?
  Old recaps drift stale. A simple "newest 3" cap is the likely default.
- **Compaction.** Many short recaps accumulate per heavy user. Fold into ADR-021's
  reflection loop (synthesize many recaps into one rolling summary), or rely on
  the "newest N" cap and let old ones age out via user deletion?
- **Privacy expectation.** Recaps capture conversation content per user. Confirm
  this is acceptable under the same expectation as `note_observation` today; the
  user owns and can delete them (ADR-026), and they are never shared (ADR-037).
