# TODO: MCP Tools as General Agentic Memory

ADR: `docs/adr/051-mcp-tools-general-agentic-memory.md`

## Overview

Unify X-Ray's MCP tool set (`internal/xray/mcpserver.go`) and voice tool set
(`internal/xray/voice_tools.go`) into **one** 7-tool set, used identically by
both surfaces: `get_memory`, `list_memory`, `search_memory`, `fetch_memory`,
`recent_memory`, `upsert_memory`, `delete_memory`. Hard result caps on every
list/search/recent tool. Full ADR-020 kind vocabulary in descriptions.
`subject` documented as the general multi-tenant private-scope key for any
MCP caller, not just human voice sessions.

Triggered by wiring triage-agent's Jucce voice agent to X-Ray's MCP server: a
live session called `list_memory` with no `kind`/`limit` and got 50 items back
with no ceiling, surfacing that the two tool sets had diverged in name, shape,
and guardrails despite doing the same job against the same `MemoryStore`.

**Naming convention (ADR-051):** every tool's action is carried by a distinct
leading verb — `get`/`list`/`search`/`fetch`/`recent`/`upsert`/`delete` — and
no two verbs may be near-synonyms of each other (checked and rejected:
`get_memory_item`/`get_memory_items`, `get_memory_cell`/`get_memory_items`,
`search_memory`/`lookup_memory`).

## Current state (baseline)

- MCP (`internal/xray/mcpserver.go`), 5 tools: `get_memory_context`
  (id-or-subject dual mode), `list_memory` (kind/subject/limit, default 50,
  **no cap**), `search_memory` (query/subject/kind/limit, default 10, **no
  cap**), `upsert_memory` (kind enum description lists only 5 kinds),
  `delete_memory` (no ownership check on private items).
- Voice (`internal/xray/voice_tools.go`), 10 tools: `end_session` (session
  control, not memory — untouched by this todo), `get_memory` (counts+summary,
  no items), `get_memory_items` (kind/limit, default 20), `search_memory`
  (query/kind/limit, default 10, min-floor 3, headroom-compressed),
  `upsert_memory`, `save_memory` (writes note/episodic, always self-scoped),
  `promote_note` (compound: read private note → create public cell → delete
  source), `delete_memory` (private notes only, ownership-checked),
  `get_pinned_items` (full content of UI-pinned ids), `get_agent_notes` (all
  private items for caller, implicitly `subject=userHash`), `get_recent_memory`
  (episodic, newest-first, hard-capped at 20 — the pattern to copy for caps
  elsewhere).
- `MemoryStore.List`/`Search` (`internal/xray/memory.go`) apply whatever
  `limit` the caller passes with no independent ceiling — the cap has to live
  in the tool handler, not the store.
- `isPrivateItem(kind, scope)` / `subject`-keyed isolation (ADR-028, ADR-037)
  already supports any caller picking an arbitrary `subject` as a tenant key
  for private-scope items (ADR-037: "callers that set scope (MCP/admin) are
  trusted for it") — undocumented in tool descriptions until now.

## Target tool set (both MCP and voice, identical names/params/behavior)

| Tool | Params | Returns |
|---|---|---|
| `get_memory` | *(none)* | Counts by kind + summary text. No items. |
| `list_memory` | `kind?`, `subject?`, `limit` (hard-capped) | Full items matching a structural filter, unranked. |
| `search_memory` | `query` (required), `kind?`, `subject?`, `limit` (hard-capped, min-floor) | Full items ranked by relevance. |
| `fetch_memory` | `ids` (required, list) | Full content of the requested items, in order; missing ids simply absent. |
| `recent_memory` | `limit` (hard-capped at 20) | Episodic items, newest-first by creation time. |
| `upsert_memory` | `kind`, `scope`, `title`, `content` (required), `id?`, `subject?`, `confidence?`, `event_date?`, `tags?` | Create/update one item. |
| `delete_memory` | `id` (required) | Delete one item; ownership-checked when `scope=private`. |

`promote_note` is dropped (not generalized) — a compound workflow, not a
memory primitive. Callers compose it from `fetch_memory` + `upsert_memory` +
`delete_memory`.

## Items

### 1. Add hard result caps — DONE (operator-configurable via flag, not hardcoded)

- Implemented: `NewMCPServer` takes `listMemoryMaxLimit`/`searchMemoryMaxLimit
  int` params (`<= 0` falls back to `DefaultMCPListMemoryMaxLimit`=20 /
  `DefaultMCPSearchMemoryMaxLimit`=10). Wired to new CLI flags
  `--mcp-list-memory-max-limit` / `--mcp-search-memory-max-limit` in
  `cmd/xray/main.go` — these are deployment-tunable, not baked into the
  binary as constants (each MCP tool consumer may want a different ceiling).
  Mirrors `voice_tools.go`'s `get_recent_memory` cap pattern
  (`if limit > 20 { limit = 20 }`).
- Tests done: `TestMCPListMemory_LimitHardCapped`,
  `TestMCPListMemory_LimitDefaultsToConstantWhenUnconfigured`,
  `TestMCPSearchMemory_LimitHardCapped` — verify both the configured-cap path
  and the falls-back-to-default path.
- Still open: voice's `get_memory_items`/`search_memory` caps remain
  hardcoded literals in `voice_tools.go` (20/10/3-floor) — not flag-driven.
  Consider whether voice should get the same flag-configurability once item 6
  (rename) unifies it with MCP's `list_memory`/`search_memory`.

### 2. Implement `fetch_memory` — DONE (both surfaces)

- MCP (`internal/xray/mcpserver.go`): `ids []string` (required) + optional
  `subject`; ownership-checked (private item omitted unless `subject`
  matches); missing id silently omitted, not an error.
- Voice (`internal/xray/voice_tools.go`, `defFetchMemory`): `ids`
  *optional* — when omitted, falls back to the pinned-items set from the
  `getPinnedIDs()` closure (preserves `get_pinned_items`' zero-arg UX under
  the new name); same ownership check either way.
- Tests: `TestMCPFetchMemory_ReturnsRequestedItemsInOrder`,
  `TestMCPFetchMemory_MissingIDOmittedNotErrored`,
  `TestMCPFetchMemory_CrossSubjectPrivateItemOmitted` (MCP);
  `TestFetchMemory_NoCrossUserPrivateLeak`,
  `TestFetchMemory_ExplicitIDsBypassPinnedFallback` (voice).

### 3. Implement `recent_memory` — DONE (both surfaces)

- MCP: new tool, same behavior as voice's `get_recent_memory` (episodic,
  newest-first, hard-capped — configurable via `--mcp-recent-memory-max-limit`,
  default 20).
