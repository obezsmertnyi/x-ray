# ADR-039: Voice Session Longevity & Conversation Quality — GoAway, Resumption, Compression, Affective Dialog, Proactive Audio

**Status:** Accepted
**Date:** 2026-06-03
**Todo:** [voice-session-longevity](../todo/voice-session-longevity.md)

> **Changes (2026-06-09):** The Google AI non-native model referenced below as an
> example, `gemini-2.0-flash-live-001`, was retired by Google on 2025-12-09. Both
> backends now use `google/gemini-live-2.5-flash-native-audio`. The longevity
> design below is unchanged; only the example model identifier is dated.

## Context

Gemini Live imposes hard server-side limits on a single bidirectional session
([Live API session management](https://docs.cloud.google.com/vertex-ai/generative-ai/docs/live-api/start-manage-session)):

- **Audio-only sessions: ~15 minutes maximum.** (Audio+video: ~2 min — not
  relevant; X-Ray is audio-only, `ResponseModalities: [AUDIO]`, `voice.go:480`.)
- **Underlying connection lifetime: ~10 minutes**, after which the socket is
  recycled even mid-conversation.
- **Context window: 128k tokens**, compaction trigger configurable in
  `[5k, 128k]`.

Before any of these fire, the server emits a **`GoAway`** message
~60 s ahead of termination (`LiveServerMessage.GoAway`, with
`LiveServerGoAway.TimeLeft time.Duration` — both present in our pinned SDK
`google.golang.org/genai@v1.47.0`).

**What X-Ray does today.** `handleGeminiMessage` (`voice.go:681`) switches on
`ToolCall`, `ServerContent.Interrupted`, `ModelTurn`, transcriptions, and
`TurnComplete`. It does **not** inspect `msg.GoAway` or
`msg.SessionResumptionUpdate`, and `LiveConnectConfig` (`voice.go:479`) sets
neither `SessionResumption` nor `ContextWindowCompression`. Net effect: a voice
conversation that crosses ~15 minutes (or ~10 on connection churn) **drops
abruptly** — the WebSocket read returns an error, `readFromGemini` (`voice.go:671`)
propagates it, and the session tears down with no warning to the browser and no
recovery. The user is simply cut off mid-sentence.

This is a real, deterministic limit, not a latency micro-optimization. It will
affect any genuinely long conversation. The fix is the session-management
toolkit the Live API provides for exactly this purpose, all available in the SDK
we already depend on.

> Scope note: this ADR is about **session longevity** — surviving the time and
> connection caps. It deliberately does **not** adopt asynchronous
> (`NON_BLOCKING`) tool calls: X-Ray's tool handlers are in-process and
> sub-millisecond-to-low-millisecond (`search_memory` ~9–30 ms over the local
> HNSW/keyword path), so blocking dispatch is fine. Async tools solve slow
> *remote* I/O, which X-Ray's first-party tools don't have. (Remote MCP tools
> via `buildRemoteMCPTools` could, someday — left out of scope here.)

Two further `LiveConnectConfig` fields — unrelated to the caps above but in the
same config literal and the same native-audio model — improve **conversation
quality** rather than longevity:

- **`EnableAffectiveDialog *bool`** (`types.go:6412`): "the model will detect
  emotions and adapt its responses accordingly." Native-audio only — which is
  exactly the Vertex model X-Ray runs (`gemini-live-2.5-flash-native-audio`).
- **`Proactivity *ProactivityConfig{ ProactiveAudio *bool }`** (`types.go:6438`,
  `6233`): lets the model "reject responding to the last prompt… ignore out of
  context speech or stay silent if the user did not make a request yet."

Both are single `*bool`s on the **same** config literal at `voice.go:479`, so
they cost almost nothing once ADR-039's config plumbing exists — bundling them
here avoids a second round of identical flag/CR/chart wiring. They are persona
behavior, not plumbing; kept off by default so the persona is unchanged until
deliberately enabled.

## Decision

Add five independent, individually flag-gated capabilities to the voice layer —
three for longevity (1–3), two for conversation quality (4–5). Each is off by
default and additive; none changes behavior when its flag is zero/false.

### 1. GoAway awareness (always on; cheap, observability-only)

Handle `msg.GoAway` in `handleGeminiMessage`. On receipt:

- Log at warn with `time_left`.
- Record a span event / metric (`voice.goaway`, `time_left_ms`) for
  observability — today these terminations are invisible in traces.
- Notify the browser via a new `VoiceServerMessage` field
  (`SessionEnding *struct{ Reason string; TimeLeftMs int64 }`) so the UI can show
  a subtle "reconnecting…" affordance rather than a silent drop.

This alone turns an invisible abrupt cut into an *observed, explained* event,
and is the prerequisite for (2).

### 2. Transparent session resumption (flag-gated)

Set `LiveConnectConfig.SessionResumption = &genai.SessionResumptionConfig{}` so
the server periodically emits `LiveServerSessionResumptionUpdate` with a
`NewHandle`. Persist the **latest** handle on the `VoiceSession`. On a `GoAway`
(or on a connection-level `Receive` error while a handle is held), reconnect
with `SessionResumptionConfig{Handle: lastHandle}` — the new upstream session
resumes with **full context intact**, so the conversation continues across the
socket recycle.

- **Resumption window:** ~10 min; **max total session:** ~24 h (server-enforced).
- The reconnect is internal to `VoiceSession`: the **browser** WebSocket stays
  open throughout; only the **upstream Gemini** `*genai.Session` is swapped.
  Audio buffering on the browser side already tolerates brief gaps.

**Reconnect mechanics — the part that needs care.** The current teardown is
trigger-on-first-error: `run` (`voice.go:587`) `select`s on a buffered `errCh`,
and the first non-nil error from *either* `readFromBrowser` or `readFromGemini`
runs `s.cancel(); s.live.Close()` and ends the session. Two consequences the
implementation must respect:

1. **`readFromGemini` must not surface a recoverable error.** When `Receive()`
   errors *and* a resumption handle is held *and* resumption is enabled, the
   loop must attempt `reconnect()` in place and continue — never `return err`
   (which would trip the `errCh` select and tear down). Only a failed reconnect,
   a cancelled context, or resumption-disabled returns the error (today's path).
2. **The swap must be serialized against sends.** `readFromBrowser` calls
   `s.live.Send*` under `s.liveMu` (`voice.go:630,640,651`). `reconnect()` must
   take `s.liveMu`, `Close()` the old `*genai.Session`, `Connect()` a new one
   with the stored handle, and assign `s.live` before releasing — so an in-flight
   send never targets a half-closed session. `Receive()` is **not** under
   `liveMu` (only one goroutine reads), so the read loop simply observes the new
   `s.live` after the swap returns.

Concretely, `readFromGemini` becomes:

```go
msg, err := s.live.Receive()
if err != nil {
    if s.canResume() {           // resumption on AND handle held AND ctx live
        if rerr := s.reconnect(ctx); rerr == nil {
            continue             // resumed; keep reading on the new session
        }
    }
    return fmt.Errorf("gemini receive: %w", err)
}
```

GoAway (capability 1) is the *proactive* trigger: on `msg.GoAway`, call
`reconnect()` before the socket actually closes, so there is no read-error gap at
all. The reactive `Receive`-error path above is the safety net for connection
recycles that arrive without a GoAway.

- Flag: `--voice-session-resumption` (bool, default **false**). Off = today's
  behavior exactly: `canResume()` is always false, `readFromGemini` returns the
  error, session tears down (byte-identical to current code).

### 3. Context window compression (flag-gated)

Set `LiveConnectConfig.ContextWindowCompression` with a sliding window so long
sessions don't hit the 128k context ceiling:

```go
ContextWindowCompression: &genai.ContextWindowCompressionConfig{
    TriggerTokens: ptr(int64(triggerTokens)),
    SlidingWindow: &genai.SlidingWindow{TargetTokens: ptr(int64(targetTokens))},
},
```

The server keeps the system instruction + a suffix of recent turns, dropping the
oldest USER-turn-aligned prefix when `TriggerTokens` is crossed. Combined with
(2), this enables effectively unbounded session duration.

- Flags: `--voice-context-trigger-tokens` (int, default **0 = disabled**) and
  `--voice-context-target-tokens` (int, default **0 → server uses trigger/2**).
  Trigger must be in `[5_000, 128_000]` when non-zero; validated at startup.

### 4. Affective dialog (flag-gated) — *conversation quality*

Set `LiveConnectConfig.EnableAffectiveDialog = ptr(true)` (`types.go:6412`). The
model detects emotional tone in the user's audio and adapts its responses. This
is a **native-audio** feature, supported by X-Ray's Vertex model
(`gemini-live-2.5-flash-native-audio`).

- Flag: `--voice-affective-dialog` (bool, default **false**).
- Caveat: only meaningful on native-audio models; if X-Ray is ever pointed at a
  non-native model (e.g. the Google AI `gemini-2.0-flash-live-001` path), the
  server may reject or ignore it. Resolve against `liveModel` and skip-with-warn
  if the model is not native-audio, rather than sending an invalid config.

### 5. Proactive audio (flag-gated) — *conversation quality*

Set `LiveConnectConfig.Proactivity = &genai.ProactivityConfig{ProactiveAudio: ptr(true)}`
(`types.go:6438`, `6233`). The model may decline to respond to input that isn't
directed at it — ignoring out-of-context/background speech and staying silent
when the user hasn't actually asked anything. Useful in an open-mic X-Ray
session where ambient speech would otherwise trigger spurious replies.

- Flag: `--voice-proactive-audio` (bool, default **false**).
- Same native-audio caveat as (4); guard on `liveModel` identically.
- Interaction with VAD: this is orthogonal to `AutomaticActivityDetection`
  (`voice.go:490`) — VAD decides *when speech starts/stops*; proactive audio
  decides *whether to answer at all*. Both can be on together.

### Configuration summary

| Knob | Flag | CR override (`voiceConfig`) | Chart (`values.yaml`) | Default |
|------|------|------------------------------|------------------------|---------|
| Resumption | `--voice-session-resumption` | `sessionResumption: bool` | `voice.sessionResumption` | false |
| Compaction trigger | `--voice-context-trigger-tokens` | `contextTriggerTokens: int` | `voice.contextTriggerTokens` | 0 (off) |
| Compaction target | `--voice-context-target-tokens` | `contextTargetTokens: int` | `voice.contextTargetTokens` | 0 (trigger/2) |
| Affective dialog | `--voice-affective-dialog` | `affectiveDialog: bool` | `voice.affectiveDialog` | false |
| Proactive audio | `--voice-proactive-audio` | `proactiveAudio: bool` | `voice.proactiveAudio` | false |

CR overrides win over the flag per session when non-zero/set, mirroring the
existing `maxUserDailyTokens` / `maxItemContentChars` resolution in
`VoiceManager.ServeHTTP` (ADR-031 / ADR-038 pattern).

## Implementation

The `LiveConnectConfig` to factor out lives at `voice.go:479` (the
`Live.Connect` call inside `newVoiceSession`). Both `SessionResumption` and
`ContextWindowCompression` are *additional fields on the existing literal* — no
structural change. The reconnect helper must rebuild that same config (system
prompt, tools, speech, VAD) plus the stored handle, so extract it into a
`buildLiveConfig()` method to avoid duplicating the ~20-line literal.

1. `internal/xray/voice.go` — the bulk of the change:
   - **`VoiceSession` struct (`voice.go:357`)**: add
     `resumptionHandle string` + reuse `liveMu` to guard it (it already guards
     everything touching `s.live`), and a `resumptionEnabled bool` /
     `ctxTrigger, ctxTarget int` set at construction.
   - **`handleGeminiMessage` (`voice.go:681`)**: add
     `if msg.GoAway != nil { … }` (log `TimeLeft`, notify browser, proactively
     `reconnect` if enabled) and
     `if msg.SessionResumptionUpdate != nil { store NewHandle }` cases alongside
     the existing `Interrupted` / `TurnComplete` cases.
   - **`readFromGemini` (`voice.go:663`)**: wrap the `Receive` error in the
     `canResume()` → `reconnect()` → `continue` guard shown in the Decision.
   - **`reconnect(ctx)` (new)**: under `s.liveMu`, `s.live.Close()` the old
     session, `Live.Connect` a new one via `buildLiveConfig()` with
     `SessionResumption.Handle = s.resumptionHandle`, assign `s.live`.
   - **`buildLiveConfig()` (new, extracted from `voice.go:479`)**: returns the
     `*genai.LiveConnectConfig`; conditionally sets `SessionResumption`,
     `ContextWindowCompression`, `EnableAffectiveDialog`, and
     `Proactivity.ProactiveAudio` from the resolved per-session config. The two
     native-audio fields (4, 5) are gated on `isNativeAudioModel(s.liveModel)` —
     skip-with-warn on non-native models rather than sending invalid config.
   - **`VoiceServerMessage` (`voice.go:413`)**: add
     `SessionEnding *SessionEnding` with `Reason string; TimeLeftMs int64`
     (this struct is here, *not* in `server.go`).
2. `api/v1alpha1/memorycell_types.go` — `VoiceSessionConfig`: add
   `SessionResumption *bool`, `ContextTriggerTokens int`, `ContextTargetTokens int`,
   `AffectiveDialog *bool`, `ProactiveAudio *bool`.
3. `internal/xray/configwatcher.go` — parse the five new CR fields into the
   runtime config (mirror `MaxItemContentChars` from ADR-038).
4. `cmd/xray/main.go` — five flags; thread into `NewVoiceManager` (same call
   site as `--headroom-content-limit`).
5. `internal/xray/voice.go` (`VoiceManager` + `ServeHTTP`): hold the flag
   defaults; resolve CR-wins per session (the block that already resolves
   `maxUserDailyTokens` / `maxItemContentChars`); pass into `newVoiceSession`
   (extend its signature — currently ends at `headroomLimit, logger`,
   `voice.go:446`).
6. `charts/xray-xray/`: `voice.sessionResumption`,
   `voice.contextTriggerTokens`, `voice.contextTargetTokens`,
   `voice.affectiveDialog`, `voice.proactiveAudio` in `values.yaml`, plus the
   deployment-template flags and the `system_configuration` CR template fields.
7. UI (`ui/`): handle the new `SessionEnding` message — non-blocking toast /
   "reconnecting" indicator. **Optional**; resumption is server-transparent and
   works with the UI untouched.

## Consequences

- **Long conversations survive.** With resumption on, the ~10-min connection
  recycle and ~15-min audio cap become transparent; with compaction on, the
  128k ceiling stops truncating context. Together: sessions up to the server's
  ~24 h ceiling.
- **Terminations become observable.** Even with both features off, GoAway
  handling (1) surfaces a previously invisible failure mode in logs/traces.
- **Cost note.** Longer sessions accrue more tokens — but per-user daily token
  quota (ADR-031) already caps spend, so longevity does not bypass cost control.
  Compaction *reduces* per-turn context cost on long sessions.
- **Backward compatible.** All five knobs default off/zero; with defaults,
  behavior is byte-identical to today except for the added GoAway log line and
  browser notification (purely additive).
- **Two concerns, one commit-worth of wiring.** Capabilities 1–3 (longevity) and
  4–5 (conversation quality) are independent and could be separate ADRs. They are
  bundled only because they share one config literal (`voice.go:479`) and one set
  of flag/CR/chart plumbing; splitting them would duplicate that wiring for two
  `*bool`s. If 4–5 prove to need richer tuning later (e.g. per-persona affective
  profiles), they can graduate to their own ADR.
- **Persona behavior change (4–5).** Affective dialog and proactive audio alter
  how the persona *sounds and decides to respond*, not just plumbing — they
  warrant a listen-test before enabling in production. Proactive audio in
  particular can make the agent *decline* to answer; validate it doesn't swallow
  legitimate quiet/short requests.
- **Latency.** Sliding-window compaction has a one-time per-trigger latency cost
  (server-side); the SDK comment notes `TargetTokens` should be `< TriggerTokens`
  to avoid running it every turn. Resumption reconnect is a single extra
  `Connect` round-trip, hidden behind buffered browser audio.

### Risks specific to reconnect (capability 2)

The reconnect path is the only genuinely delicate part; the rest is additive
config. Known sharp edges to cover in implementation/tests:

- **Reconnect mid-turn.** If GoAway/recycle lands while the model is mid-
  utterance, the in-flight audio is lost and the resumed session continues from
  the last committed turn. Mitigation: prefer the *proactive* GoAway trigger
  (60 s of warning) over the reactive read-error path, and reconnect at the next
  `TurnComplete` when one is imminent rather than interrupting active audio.
- **Send during swap.** `readFromBrowser` could be blocked on `s.liveMu` while
  `reconnect()` holds it; that send then targets the *new* session, which is
  correct. But a send that already passed the mutex and is in-flight on the old
  socket when GoAway arrives may error — `readFromBrowser` must treat a send
  error as recoverable (retry-after-reconnect or drop the frame) rather than
  returning it and tripping teardown, symmetric to the `readFromGemini` guard.
- **Reconnect storm.** A handle that the server rejects (expired past the ~10-min
  window, or a persistently broken upstream) must **fail closed after one
  attempt** — fall through to today's teardown rather than loop `Connect`. No
  unbounded retry. Test this explicitly.
- **Token accounting.** `UsageMetadata`-driven counters (`sessionTotalTokens`,
  ADR-031 quota) must keep accumulating across the swap, not reset — the
  `VoiceSession` outlives the `*genai.Session`, so the atomics already survive;
  just confirm no counter is tied to the connection.

### Phasing

Capability 1 (GoAway awareness) is **independently mergeable and valuable on its
own** — it has no reconnect machinery, only adds a log line + browser message,
and converts today's silent drop into an observed event. Capabilities 2 and 3
can land in the same commit or a follow-up. If splitting, 1 ships first.

## Alternatives considered

- **Do nothing / let sessions drop at 15 min.** Rejected: it's a real
  user-facing cut-off with no warning. Even if (2) and (3) are deferred, (1) is
  near-free and removes the silent-failure problem.
- **Client-side full reconnect (new session, no handle).** Rejected: loses all
  conversation context — the user has to re-establish what they were talking
  about. Resumption handles preserve context, which is the entire point.
- **Asynchronous (`NON_BLOCKING`) tool calls** (`FunctionDeclaration.Behavior`,
  `FunctionResponse.Scheduling`). Rejected for this ADR: X-Ray's first-party
  tools are sub-30 ms in-process calls, so blocking dispatch is not a bottleneck.
  Revisit only if slow remote MCP tools become a primary path.
- **Raising the per-session token cap instead of compaction.** Not an
  alternative — the 128k limit is server-enforced; the only lever is the
  server's own compaction mechanism.
