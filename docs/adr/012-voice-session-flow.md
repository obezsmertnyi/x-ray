# ADR-012: Voice Session End-to-End Flow

**Status:** Implemented  
> **Changes (2026-06-09):** The Google AI backend model named below,
> `gemini-2.0-flash-live-001`, was retired by Google on 2025-12-09. Both backends
> now use `google/gemini-live-2.5-flash-native-audio`, resolved from config only
> (`--voice-model` flag default → `system_configuration.voiceModel` CR), not a
> compiled-in constant. The flow below is unchanged.

## Overview

A voice session is a bidirectional audio proxy: raw PCM from the browser mic is streamed over WebSocket to the Go backend, which relays it to Gemini Live (Vertex AI or Google AI). Gemini returns audio chunks, transcripts, and tool calls. The backend fans those back to the browser in real time. No audio is buffered server-side.

## Data flow

```
Browser mic (16 kHz PCM)
  → AudioWorklet (Float32 frames)
    → Int16 base64 JSON  {"audio": "<base64>"}
      → WebSocket  wss://<host>/v1/voice
        → readFromBrowser  →  live.SendRealtimeInput
          → Gemini Live (Vertex AI / Google AI)
            → readFromGemini  ←  live.Receive()
              ↓ ToolCall  →  dispatch  →  MemoryStore  →  SendToolResponse  →  Gemini
              ↓ Audio chunks (24 kHz PCM)  →  WebSocket  →  browser AudioContext
              ↓ Transcripts                →  WebSocket  →  speech bubbles
              ↓ UsageMetadata              →  OTEL spans
```

## UI layer (`ui/hooks/use-voice-session.ts`)

`useVoiceSession({ enabled })` is the single hook that owns the entire session lifecycle.