- Voice: `defGetRecentMemory` renamed to `defRecentMemory`, tool name
  `get_recent_memory` → `recent_memory`.
- Tests: `TestMCPRecentMemory_NewestFirst`, `TestMCPRecentMemory_LimitHardCapped`
  (MCP); existing episodic tests in `voice_episodic_test.go` updated to the
  renamed function.

### 4. Add `get_memory` to MCP (currently voice-only)

- New tool in `mcpserver.go`: no params, returns `{total, by_kind, summary}` —
  same shape as `voice_tools.go`'s `defGetMemory`.

### 5. Add ownership check to MCP's `delete_memory` — DONE (documentation-level; real auth still open)

- Implemented: `delete_memory` now takes an optional `subject` param; a
  private item is only deletable when the passed `subject` matches the
  item's own `subject` (`internal/xray/mcpserver.go`). Unlike voice's
  version (which restricted deletion to private items only), this allows
  deleting a public item too — a general agent should be able to correct its
  own public writes, not just its private notes.
- **Still open** (see ADR-051 "Negative / open questions"): this check is
  only as strong as the MCP auth trust model, which today lets any caller
  holding the shared static secret claim any `subject` string — there is no
  real per-agent identity binding a credential to a fixed subject the server
  enforces. Two options on the table, undecided: (a) per-agent API keys bound
  server-side to a fixed subject, or (b) OAuth client-credentials grant for
  service accounts (reusing `validateMCPToken`, which already returns a real
  `sub` for browser-OAuth callers but has it discarded in
  `mcpserver.go` today). Needs a decision before this ownership check (or any
  future per-subject write restriction) is more than a convention a
  well-behaved caller follows voluntarily.
- Test done: regression test — private item created under subject A is not
  deletable by a call omitting/using a different subject (documents the
  current, unenforced trust level — not a claim that this is airtight).

### 6. Rename voice's `get_memory_items` → `list_memory` — DONE

- Correction to the original plan: `save_memory` and `upsert_memory` are
  NOT duplicates — voice already registers both simultaneously as a
  deliberate two-tier privilege design (`save_memory`: any authenticated
  user, fixed private scope, fixed `subject=userHash`, kind locked to
  `note`/`episodic`; `upsert_memory`: write-allowlisted users only, any
  kind/scope/subject). Left both as-is; only `get_memory_items` →
  `list_memory` was a real rename (`defGetMemoryItems` → `defListMemory` in
  `internal/xray/voice_tools.go`).
- Also added: `defFetchMemory` (new, `fetch_memory`) replacing
  `defGetPinnedItems` + `defGetAgentNotes` — takes an optional `ids []string`;
  when supplied, fetches exactly those (ownership-checked, same as
  `delete_memory`); when omitted, falls back to the pinned-items behavior
  (reads from the `getPinnedIDs()` closure) so the zero-arg "what have I
  pinned" UX is preserved under the unified name. `get_agent_notes`' job
  (browse your own private notes) is now `list_memory(kind="note",
  subject=<your own>)` — no separate tool needed, folded into `list_memory`'s
  prompt guidance instead.
