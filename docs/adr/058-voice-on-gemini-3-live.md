# ADR-058: Voice on Gemini 3 Live, and the Bounds on a Runaway Turn

**Status:** Implemented

## Context

The default Live model moves to `gemini-3.8-live`. On Vertex it is served only on
the multi-regions `eu` and `us`, not on a region such as `europe-west4`, and the
genai SDK needs a version that reaches the multi-region endpoints. It closes the
session with 1007 when affective dialog is set.

One session on it also cost $2.15: a model asking for a File node of the code
graph got an empty string, asked again about twenty times, and made 82 model
turns and 823k input tokens, because each tool round trip re-reads the whole
conversation. And after a goodbye the model read the end-of-session status aloud.

## Decision

- **Model and location.** `--voice-model` defaults to `gemini-3.8-live`. A new
  `--voice-location` (default: `--gcp-location`) sets where voice connects, so
  embeddings keep their own location. The genai SDK is v1.72.
- **Affective dialog** is never sent to a `gemini-3*` model.
- **File nodes say something.** `get_graph_node` on a File lists its symbols with
  the qn to fetch each by, instead of an empty body.
- **A repeated code-graph call is answered once.** The same tool with the same
  arguments, in one session, gets a pointer to the earlier result.
- **Tool-call caps.** 6 calls per answer and 40 per session, the Triage core's
  numbers; past them the model is told to answer from what it has. `save_memory`
  and `end_session` are never blocked.
- **`end_session` is answered by hanging up** (see ADR-015): no tool response, so
  the model gets no further turn to fill, and the session closes after the drain
  window. The tool description asks for one goodbye first and nothing after.
- **Measurement.** Latency histograms use second-sized buckets (the defaults put
  every voice latency in the first two), and a `voice first audio` log line gives
  each turn's time to first audio and the tool calls before it.

## Consequences

- A default install on Vertex needs `--voice-location=eu` (or `us`).
- A genuinely long investigation can hit the per-answer cap and gets a partial
  answer; the cap is a constant, easy to raise.

## Tests

`TestLiveSupportsAffectiveDialog`, `TestNodeJSON_AFileListsItsSymbols`,
`TestVoiceSession_IsRepeatedCodeCall`, `TestEndSession_SetsPendingHangup`.
