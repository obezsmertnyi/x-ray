# ADR-018: Voice Tool Content Budget

**Status:** Superseded by [ADR-038](038-headroom-token-compression.md)  

> The content-budget idea here is implemented by ADR-038, which replaces the
> naive character cap with Headroom-derived Preview (markdown-aware strip +
> boundary truncation), adaptive-K row selection, and field factoring, applied
> to both `search_memory` (voice) and `search_knowledge` (MCP).


## Context

Voice tools (`search_memory`, `get_memory_items`) return data that is injected back
into the Gemini Live context window as tool responses. There are two problems today:

1. **No per-item content cap** — `search_memory` returns the full `content` field of
   each matched item (thumbnail stripped). A single blog post or detailed project
   description can be thousands of characters. 10 results × unbounded = unbounded
   token spend per tool call.

2. **Model can request "give me all"** — `get_memory_items` accepts a `limit`
   parameter with no server-side enforcement ceiling. The model can (and does) call
   `get_memory_items` with no `kind` filter, pulling every item with full titles
   across all categories when a narrower `search_memory` would suffice.

## Decision

### 1. Per-item content cap on `search_memory`

Add a `maxItemContentChars` limit applied server-side before the tool response is
serialised. Content exceeding the limit is truncated with an ellipsis (`…`).

- Default: **500 chars** per item — 10 items × 500 ≈ 5 000 chars ≈ 1 250 tokens.
- Configurable via:
  - **Flag**: `--voice-item-content-limit` (int, default 500) — server-wide default.
  - **CR**: `voiceConfig.maxItemContentChars` in the `system_configuration` cell — per-deployment override; takes precedence over the flag when non-zero.
- `get_memory_items` is unaffected (it returns title-only rows, already compact).

### 2. Tool descriptions that discourage bulk fetches

Reword `get_memory_items` description to:
- Require `kind` to be specified (not optional).
- Steer the model toward `search_memory` for specific lookups.
- Make clear this tool returns titles only, not content.

## Implementation

Files to touch:
1. `api/v1alpha1/memorycell_types.go` — add `MaxItemContentChars int` to `VoiceSessionConfig`.
2. `cmd/xray/main.go` — add `--voice-item-content-limit` flag; pass to `NewVoiceManager`.
3. `internal/xray/voice.go` — add `maxItemContent int` field to `VoiceManager`; return limit from `loadSessionConfig` (CR wins over flag when non-zero).
4. `internal/xray/voice_tools.go` — pass limit into `buildVoiceTools` → `defSearchMemory`; apply cap; reword `get_memory_items` description.
5. `charts/xray-xray/values.yaml` — add `voice.itemContentLimit` (default 500).

## Consequences

- Tool responses are bounded; Gemini Live context cost is predictable.
- Items with long content can still be fully read via the UI (`GET /v1/memory/{id}`).
- The model can still get full content by calling `search_memory` with `limit=1` on
  a specific item — the cap only prevents bulk dumps.
- CR override allows tuning per deployment without a binary rebuild.
