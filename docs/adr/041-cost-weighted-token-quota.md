# ADR-041: Cost-Weighted Per-User Voice Token Quota

**Status:** Superseded by [ADR-042](042-audio-token-quota.md)  
**Date:** 2026-06-05  
**Builds on:** [ADR-040](040-per-modality-token-attribution.md) (the per-modality split this consumes), [ADR-031](031-per-user-daily-token-limit.md) (the daily quota this re-meters)

> **Superseded by ADR-042.** The weighted-token cap was shipped but never enabled
> (weights left unset → fused-total fallback). Its fatal flaw: the weighted-token
> unit appears nowhere in billing or the UI, so the cap was unverifiable. ADR-042
> replaces it by metering **real audio tokens** — a billable, UI-visible quantity —
> and removes the `TokenWeights` machinery this ADR added.

## Context

ADR-031 caps per-user Gemini Live spend by metering the fused
`UsageMetadata.TotalTokenCount` into a per-user, per-UTC-day counter and rejecting
sessions once it crosses the limit. ADR-040 then split that spend by modality in
traces/metrics/UI, and the live data confirmed the problem: a spoken turn carries
both text and **audio** input tokens (e.g. 1047 text + 723 audio observed), and
Vertex bills audio-input ~6× and audio-output ~25× the text-input rate. Audio
input alone was ~62% of a sample voice bill.

So the cap meters *volume* while cost is *modality-weighted*. Two users at the same
`total_tokens` cap can differ 5–10× in real spend; a heavy-audio user is
under-charged against the cap and a text-only user over-charged.

ADR-040 was deliberately observe-first. The per-modality counts it surfaced are
now metered on every turn — this ADR consumes them to make the cap cost-true.

## Decision

Meter a **cost-weighted token sum** into the per-user daily counter instead of the
raw fused total, using per-modality weights derived from the Vertex Live SKU price
ratios.

**Weights** — a `TokenWeights` struct `{InAudio, InText, OutAudio, OutText
float64}` on `VoiceManager`, resolved at session start: CR
`VoiceConfig.tokenWeights` overrides the four `--token-weight-*` flags when set
(same flag-default → CR-override pattern as `MaxUserDailyTokens` / ADR-031).

**Metering** — in `emitTurnSpan`, replace
`AddUserTokens(userHash, total)` with `AddUserTokens(userHash, weightedTokens)`
where

```
weightedTokens = round( InText·inText + InAudio·inAudio
                      + OutText·outText + OutAudio·outAudio )
```

computed from the per-modality split ADR-040 already produces. Cached tokens are a
discounted subset of the prompt count and are **not** added separately (avoids
double-counting); they remain visible as their own trace/UI line.

**Backward compatibility — the key constraint.** The default for every weight is
`1.0`. With all weights at 1.0 the formula collapses to
`inText+inAudio+outText+outAudio`, which equals the fused input+output for any turn
whose tokens are wholly text/audio (the only modalities voice produces). To
guarantee *exactly* the ADR-031 number when unconfigured — including any future
turn that reports a modality outside {text, audio}, or a backend that omits the
details slices — metering falls back to the fused `total` whenever the resolved
weights are all 1.0. Weighted metering engages only when an operator sets a
non-unit weight. Result: zero behavioural change on existing deployments; opt-in
cost-weighting.

**Everything downstream is unchanged.** The store, daily rollover, CR persistence,
reject gate, mid-session farewell, and UI quota push all still operate on one
integer per user per day — they just receive a cost-proportional number. The
`tokens X / limit` UI readout now reads in *weighted* (cost-equivalent) units when
weighting is active; the per-modality breakdown rows (ADR-040) continue to show
raw counts for transparency.

## Consequences

- **Opt-in and safe.** Unset (or all-1.0) weights reproduce ADR-031 exactly via the
  fused-total fallback. No migration, no surprise tightening of existing caps.
- **The limit unit changes meaning when engaged.** With weights set, `limit` is a
  budget of *cost-equivalent tokens*, not raw tokens — operators must re-pick the
  limit to match (e.g. if audio-out weight is 25, a mostly-spoken user exhausts a
  given limit far faster). Documented on the flags.
- **Still a proxy, not billing.** Weights are price *ratios*, not live prices, and
  audio token granularity differs from the SKU meter (~±5%, per ADR-040). This is a
  fairer cap, not an accounting system.
- **Weights are a tuning surface.** They can be changed at runtime via the CR (no
  restart), like every other voice-session knob.

## Alternatives Considered

- **Dollar-denominated cap** (meter a `$`-equivalent, limit reads "USD/day").
  Rejected for now — needs a maintained price table and a unit change in the UI;
  the weighted-token cap gets ~all the fairness with a one-line meter change and no
  new units to source. Left as a future option in the todo.
- **Independent per-modality caps** (separate audio-out budget, etc.). Rejected as
  premature — more per-user state (`(userHash, modality)` keys) and UI for marginal
  gain over a single weighted budget. Future option.
- **Always meter weighted, default weights 1.0, no fused fallback.** Rejected — a
  turn reporting an unmapped modality (image, or an unnamed one) would then be
  *undercounted* vs ADR-031. The fused fallback at unit weights is strictly safer.
