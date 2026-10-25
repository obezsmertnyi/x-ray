# ADR-008: Phoenix Session Tracing for Voice Conversations

**Status:** Implemented  
## Context

ADR-007 introduced OTEL tracing for voice sessions but spans were not appearing in Phoenix's **Sessions** tab. Investigation revealed several issues:

1. **Wrong attribute keys**: spans used `openinference.session.id` / `openinference.user.id` but Phoenix indexes sessions on `session.id` / `user.id`.
2. **Empty `user.id`**: the voice WebSocket is opened without a `?subject=` query param in the default UI flow, so `user.id` was set to `""` — Phoenix silently skips session indexing for empty user IDs.
3. **Colliding session IDs**: the counter reset to `vs-1` on every pod restart, collapsing all historical sessions into one entry in Phoenix.
4. **0ms turn latency**: turn spans used the same timestamp for start and end; audio-only turns (no transcript) never set `turnStartTime` at all.
5. **Missing conversation text**: Phoenix session detail view showed "undefined" for HUMAN/AI bubbles because `input.value` / `output.value` were only on child `llm.gemini_realtime` spans, not the root `voice.session` span.
6. **Disconnected span tree**: `s.traceCtx` was captured before the `voice.session` span started, so turn/tool spans were root spans rather than children.

## Decision

### Span structure

Wrap each voice session in a root `voice.session` CHAIN span. All turns and tool calls are children:

```
voice.session  [CHAIN, root]
├── llm.gemini_realtime  [LLM]
│   └── tool.search_memory  [TOOL]
├── llm.gemini_realtime  [LLM]
├── ...
├── llm.session_summary  [LLM]
└── tool.end_session  [TOOL]
```

### Session identity

- `session.id` — set to `vs-<counter>-<4-byte-random-hex>` (e.g. `vs-1-19573f05`); random suffix ensures uniqueness across pod restarts.
- `user.id` — set to `s.subject` (the `?subject=` query param) when provided, otherwise falls back to the session ID so Phoenix never receives an empty value.
- Both attributes are set explicitly on every span type (session, turn, tool, summary) and also propagated via OpenTelemetry baggage using `ContextWithSession()` + `sessionBaggageProcessor`.

### Baggage propagation (Go equivalent of Phoenix `setSession()`)

`observability.ContextWithSession(ctx, sessionID, userID)` stores `session.id` and `user.id` in OTel baggage. `sessionBaggageProcessor.OnStart` copies these onto every span automatically — equivalent to the TypeScript `setSession()` + `context.with()` pattern from the Phoenix docs.

### Turn latency

`turnStartTime` is recorded on whichever arrives first: audio chunk (`InlineData`), output transcription, or input transcription. This ensures non-zero latency even for greeting turns where the user has not spoken.

### Conversation preview

`sessionFirstInput` and `sessionLastOutput` are accumulated across turns. At session close they are stamped as `input.value` / `output.value` on the root `voice.session` span so Phoenix shows real conversation text in the session list and detail view.

### traceCtx assignment

`s.traceCtx` is assigned after `tracer.Start("voice.session")` so it carries the active session span. Turn and summary spans started from `s.traceCtx` are correctly nested as children.

## Consequences

- Phoenix Sessions tab now shows one entry per voice conversation with unique IDs, correct latency, token counts, and human/AI conversation text.
- `session.id` is always non-empty (random suffix) and `user.id` is always non-empty (falls back to session ID).
- Baggage propagation is belt-and-suspenders alongside direct attribute stamping — either mechanism alone would be sufficient, but both ensures compatibility with any OpenInference backend.
- Turn latency is accurate for all turn types including Gemini-initiated greetings.
- No changes to the public API or deployment flags.
