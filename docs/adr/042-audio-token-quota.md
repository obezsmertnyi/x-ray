# ADR-042: Per-User Daily Quota on Real Audio Tokens

**Status:** Implemented  
**Date:** 2026-06-05  
**Supersedes:** [ADR-041](041-cost-weighted-token-quota.md) (cost-weighted token quota)  
**Builds on:** [ADR-040](040-per-modality-token-attribution.md) (per-modality split), [ADR-031](031-per-user-daily-token-limit.md) (the quota mechanism)

## Context

ADR-031 capped per-user voice spend on the fused `TotalTokenCount`; ADR-041 made
that cap cost-aware by metering a *weighted* token sum (`Σ weightₘ · tokensₘ`). The
weighted cap is fairer than raw tokens, but it has a fatal usability flaw:

**The weighted-token unit is synthetic — it appears nowhere in billing or the UI.**
You cannot open the Vertex console and reconcile "weighted tokens" against any SKU,
and the per-modality UI rows show raw counts, not the weighted number the cap uses.
The cap became a black box: you can't see why a user was throttled, or check the cap
against the bill.

The fix follows from one fact ADR-040's live data made undeniable: **audio *is* the
cost.** In the sample billing window, Live Audio Input + Audio Output were $0.46 of
the $0.56 total voice spend (~82%); all text in/out was ~$0.10 (rounding noise).
Capping a *synthetic* aggregate to control a cost that is ~entirely audio is
needless indirection.

## Decision

**Meter the per-user daily quota on real audio tokens** — the sum of audio-input
and audio-output tokens from the per-modality split (ADR-040) — instead of the
fused total or a weighted sum.

```
audioTurn = inByModality["audio"] + outByModality["audio"]
AddUserTokens(userHash, audioTurn)   // in emitTurnSpan
```

Everything else about the quota mechanism (ADR-031) is unchanged: the same daily
per-UTC-day counter, CR-status persistence, rollover, pre-session reject gate, and
mid-session farewell. Only the metered quantity changes, from `total` to
`audioIn + audioOut`.

**Why audio is the right base — and why it's better than weighting:**

- **Real and billable.** The metered number equals the Live Audio Input + Live
  Audio (AV2A) Output SKUs. You can reconcile the cap against the Vertex bill.
- **Visible.** The UI already renders `in audio` and `out audio` rows (ADR-040) —
  the user sees exactly the number being counted; no hidden weighted aggregate.
- **Captures ~all the cost.** Audio is ~82% of voice spend; the dropped text
  tokens are rounding noise, so a pure-audio cap is within a few percent of a
  fully cost-weighted one without any synthetic factor.
- **Single honest number.** audio-in and audio-out differ in unit price (~6× vs
  ~25× text), but audio-out *volume* is tiny (e.g. 472 vs 2,268 in a sample
  session), so summing them 1:1 barely moves the total. We deliberately do **not**
  re-weight in:out — that would reintroduce the synthetic factor this ADR removes.

**Configuration** — renamed to say what is metered:

- Flag `--user-daily-audio-token-limit` (replaces `--user-daily-token-limit`).
- CR `voiceConfig.maxUserDailyAudioTokens` (replaces `maxUserDailyTokens`).
- Chart `voice.userDailyAudioTokenLimit` (replaces `userDailyTokenLimit`).

The old flag/CR/chart names are **removed**, not aliased: they are inert in
production (the cap was never tightened beyond raw tokens) and keeping a
silently-different-unit alias is exactly the trap this ADR closes.

**The weighted-token machinery from ADR-041 is removed**: `TokenWeights`, the four
`--token-weight-*` flags, `voiceConfig.tokenWeights`, and `weightedTurnTokens`.
ADR-041 is superseded.

## Migration — the limit value must be re-picked

This is a **breaking change to what the limit means.** The deployed
`userDailyTokenLimit: 150000` was a raw-token budget; under audio metering the same
session meters far fewer tokens (audio is a fraction of each turn — e.g. ~700 audio
of ~1800 total). So 150k *audio* tokens is a much larger real budget than 150k raw
tokens was.

The chart default is re-picked to **50000 audio tokens/day**, anchored to cost:
audio-in ~$3/1M and audio-out ~$12/1M, so 50k audio tokens ≈ $0.15–0.30/user/day
depending on the in:out mix — comparable to the prior raw-token budget's real cost.
Operators tuning the cap now read a number they can verify directly against the
Audio SKUs in billing.

## Consequences

- **Reconcilable and visible.** The cap, the UI rows, and two billing SKUs are the
  same quantity. No synthetic unit anywhere.
- **Text is uncapped.** A pathological text-only user (e.g. pasting huge prompts)
  is not throttled by this quota. Acceptable: text is ~$0.10 of the sample bill,
  and other limits (max-request-bytes, headroom, concurrency) bound text surfaces.
  If text spend ever matters, a separate text cap can be added — but it would be a
  second *real* number, not a re-weighting.
- **Simpler than ADR-041.** One metered quantity, no weights to choose or tune, no
  unit translation in the UI.
- **Still a proxy, ~±5%.** Audio token granularity differs from the SKU meter
  (ADR-040), so the cap tracks audio cost within a few percent, not exactly.

## Alternatives Considered

- **Keep the ADR-041 weighted cap.** Rejected — the synthetic unit is invisible in
  billing and UI; the whole point here is a number the operator can see and verify.
- **Weight audio in:out (audioIn·1 + audioOut·4).** Rejected — reintroduces a
  synthetic multiplier for negligible accuracy gain (audio-out volume is tiny).
- **Cap audio + a separate text cap.** Deferred — text is rounding-noise cost
  today; add only if a real text-spend problem appears, as its own real number.
