# ADR-038: Headroom Token Compression for Search Tool Results

**Status:** Implemented  
**Date:** 2026-06-02  
**Supersedes:** the content-budget portion of [ADR-018](018-voice-tool-content-budget.md)

## Context

The largest text-token surface in X-Ray is **memory-cell content injected into
the model context as search tool results** — not the voice WebSocket transport
(audio frames are not context tokens). Three handlers serialize full
`MemoryItem.Content` (the Markdown body, the big field) straight into the LLM
context:

- `search_memory` (voice, `voice_tools.go`) — `{id, kind, title, content}` × up to 10.
- `search_knowledge` (MCP, `mcpserver.go`) — full `MemoryItem` JSON × up to 10, `MarshalIndent` (whitespace billed too).
- subject listings via `get_knowledge_context` — every item for a subject.

A single blog post or project description is thousands of characters; ten of
them per call is unbounded, mostly-redundant context spend. ADR-018 proposed a
naive per-item character cap to address this but was never implemented.

[Headroom](https://github.com/chopratejas/headroom) is a Python/Rust context-
compression layer. It is genuinely multi-provider (it ships `openai`, `gemini`,
`google`, `anthropic`, `cohere` provider modules and even its own MCP server,
`headroom/memory/mcp_server.py`) — so the obstacle is **not** that it only
speaks OpenAI or is client-only. The obstacle is simpler: **it has no Go
binding** (Python/Rust only), and our chokepoint is in-process Go. Running it
would mean adding a Python sidecar and a network hop to compress JSON we already
hold in memory — and its Gemini support targets the REST `generateContent` API,
not the bidirectional Gemini **Live** WebSocket our voice layer uses. Its
**mechanisms**, however, are deterministic algorithms that port cleanly to Go
and target exactly our chokepoint. We read its source (`smart_crusher`,
`search_compressor`, `adaptive_sizer`) and ported the transferable ideas
natively.

## Decision

Add an `internal/headroom` package — pure, deterministic, no model call, no new
runtime — implementing two Headroom-derived mechanisms, and wire it into the
voice `search_memory` chokepoint behind an opt-in budget knob.

### 1. LossyCompress (Headroom `SmartCrusher` per-field crush)

`LossyCompress(body, maxChars)` lossily compresses one content body: it strips
token-wasteful Markdown noise then truncates on a word boundary. It is lossy by
design (structure discarded, tail cut) — the result is a compact stand-in, not a
faithful copy. On the production corpus the rules that actually fire are:

- **inline links `[text](url)` → text** — 26 of 60 cells carry markdown links; dropping the URL is the dominant per-cell saving.
- fenced code ```` ```…``` ```` → `«code»`
- leading heading / blockquote / list markers
- collapsed whitespace runs

It also rewrites markdown image *syntax* `![alt](url)` → alt text, but that is a
no-op here: **zero** corpus cells embed image markdown in their `content`.
Actual images are stored in the separate `MemoryItem.Data["image"]` blob, which
the voice `search_memory` row projection (`{id, kind, title, content}`) never
serialized — so no image data ever reaches Gemini Live, before or after this
change. LossyCompress operates on `content` text only.

It returns a `truncated` flag. The full body is **always retrievable by ID** via
the item-fetch path — that existing path *is* Headroom's reversible "CCR"
contract, so no separate original-store is needed.

### 2. Field factoring (Headroom constant-field removal)

Fields uniform across every row (`kind`, `scope`) are hoisted into a shared
`common` header and blanked per-row, so they are paid for once. This moves a
value, it does not drop information — lossless.

### Wiring & configuration

- **Flag:** `--headroom-content-limit` (int, default **0 = disabled**) — the per-item LossyCompress budget in chars. Opt-in: zero Config is a pure passthrough.
- **CR override (voice):** `voiceConfig.maxItemContentChars` in `system_configuration`; wins over the flag per session when non-zero (resolved in `VoiceManager.ServeHTTP` at session start, mirroring `maxUserDailyTokens`).
- **Chart:** `voice.headroomContentLimit` (flag default, **500**) and `voice.systemConfig.maxItemContentChars` (CR override).

Scope — **voice `search_memory` ONLY:**

- **`search_memory` (voice):** LossyCompress + factoring. **No row dropping** — every search candidate reaches the model (voice mishears need them all for disambiguation). Rows carry `truncated`; the response carries a `common` header for factored fields. This is the only path whose tool results feed the LLM (Gemini Live).
- **MCP (`search_knowledge`, `get_knowledge_context`, `list_memory`): explicitly NOT compressed.** MCP exists to give external agents the **full** context, image blobs included. Truncating or dropping fields there is mutilation, not token reduction — and MCP is not the LLM-facing path. An earlier revision did compress `search_knowledge`; it was reverted. The headline figure it logged (96.6% reduction) was meaningless: ~28 KB of the "saving" was the `Data["image"]` base64 blob, which is a separate latent concern (a search result arguably should not ship images), not compression.

The full body of any voice result remains retrievable by ID via the existing
item-fetch path — that is Headroom's reversible "CCR" contract, so nothing is
permanently lost.

## Implementation

1. `internal/headroom/headroom.go` — `LossyCompress`, `CompressRows`, field `factor`, and the markdown-strip/truncate internals.
2. `api/v1alpha1/memorycell_types.go` — `MaxItemContentChars int` on `VoiceSessionConfig`.
3. `internal/xray/configwatcher.go` — parse `MaxItemContentChars` into the runtime config.
4. `cmd/xray/main.go` — `--headroom-content-limit` flag; passed to `NewVoiceManager` only.
5. `internal/xray/voice.go` — `headroomLimit` on `VoiceManager`; CR-wins resolution at session start; threaded through `newVoiceSession` → `buildVoiceTools` → `defSearchMemory`; session logger passed through for stats logging.
6. `internal/xray/voice_tools.go` — `defSearchMemory` applies `CompressRows` (LossyCompress + factor); rows gain `truncated`, response gains `common`; emits a `headroom compression` debug line per call (bytes in/out, tokens saved, reduction %, truncated count, factored).
7. `charts/xray-xray/` — `voice.headroomContentLimit` (default 500), `voice.systemConfig.maxItemContentChars`, deployment flag, CR template field.
8. `internal/xray/mcpserver.go` — **unchanged** (MCP deliberately not compressed).

## Consequences

### Measured reduction — real (production logs)

Live `search_memory` calls from a voice session against the production corpus
(60 cells, mean content ~370 chars), `limit=500`. From the `headroom
compression` debug log (text only; no image was ever on this path):

| query | items | bytes_in | bytes_out | reduction | tokens saved | notes |
|-------|-------|----------|-----------|-----------|--------------|-------|
| "Lunchflow project"          | 2 | 954  | 832  | 12.8% | ~30 | factored; 1 truncated |
| "projects by Huzefa Hamdard" | 2 | 1632 | 1265 | 22.5% | ~91 | factored; 2 truncated |
| "Code Radar"                 | 3 | 1809 | 1467 | 18.9% | ~85 | no factor; 0 truncated (all under 500 → saving is markdown-strip alone) |

So on short real cells the win is **~13–23%**. The 500-char cap rarely engages
(mean cell is ~370), so most saving comes from markdown-stripping and factoring.

### Measured reduction — synthetic (`internal/headroom` unit tests)

`CompressRows` run directly on long markdown-heavy rows — pure package tests, no
voice/MCP code involved. `TestComparison_BeforeAfter`, `TestLossyCompress_ReductionOnLongBody`:

| config | rows | bytes | reduction |
|--------|------|-------|-----------|
| OFF (baseline)                    | 12 | 15961 | — |
| LossyCompress + factor (limit=500) | 12 | 6817 | 57.3% |
| LossyCompress + factor (limit=200) | 12 | 3157 | 80.2% |
| single 1217-char body, LossyCompress(500) | — | 1217→500 | 58.9% |

Rows are never dropped (12 → 12); all reduction is markdown-stripping +
truncation + field factoring. The win scales with body length and how tight the
limit is: short cells → low double digits; long blog/project bodies at a 200–500
cap → 57–80%. Pinned by `TestComparison_BeforeAfter` (fails if ON ever exceeds
OFF).

### Other

- The per-item limit caps each result's content independently (~500 chars), not the whole response. Worst-case body size = limit × result count (search count limit 10, floored at 3 for voice mishears) ≈ ~1,250 tokens.
- Cost is sub-millisecond per result set (`BenchmarkCompressRows`) — negligible against an LLM round-trip.
- Off by default (`--headroom-content-limit=0`); enabling is one flag (chart default 500).
- Reversible: the model fetches any full body by ID; nothing is permanently lost.
- Deterministic and dependency-free — no model call, no Python/Rust runtime, no network hop.
- The earlier MCP-path figure (96.6%) is **not** included: it came from a reverted, non-LLM path and was inflated by a 28 KB image blob. It is not a headroom-compression result.

### Follow-up fix — 2026-06-20 (remote-MCP strip leak)

The **remote-MCP** result path (`fetchMCPTools`, distinct from the built-in
`search_memory` path) applied `LossyCompress` but only adopted its output `if
truncated` — so for any result that fit *under* the budget it discarded the
markdown-strip and returned the original (logging `0%`). That contradicted the
built-in path, which strips unconditionally, and leaked exactly the saving this
ADR identifies as dominant on the corpus (most cells are under the 500-char cap,
so the win is the strip, not the truncation). Compression+logging was extracted
to `compressMCPResult`, which **always** adopts the crushed form; `truncated` is
still logged to mean "and it was additionally capped to fit." Regression tests:
`TestCompressToolResult_StripsMarkdownWhenUnderBudget` (+ `_TruncatesOverBudget`,
`_ZeroLimitPassesThrough`).

### Follow-up fix — 2026-06-20 (native memsnap code tools were uncompressed)

When the xray-code graph was embedded as **native** in-process voice tools
(ADR-046, `buildMemsnapVoiceTools`), the headroom wiring was not carried over —
the file documented the omission as intentional on the theory that these return
"lean, self-capped JSON." Production voice logs disproved it: `get_code_node`
returned 2280-byte raw bodies and `search_code`/`search_docs` 2000–2300 bytes,
all uncompressed to Gemini Live. The payloads are exactly what headroom targets
— **markdown docs (ADR/README bodies), YAML manifests, code** — inside a thin
JSON envelope (`qn`/`file`/`line`/`calls`) that survives the markdown-noise strip
unchanged. All six handlers now route their result through the budget. As part
of this, the shared helper `compressMCPResult` was renamed `compressToolResult`
(it serves the built-in, native, and remote-MCP paths — never MCP-specific) and
its log line dropped the hardcoded `mcp=true`. The three `TestCompressMCPResult_*`
tests were renamed `TestCompressToolResult_*` accordingly.

### Follow-up fix — 2026-06-20 (budget narrowed to get_code_node)

The fix above wired the char budget into all six native tools uniformly. Live
logs then exposed the error: `search_code`/`search_docs` fired with
`truncated=true`, cutting ~2000-byte results to 500 (75–79% "reduction"). But
those tools return a **JSON array of N ranked pointers**, not one body — so the
flat char cap was not compressing, it was **truncating the array mid-list**,
dropping hits 2..N while the envelope still claimed `count: 5`. That is exactly
the row-dropping hazard this ADR rejected AdaptiveK to avoid, reintroduced
accidentally via a byte cap instead of row selection. The budget now applies to
**`get_code_node` only** — the single-body tool (whole ADR/README/source/YAML),
which is the genuine token-heavy payload. The other five are exempt and bounded
by their own list-aware limits: `search_code`/`search_docs` by `limit`,
`trace_code_calls` by `maxQueryGraphEdges`, `get_code_impact` by `max_nodes`;
`get_code_stats` is a small fixed counts object. Lesson: the char budget is a
**body** lever, not a **list** lever — never put it on a tool that returns an
array of results.

### Follow-up fix — 2026-06-20 (all content-returning mcell tools)

ADR-038 originally wired compression into `search_memory` only. Two other voice
tools return full memory-cell `content` to Gemini Live and were missed:
`get_pinned_items` and `get_agent_notes` (both `{count, items:[{id,…,content}]}`,
the same shape as search). Both now use the per-row `CompressRows` path — the
correct **list** lever (compress each body, factor uniform fields, NEVER drop a
row), distinct from the char budget that mauled the code search lists above. The
shared builder was extracted to `compressItemRows`; `search_memory` uses it too.
A full audit of the remaining tools confirms the surface is now complete:
`get_memory` (counts+summary), `get_memory_items` (deliberately content-free
browse list), and the write/control tools (`upsert_memory`, `promote_note`,
`delete_memory`, `note_observation`, `end_session`) return no cell content.

## Alternatives considered

- **Adopt the Headroom package** (library/proxy/MCP) — rejected on **one** real ground: no Go binding (it is Python/Rust), so using it means a Python sidecar + a network hop to compress JSON we already hold in-process. (It *is* multi-provider and *does* ship its own MCP server, so neither "OpenAI-only" nor "client-only" is a reason — those earlier framings were wrong.) Its Gemini support also targets the REST `generateContent` API, not the Live WebSocket our voice layer uses.
- **Naive char-cap only (ADR-018)** — superseded: blind truncation keeps markdown URL/fence noise. LossyCompress strips the noise first, compressing materially harder for the same budget.
- **AdaptiveK (Headroom `compute_optimal_k`: Kneedle knee on a unique-bigram coverage curve + SimHash near-duplicate row collapse)** — **prototyped, tested live, and removed.** It drops result *rows* judged near-duplicate. Two problems: (1) on the X-Ray corpus it omitted **zero** rows even when enabled and triggered (live `search_memory` over 9–10 results logged `omitted=0` — cells are genuinely distinct), so it computed SimHash fingerprints for no benefit; (2) dropping a row risks deleting a voice **mishear-disambiguation candidate** — two similarly-named items (e.g. a "LaunchFlow" platform vs. a "Launch Flow" event) SimHash as near-duplicates and one would be removed before the model sees it, producing a confident wrong answer with no "did you mean?". The ~80 LOC (`OptimalK`/`findKnee`/`uniqueBigramCurve`/`simhash`/`countUniqueSimhash`/`hamming`) plus `AdaptiveK`/`Bias`/`MinK`/`FastPathMax`/`Omitted` fields were cut. Row-count selection is left to the existing `search_memory` `limit` (default 10, floored at 3).
