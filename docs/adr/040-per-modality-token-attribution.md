# ADR-040: Per-Modality Voice Token Attribution (Traces, Metrics, UI)

**Status:** Implemented  
**Date:** 2026-06-05  
**Relates to:** [ADR-031](031-per-user-daily-token-limit.md) (the daily quota this observes), [ADR-007](007-otel-tracing-voice-sessions.md) (the span/metric pipeline this extends)

## Context

ADR-031 meters per-user Gemini Live spend as a single scalar:
`UsageMetadata.TotalTokenCount`, accumulated per user per UTC day and enforced as
a daily cap. `emitTurnSpan` (`voice.go`) likewise records only the fused
`PromptTokenCount` / `ResponseTokenCount` / `TotalTokenCount` on each turn span
and metric.

But Vertex AI bills Gemini Live as **five distinct SKUs at very different
rates**, and the fused counts collapse all of them:

| SKU | Per-1k cost (observed) |
|-----|------------------------|
| Live **Audio** Input | ~$3.0 |
| Live **Text** Input | ~$0.5 |
| Live Input Text Caching | ~$0.04 |
| Live **Audio** Output (AV2A) | ~$12 |
| Live **Text** Output | ~$0.4 |

Audio output is ~25× the per-token cost of text input; audio input ~6×. In a
sample billing window, **audio input alone was ~62% of voice spend** ($0.35 of
~$0.56). Consequences:

1. **Tokens ≠ cost.** Two users who hit the same `total_tokens` cap can differ in
   real cost by 5–10× depending on their audio/text mix. The daily cap bounds
   *volume*, not *spend*.
2. **No cost visibility.** Traces and metrics carried only the fused number, so
   there was no way to see *which modality* drives cost per user/session — the
   one dimension that actually maps to the bill.
3. **No reconciliation.** Trace token sums could not be lined up against the
   billing SKUs, because the SDK's single prompt/response scalar does not split
   audio vs text vs cache the way billing does.

The data to fix this is **already on every turn** — the SDK's
`*genai.UsageMetadata` carries `PromptTokensDetails`, `ResponseTokensDetails`,
`CacheTokensDetails` (each a `[]*ModalityTokenCount`) and
`CachedContentTokenCount`. `emitTurnSpan` simply ignored them.

This ADR is the **observe-first** step: surface the per-modality split everywhere
(traces, metrics, UI) without yet changing how the quota meters. A cost-weighted
cap is a deliberate follow-up (see Consequences) that builds on the same fields.

Per-**customer** cost attribution from the GCP billing *console* is out of scope
and not achievable there — it requires the BigQuery billing export with request
labels, or per-tenant projects. Tracked separately.

## Decision

Emit the per-modality token breakdown that Gemini Live already returns, in three
places, deterministically and with no extra model call.

**1. Traces** (`internal/xray/voice.go` — `emitTurnSpan`)
A new `modalitySplit([]*genai.ModalityTokenCount) map[string]int64` helper folds
each details slice into a `modality→tokens` map (lower-cased SDK modality string;
nil-tolerant; missing key reads as 0). Per-turn span gains:
- `gen_ai.usage.input_tokens.<modality>` (e.g. `.audio`, `.text`)
- `gen_ai.usage.output_tokens.<modality>`
- `gen_ai.usage.cached_tokens` — `CachedContentTokenCount`, emitted **separately**
  and **not** folded into `input_tokens` (it is a billed-at-discount subset of the
  prompt tokens; adding it would double-count).

**2. Metrics** (`internal/observability/otel.go`)
A new `RecordGenAITokenUsageByModality` records the existing
`gen_ai.client.token.usage` histogram with an added `gen_ai.modality` dimension
alongside the standard `gen_ai.token.type` (`input`/`output`). This keeps one
instrument; dashboards can now slice token usage by audio vs text and reconcile to
the Vertex Live SKUs.

