# ADR-015: end_session Hangup Sequencing — Audio-Gated Close

**Status:** Implemented  
## Context

`end_session` is the voice tool that triggers graceful disconnect. The requirement
is that the model speaks a farewell *before* the session closes. Two bugs were
introduced and fixed along the way; this ADR records the invariant so it is never
broken again.

### Bug 1 — 9-second delay

**Root cause:** `hadModelOutput` was a local bool reset each iteration of the
`handleGeminiMessage` loop. `TurnComplete` arrives in a *separate* message from
the last audio chunk, so when the `TurnComplete` message was processed,
`hadModelOutput` was `false` — the hangup check was gated on it and never fired.

**Fix:** Made `TurnComplete` unconditionally set `turnIdle=true` and close the
hangup if `pendingHangup` was set. This removed the delay but introduced Bug 2.

### Bug 2 — silent drop, no goodbye

**Root cause:** `TurnComplete` now fired the hangup unconditionally. After the
model calls `end_session`, Gemini sends a silent `TurnComplete` to acknowledge
the tool response. With `pendingHangup=true` already set, that silent
`TurnComplete` closed the session before the model had a chance to speak a
farewell. The user heard nothing — the call just dropped.

Additionally, the tool handler itself had an early-close path:
```go
if turnIdle.Load() {
    close(hangup)  // fired immediately on end_session call
}
```
This closed the channel as soon as `end_session` was called, before the model
could speak.

## Decision

**The hangup channel must only be closed when the model has produced audio in the
current turn AND `TurnComplete` arrives.**

Implementation (`voice.go`):

```
turnHadAudio atomic.Bool
  — set to true on any audio chunk or output transcription within the current turn
  — reset to false (via Swap) when TurnComplete is processed

On TurnComplete:
  hadAudio := s.turnHadAudio.Swap(false)
  s.turnIdle.Store(true)
  if hadAudio && s.pendingHangup.Load() {
      close(s.hangup)
  }
```

The `end_session` tool handler only sets `pendingHangup=true`. It never closes
the hangup channel directly.

## Invariant

> The hangup channel is closed exactly once, on the first `TurnComplete` that
> arrives **after** `pendingHangup` is set **and** that turn produced model audio.

This means:
- Silent tool-response `TurnComplete` messages never trigger hangup.
- The model always speaks its goodbye turn; only after that turn's audio is
  delivered and `TurnComplete` arrives does the session close.
- `turnIdle` remains set to `true` after `TurnComplete` regardless of audio, so
  status queries are accurate.

## Consequences

- Goodbye latency = time for model to generate and stream farewell audio (~1–2 s).
- If the model calls `end_session` but produces no audio in any subsequent turn
  (e.g. Gemini returns an empty response), the session will not close automatically.
  This is an acceptable edge case — the browser WebSocket timeout will clean it up.
- Do not add any early-close logic to the `end_session` tool handler. All hangup
  decisions live in `handleGeminiMessage`.


## Addendum: `end_session` goes unanswered (ADR-058)

The sequencing above hangs up when a turn completes with the hangup pending. That
left the model a turn after the tool response, and it filled that turn with
speech: a bare status was read aloud, and an instruction to stay silent was spoken
("no speech"). `end_session` is now answered by hanging up: the call gets no tool
response and the session closes after the same four-second drain window. The
turn-complete path stays for a hangup set without a tool call (the quota farewell).
The channel is closed through a `sync.Once`, because both paths can fire.