**Session open** (when `enabled` flips true):
1. `getUserMedia({audio})` — mono, 16 kHz, echo/noise cancellation on.
2. Create capture `AudioContext` at 16 kHz; load an inline `AudioWorklet` (`PcmCaptureProcessor`).
3. Open `WebSocket` to `/v1/voice` (`wss:` if HTTPS, `ws:` otherwise).
4. Wait for `ws.onopen`; set status → `active`.
5. Create playback `AudioContext` at 24 kHz (matches Gemini's output rate).
6. Connect mic source → worklet node; worklet fires `onmessage` per frame.

**Outbound audio loop** (per worklet frame):
- Convert Float32 → Int16 PCM, base64-encode, send `{"audio": "<b64>"}` over WS.

**Inbound message handling** (`ws.onmessage`):
| Field | Action |
|---|---|
| `audio` | base64 decode → Int16 → Float32 → schedule on playback `AudioContext` queue |
| `transcript` | buffer agent speech chunks; flush after 600 ms silence into a bubble |
| `inputTranscript` | flush agent buffer first, then add user speech bubble |
| `interrupt` | stop all queued playback sources; clear agent transcript buffer |
| `done` | set status → `idle`; call `onDone()` callback |
| `error` | set error state |

**PTT / end-of-speech**: `stopMic()` disconnects the worklet and sends `{"audioStreamEnd": true}` without closing the WebSocket — Gemini responds, then `end_session` tool triggers graceful hangup.

**Session close**: cleanup in `useEffect` return — close mic tracks, disconnect worklet, close both `AudioContext`s, close WS.

## Backend: session setup (`internal/xray/voice.go`)

`VoiceManager.ServeHTTP` (route `GET /v1/voice`):

1. **`ensureClientLocked`** (line 95) — lazily constructs `genai.Client` once per process:
   - `--gcp-project` set → Vertex AI backend, model `google/gemini-live-2.5-flash-native-audio`
   - `--gemini-api-key` set → Google AI backend, model `gemini-2.0-flash-live-001`
2. **`loadSessionConfig`** (line 72) — reads the `system_configuration` memory cell (subject=`xray`) per session:
   - Cell content → system prompt
   - Cell tags → tool allowlist, `voice:<name>`, `model:<id>`, `greeting:<text>` overrides
   - Flag overrides (`--voice-name`, `--voice-model`) take precedence over cell values
3. **WebSocket upgrade** — gorilla `wsUpgrader`; assigns session ID `vs-<n>-<hex>`.
4. **`newVoiceSession`** (line 250):
   - Builds `[]voiceToolDef` via `buildVoiceTools` (see Tools section).
   - Calls `genaiClient.Live.Connect` with `LiveConnectConfig`:
     - `ResponseModalities: [Audio]`
     - `SystemInstruction`: loaded system prompt
     - `Tools`: function declarations derived from the tool defs
     - `SpeechConfig`: prebuilt voice name (default `Kore`)
     - `InputAudioTranscription` + `OutputAudioTranscription` both enabled
     - VAD: `StartSensitivityHigh`, `EndSensitivityHigh`, silence threshold 400 ms
   - Optionally sends a `greeting` as the first user turn.
   - Spawns `run()` goroutine; returns `*VoiceSession`.

## Backend: bidirectional pump (`voice.go:327`)

`run()` opens an OTEL `voice.session` span, then starts two goroutines and selects on the first to exit (or `hangup` / context cancel):

**`readFromBrowser` (line 387)**
Reads `VoiceBrowserMessage` JSON from the WebSocket:
- `audio` bytes → `live.SendRealtimeInput({Audio: Blob{MIMEType:"audio/pcm;rate=16000"}})`
- `audioStreamEnd` → `live.SendRealtimeInput({AudioStreamEnd: true})`
- `text` → `live.SendClientContent({Turns: [text content]})` (text injection path)

**`readFromGemini` (line 434)**
Loops on `live.Receive()`:
- `ToolCall` → `handleToolCall` (see Tools section)
- `ModelTurn.Parts[InlineData]` → audio chunk → `WriteJSON({Audio: bytes})` to browser
- `OutputTranscription.Text` → `WriteJSON({Transcript: text})` to browser
- `InputTranscription.Text` → `WriteJSON({InputTranscript: text})` to browser
- `Interrupted` → `WriteJSON({Interrupt: true})` to browser
- `TurnComplete` + `pendingHangup` set → close `hangup` channel → `run()` exits cleanly
- `UsageMetadata` → `emitTurnSpan` → OTEL `llm.gemini_realtime` span with token counts

Session cleanup: `cancel()` → `live.Close()` → `wg.Wait()` → send `{done:true}` → close `done` channel.

## Tool dispatch (`voice.go:619`, `voice_tools.go`)

When Gemini sends a `ToolCall` with one or more function calls:
1. All calls dispatched in parallel goroutines.
2. Each call: send `{toolCall: name}` to browser → run handler → send `{toolResult: json}` to browser.
3. Collect all `FunctionResponse`s → `live.SendToolResponse` back to Gemini.
4. Each dispatch is wrapped in an OTEL `tool.<name>` span.

**Available tools:**

| Tool | Handler | Notes |
|---|---|---|
| `get_memory` | `defGetMemory` | Item counts by kind + memory summary |
| `get_memory_items` | `defGetMemoryItems` | List items by kind; fires SSE `MemoryHitEvent` |
| `search_memory` | `defSearchMemory` | Keyword then HNSW semantic search; fires SSE `MemoryHitEvent` |
| `note_observation` | `defNoteObservation` | Writes `agent_note` to in-memory store (not persisted to CR) |
| `end_session` | `defEndSession` | Sets `pendingHangup`; session closes after next `TurnComplete` |

`end_session` is always included regardless of the tool allowlist in the config cell.

## Audio encoding

| Direction | Sample rate | Format | Encoding |
|---|---|---|---|
| Browser → Gemini | 16 kHz | PCM Int16 LE | base64 JSON over WS |
| Gemini → Browser | 24 kHz | PCM Int16 LE | base64 JSON over WS |

## OTEL tracing

See ADR-007 and ADR-008 for full tracing design. Per session:
- `voice.session` span wraps the entire session lifetime (kind: CHAIN)
- `llm.gemini_realtime` child span per `TurnComplete` with token usage
- `llm.session_summary` span at session end with cumulative totals
- `tool.<name>` child spans per tool call