**3. UI** (`ui/`)
`QuotaUsage` gains an optional `Breakdown *TokenBreakdown`
(`{inputAudio, inputText, outputAudio, outputText, cached}`) carrying the
**current session's** cumulative per-modality totals (the daily store keeps only a
fused total, so the live breakdown is session-scoped). `pushQuota` now sends the
breakdown **even when no daily limit is configured** (previously it no-op'd
entirely without a limit) — so cost visibility does not depend on a cap being set.
`StatsHud` renders `in audio / in text / out audio / out text / cached` rows when a
live breakdown is present.

Session-cumulative per-modality counters are added to `VoiceSession`
(`sessionInAudioTokens`, `sessionInTextTokens`, `sessionOutAudioTokens`,
`sessionOutTextTokens`, `sessionCachedTokens`) and incremented in `emitTurnSpan`.

## Consequences

- **Reconciliation is directional, not penny-exact.** Vertex meters audio
  (seconds→tokens) on a different granularity than the fused prompt/response
  scalar, so modality attributes reconcile to the SKUs within roughly ±5%, not to
  the token. This is attribution, not accounting.
- **Caching is reported, never summed.** `cached_tokens` is shown on its own as a
  discount marker, mirroring its separate (cheaper) SKU.
- **The quota is still cost-blind.** This ADR deliberately does not change
  `AddUserTokens` — it still meters raw `TotalTokenCount`. The new per-modality
  fields are exactly the inputs a future cost-aware cap would consume. That cap is
  the natural next ADR; this one makes the mix visible first.
- **No new instruments or flags.** Reuses the existing histogram and span
  pipeline; zero config surface added.

## Future Work — Per-Type / Cost-Weighted User Limits

The observability this ADR adds unlocks turning the per-user daily cap from
*volume*-based into *cost*-based. Three options, smallest to largest change, all
building on the per-modality split now available — to be chosen against observed
real-world mix:

1. **Weighted token cap (smallest).** Meter `Σ weightₘ · tokensₘ` into
   `AddUserTokens` instead of `TotalTokenCount`, with per-modality weights derived
   from the SKU price ratios (audio-output ≫ audio-input > text). Still one number
   and one cap, but now cost-proportional. Weights live in config — a new flag
   (`--token-weight-audio-out`, …) with CR override, mirroring how
   `MaxUserDailyTokens` overrides `--user-daily-token-limit` today (ADR-031).

2. **Dollar-denominated cap.** Carry a per-modality price table ($/1k tokens) and
   meter a `$`-equivalent so the cap reads "X USD/day" — the most intuitive limit
   for operators. Requires a price source kept current (config-supplied, since
   Vertex prices are not on the API).

3. **Independent per-type caps (most granular).** Separate daily budgets per
   modality — e.g. cap **audio-output** tokens specifically (the priciest lever),
   independent of text. Most precise control, but more per-user state to persist
   (the daily store would key by `(userHash, modality)`) and more to surface in the
   quota push and UI.

All three are additive and backward-compatible: with no weights/prices configured,
the cap falls back to today's fused-token behaviour. None is implemented here —
ADR-040 is the prerequisite observability, not the policy change. The work plan was removed once done.

Out of scope for any of the above: per-**customer** Vertex cost attribution, which
the GCP billing console cannot provide — it needs the BigQuery billing export with
request labels, or per-tenant projects.

## Alternatives Considered

- **Add the split only to metrics, not traces/UI.** Rejected — per-session/per-user
  trace attribution and an operator-facing UI readout were the whole point;
  metrics alone answer "how much audio overall" but not "for whom / this session".
- **Jump straight to a cost-weighted quota.** Rejected as premature — weights
  should be chosen against observed real-world mix, which requires this
  observability to exist first.
- **A separate `gen_ai.modality.token.usage` instrument.** Rejected — adding a
  dimension to the existing histogram keeps queries unified and avoids a parallel
  metric to keep in sync.
