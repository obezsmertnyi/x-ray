# ADR-051: MCP Tools as General Agentic Memory — Align with Voice, Add Multi-Tenant Scope

**Status:** Implemented (X-Ray side)
**Date:** 2026-07-04
**Todo:** [051-mcp-general-agentic-memory](../todo/051-mcp-general-agentic-memory.md)

## Context

X-Ray's MCP server (`internal/xray/mcpserver.go`, ADR-004) and its voice-session
tool set (`internal/xray/voice_tools.go`) evolved independently and now diverge
in name, shape, and guardrails, even though both operate on the same
`MemoryStore`:

| Voice tool | MCP tool | Divergence |
|---|---|---|
| `get_memory` | *(none)* | No MCP equivalent for the orientation/counts snapshot |
| `get_memory_items` | `list_memory` | Different name; MCP has no hard result cap |
| `search_memory` | `search_memory` | Same name, but MCP has no min-limit floor and no hard cap; no headroom compression |
| `save_memory` / `upsert_memory` | `upsert_memory` | MCP's `kind` description lists only 5 X-Ray kinds (`stable_fact`, `recent_change`, `system_identity`, `event`, `note`), omitting the SRE kinds (`active_concern`, `operational_pattern`, `watch_item`, `hypothesis`, `unknown`) that ADR-020 already defines and that external agents (e.g. the Triage core) actually write |
| `delete_memory` (private-notes-only, ownership-checked) | `delete_memory` (any item, no ownership check) | Voice has a real guard MCP lacks |
| `get_pinned_items` | *(none)* | Full content of UI-pinned ids — voice/UI-specific, but functionally "fetch several full items by id" |
| `get_agent_notes` | *(none)* | All private items for the caller (implicitly scoped to `userHash`) — functionally `list_memory(subject=caller, scope=private)`, just implicit |
| `get_recent_memory` | *(none)* | Time-ordered (newest-first) recall of episodic items, hard-capped at 20 — a genuinely different shape from `list_memory`'s unordered filter |
| `promote_note` | *(none)* | Compound op (read private note → create public cell → delete source) — a workflow, not a memory primitive; dropped from the redesign rather than generalized |
| — | — | Neither tool set documents `subject` as a general multi-tenant key for external agents |

**Trigger:** wiring an external voice agent to X-Ray's MCP server as a
shared memory backend (alongside its own internal `AwarenessMemoryCell`
storage) surfaced two concrete problems:

1. **Unbounded list/search calls.** A live session issued `list_memory` with no
   `kind`/`limit` and got back 50 items — harmless today, but `list_memory` and
   `search_memory` have no server-side ceiling (`MemoryStore.List`/`Search`
   apply whatever `limit` the caller passes, including 0/unset which some
   paths treat as "no cap"). A misbehaving or adversarial external agent can
   force a full-store dump.