- `defGetRecentMemory` → `defRecentMemory` (tool name `get_recent_memory` →
  `recent_memory`), matching the MCP-side tool from item 3.
- Updated `writeTool` allowlist-bypass map in `buildVoiceTools` to the new
  names (`fetch_memory`, `recent_memory`; `get_pinned_items`/`get_agent_notes`/
  `promote_note` removed).
- Updated all string references in Helm values (`charts/xray/values.yaml`
  and `values-triage-xray.yaml`'s `enabledTools` lists and system prompt
  text) to the new names — these are live-deployed prompts, not just code.
- Tests updated/added: `TestBuildVoiceTools_*` assertions renamed;
  `TestFetchMemory_NoCrossUserPrivateLeak` (was
  `TestGetPinnedItems_NoCrossUserPrivateLeak`),
  `TestFetchMemory_ExplicitIDsBypassPinnedFallback` (new);
  `voice_episodic_test.go`'s `defGetRecentMemory` calls renamed to
  `defRecentMemory`.

### 7. Drop `promote_note` — DONE

- Removed `defPromoteNote` and its registration from `buildVoiceTools`.
- Removed its two dedicated tests (`TestPromoteNote_OwnershipCheck`,
  `TestPromoteNote_DeletesSourceNote`) — no live voice system prompt in
  either deployment (`xray`, `triage-xray`) instructed the model to
  call it, confirmed by grep before removal.
- Callers needing that workflow (promote a private note to a public/other-kind
  cell) now compose it from `fetch_memory` (read the note) + `upsert_memory`
  (create the new cell) + `delete_memory` (remove the source) — three tool
  calls instead of one, but no primitive lost.

### 8. Broaden `upsert_memory`'s `kind` description to the full ADR-020 vocabulary

- Add `active_concern`, `operational_pattern`, `watch_item`, `hypothesis`,
  `unknown` alongside the existing 5 X-Ray kinds (already accepted by
  `MemoryStore.Upsert`, which has no kind enum enforcement — documentation-only
  fix).

### 9. Document `subject` as the general multi-tenant private-scope key

- In `upsert_memory`, `list_memory`, `search_memory` descriptions (both
  surfaces): pass a stable `subject` (e.g. an agent's own name like
  `"triage-agent"`) with `scope=private` for working memory invisible to other
  callers unless they also pass that same `subject`; `scope=public` for shared
  knowledge.
- Test: two different `subject` values' private items don't leak into each
  other's `list_memory`/`search_memory`/`fetch_memory` results (extends the
  agent-note tenant isolation suite, ADR-028).

## Cross-repo naming drift — STILL OPEN on the triage-agent side

`charts/xray/values.yaml` and `values-triage-xray.yaml` (this repo)
have been updated to the final names — `enabledTools` lists and system
prompt text both use `get_memory`/`list_memory`/`search_memory`/
`fetch_memory`/`recent_memory`/`save_memory`/`upsert_memory` now.

**triage-agent's deployment values file's Jucce system
prompt** (`prompts.jucceSystemPrompt`, "Awareness Memory"/"Writing Memory"
sections) was NOT touched by this work (separate repo) and still references
the OLD names (`get_memory_items`, `get_recent_memory`) that no longer exist
on the MCP server Jucce connects to. Needs the same find-and-replace applied
there: `get_memory_items` → `list_memory`, `get_recent_memory` →
`recent_memory` (`get_memory`/`search_memory`/`save_memory` already match).
`fetch_memory` could also be mentioned as a way to get full content of a
known item by id, though Jucce's prompt doesn't currently reference the old
`get_pinned_items`/`get_agent_notes` equivalents so no direct replacement is
needed there.

## Explicitly out of scope (see ADR-051)

- No change to the MCP auth trust model — a caller authenticated with the
  static shared secret can still set any `subject` string; no per-token
  subject allowlisting.
- Triage-agent's reflection/knowledge loop (`agent_reflection.go`) still can't
  consume these tools — it's a pure text-completion call with no tool-calling
  capability. Separate, larger piece of work. Not tracked here.

## Notes

- Sequencing: 1 (caps) and 4/8/9 (additive: new `get_memory` on MCP, broader
  kind docs, subject docs) first — zero breakage. Then 2/3 (new tools
  replacing old ones) before 6/7 (the actual renames/removals), so the new
  behavior exists and is tested before the old names disappear. 5 (delete
  ownership check) can land anytime, it's a pure hardening fix.
- Test file: extend `internal/xray/mcp_test.go` / `voice_tools_test.go`
  (whichever exists) rather than creating a new file, unless the existing
  ones are already large — check before adding.