2. **No clear guidance for multi-agent private memory.** `scope=private` is
   already tenant-keyed by an arbitrary `subject` string, not by a specific
   identity system (ADR-037: *"callers that set scope (MCP/admin) are trusted
   for it"*) — the mechanism already supports one agent's private working
   memory being invisible to another, but no tool description teaches this
   pattern, so external agents have no way to discover or use it.

Prompted by trying to reuse X-Ray as an agent's shared memory: the
"private/public scope" model that already exists for voice/human users
(ADR-028, ADR-037) generalizes cleanly to *any* MCP caller, human or agent — an
agent just picks a stable `subject` (its own name, e.g. `"my-agent"`) as
its private tenant key, the same way a human's `subject` is `userHash`. This
ADR proposes making that pattern a first-class, well-documented MCP surface
rather than an incidental capability of the storage layer.

## Decision

**One unified tool set, 7 tools, used identically by MCP and voice.** Both
surfaces call the same `MemoryStore` methods through the same handler code;
today's split (5 MCP tools, 10 voice tools, mostly-but-not-quite-overlapping)
collapses into a single set that expands the CRUD trio
(`search`/`upsert`/`delete`) with the read shapes actually needed: an
aggregate summary, a filtered browse, a recency-ordered recall, and an
exact-id fetch.

### Naming convention: one distinct verb per action, never a shared-noun near-miss pair

Two earlier drafts of this ADR hit the same failure mode at different levels:
`get_memory_item` next to `get_memory_items` (singular/plural, one letter
apart), then `get_memory_cell` next to `get_memory_items` (`cell`/`cells`,
same problem one level removed). `search_memory` vs `lookup_memory` was also
checked and rejected as too close semantically (both mean "find something"),
even though the words don't share a suffix.

**Rule: every tool name's action is carried entirely by its leading verb —
`get`/`list`/`search`/`fetch`/`recent`/`upsert`/`delete` — and no two verbs may
be near-synonyms of each other, not just non-identical.** The verb alone must
be enough to route to the right tool without reading the rest of the name.

### The 7 tools

| Tool | Verb | Params | Returns |
|---|---|---|---|
| `get_memory` | **get** — aggregate summary | *(none)* | Counts by kind + summary text. No items. |
| `list_memory` | **list** — filtered browse | `kind?`, `subject?`, `limit` (hard-capped) | Full items matching a structural filter, unranked. Replaces MCP's `list_memory`, voice's `get_memory_items`, and voice's `get_agent_notes` (call with `subject=<caller>, scope=private`). |
| `search_memory` | **search** — relevance query | `query` (required), `kind?`, `subject?`, `limit` (hard-capped, min-floor) | Full items ranked by relevance to free text. Same job on both surfaces today; unify on voice's guardrails (min-limit floor, headroom compression). |
| `fetch_memory` | **fetch** — exact retrieval | MCP: `ids` (required, list). Voice: `ids?` (optional — omitted falls back to the user's pinned X-Ray items, preserving `get_pinned_items`' zero-arg UX) | Full content of the specific items requested, in the order given (missing ids simply absent from the result, not an error). Replaces MCP's `get_memory_context` id-mode and voice's `get_pinned_items` + `get_agent_notes` (the latter's job — browse your own notes — is `list_memory(kind="note", subject=<caller>)`). |
| `recent_memory` | **recent** — time-ordered recall | `limit` (hard-capped at 20) | Episodic items, newest-first by creation time. A genuinely different shape from `list_memory` (ordering matters, kind is implicitly `episodic`), not just a filter variant — kept as its own tool rather than a `sort=` param. Replaces voice's `get_recent_memory`; new on MCP. |
| `upsert_memory` | **upsert** — write | `kind`, `scope`, `title`, `content` (required), `id?`, `subject?`, `confidence?`, `event_date?`, `tags?` | Create/update one item. Unifies MCP's and voice's `upsert_memory` (same name, same shape already). **Not** unified with voice's `save_memory` — that's a deliberate, separate low-privilege tool (any authenticated user, fixed private scope, fixed `subject=userHash`, kind locked to `note`/`episodic`) distinct from `upsert_memory`'s full-privilege (write-allowlisted, any kind/scope/subject) shape; folding them would remove a real access-control boundary, so both remain registered on voice. |
| `delete_memory` | **delete** — remove | `id` (required) | Delete one item. Adds the ownership check voice's version had (private items only deletable by their own `subject`) to the MCP surface, which currently has none. |

`promote_note` (voice) is **dropped**, not generalized — it's a compound
workflow (read a private note → create a differently-scoped cell → delete the
source), not a memory primitive. Any caller needing that behavior composes it
from `fetch_memory` + `upsert_memory` + `delete_memory` directly.

Seven tools, seven distinct verbs — none is a substring, plural, or
near-synonym of another, so there is no pair a model can confuse by skimming
the verb alone.

### Multi-tenant scope, documented explicitly

`upsert_memory`, `list_memory`, and `search_memory` all document `subject` as
the private-scope tenant key for *any* caller: pass a stable `subject` (e.g.
an agent's own name like `"my-agent"`) with `scope=private` for working
memory invisible to other callers unless they also pass that same `subject`;
`scope=public` for shared knowledge visible to everyone, including the X-Ray
UI.

### Guardrails carried over from voice, applied uniformly

- **Hard server-side caps** on every list/search tool — not just a default,
  a ceiling regardless of what the caller requests. (`get_recent_memory`'s
  `if limit > 20 { limit = 20 }` in `voice_tools.go` is the existing pattern
  to copy.)
- **Full kind vocabulary** in every tool description (ADR-020's 11 kinds), so
  external agents know `active_concern`/`operational_pattern`/`watch_item`/
  `hypothesis`/`unknown` are valid, not just the 5 kinds X-Ray's own content
  happens to use.
- **Explicit multi-tenant documentation**: `subject` is the private-scope
  tenant key for *any* caller, not implicitly tied to human SSO identity.
  ADR-037's existing trust model ("callers that set scope are trusted for it")
  is preserved, just made discoverable via tool descriptions instead of left
  implicit.

### Explicitly out of scope for this ADR

- **Identity verification for MCP-caller `subject` values.** Any caller
  authenticated with the static shared secret (the credential external agents
  like the Triage core actually use — see `internal/xray/mcpserver.go`) can set
  *any* `subject` string; there is no cryptographic binding between a bearer
  token and an allowed set of subjects. This ADR does not change that trust
  model (per ADR-037, it's already accepted for MCP/admin callers) — it only
  makes the pattern legible. A future ADR could add per-token subject
  allowlisting if stronger isolation between agents is needed.
  **Open decision, raised while implementing `delete_memory`'s ownership
  check:** once ownership actually gates a destructive operation (delete, and
  arguably update-of-others'-private-items), an unenforced "any caller can
  claim any subject" model stops being a documentation nuance and becomes a
  real gap — a caller with the shared static secret can delete anything by
  guessing/asserting the right `subject` string. Two concrete options
  surfaced, neither decided yet:
  (a) **per-agent API keys bound to a fixed subject** — each agent (e.g.
  the Triage core) gets its own bearer credential, registered server-side with a
  subject the server enforces (the caller can't override it even if it tries)
  — smaller lift, reuses the existing static-secret-style credential, needs a
  credential→subject mapping;
  (b) **OAuth client-credentials grant for service accounts** — extend the
  existing OAuth 2.1 authorization server (ADR-034) with a non-human grant
  type so an agent authenticates as its own registered client and gets a JWT
  with `sub=client_id`, validated through the same `validateMCPToken` path
  already used for humans (note: `validateMCPToken` already returns a real
  `sub` for browser-OAuth callers today — `internal/xray/mcpserver.go`
  currently discards it, so wiring that through is a prerequisite for option
  (b) regardless of which option is chosen for humans vs. agents) — more
  proper OAuth, more implementation work (new grant type, client
  registration/secret storage). Needs a decision before `delete_memory`'s
  ownership check (and any future per-subject write restriction) is more than
  a documentation-level convention.
- **The reflection/knowledge loop's own read/write path** (the Triage core's
  `agent_reflection.go`) is a separate, larger change — it is a pure
  text-completion call today with no tool-calling capability at all, so it
  cannot consume these MCP tools without first being restructured to run
  through the Triage core's ADK agent runner (which already supports MCP
  toolsets). Tracked separately; not part of this ADR.

## Consequences

**Positive:**
- One tool shape, one set of guardrails, for both voice sessions and external
  MCP agents — no more drift between the two surfaces.
- X-Ray becomes usable as genuine shared multi-agent memory: multiple agents
  can each keep private working memory (scoped by their own `subject`) while
  reading/writing a common public knowledge base, through the same 7 tools.
- Removes a real availability risk (unbounded `list_memory`/`search_memory`
  result sizes).
- Voice gains `fetch_memory` (batched exact-id fetch); MCP gains `get_memory`
  (summary), `recent_memory`, and an ownership check on `delete_memory` it
  didn't have before.

**Negative / open questions:**
- This is a breaking rename on **both** surfaces, not just one: voice's
  `get_memory_items`/`save_memory`/`get_pinned_items`/`get_agent_notes`/
  `get_recent_memory`/`promote_note` all change or disappear, and MCP's
  `get_memory_context` disappears in favor of `fetch_memory`. Voice-side
  breakage is lower risk (server-side function-calling declarations, not
  exposed to third parties) than MCP-side breakage (external clients like
  the Triage core depend on the exact tool names) — check for existing MCP
  integrations before removing old names outright, vs. keeping them as
  deprecated aliases for one release.
- `promote_note` is dropped outright rather than generalized — any caller
  needing that workflow must compose it from `fetch_memory` +
  `upsert_memory` + `delete_memory`. Confirm no current voice usage relies on
  its atomicity (read-then-write-then-delete in one call) before removing it.
- The "any caller can claim any subject" trust model (inherited from ADR-037)
  means one agent could, in principle, read or write into another agent's
  private subject if it guesses/knows the string — acceptable today per
  ADR-037's existing acceptance of this for MCP/admin callers, but worth
  revisiting if X-Ray starts hosting many mutually-distrusting agents.

## Tests (planned)

- Table-driven cap tests for `list_memory`, `search_memory`, `recent_memory`:
  requesting a limit far above the ceiling returns at most the ceiling.
- `fetch_memory`: multiple ids returns items in the requested order; a
  missing/nonexistent id is simply absent from the result, not an error;
  another user's private item is omitted unless `subject` matches.
- `delete_memory`: private item deletable only by its own `subject`
  (regression test for the ownership check MCP currently lacks).
- A multi-subject isolation test: two different `subject` values' private
  items don't leak into each other's `list_memory`/`search_memory` results
  when subject is unset vs. set (extends the existing agent-note tenant
  isolation suite, ADR-028).
- Tool-name migration test (if aliases are kept): old name still resolves,
  emits a deprecation log line.
