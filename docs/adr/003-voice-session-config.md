# ADR-003: Voice Session Configuration via system_configuration Cell

**Status:** Implemented  
> **Changes (2026-06-09):** The Gemini Live model named below for the Google AI
> path, `gemini-2.0-flash-live-001`, was retired by Google on 2025-12-09. Both
> backends now use `google/gemini-live-2.5-flash-native-audio`, and the model is
> no longer a compiled-in constant — it is resolved from config only
> (`--voice-model` flag default → `system_configuration.voiceModel` CR override).
> The decision below is unchanged; only the model identifier and where it comes
> from have moved.

## How it works

When a voice WebSocket session starts (`voice.go: ServeVoiceWS()`), `loadSessionConfig()` is called to determine:
- The system prompt for Gemini Live
- Which memory tools are enabled
- Which voice name to use
- Which Gemini Live model to use

`loadSessionConfig()` calls `memory.List("system_configuration", "", "xray", 1)` to find the config cell.

**Critical**: `Search()` must NOT be used here — `Search()` returns nil for empty queries. `List()` is used instead.

## The system_configuration cell

- Kind: `system_configuration` (reserved — excluded from all normal search/list results)
- Subject: `xray`
- Content: the system prompt verbatim (passed to Gemini Live as the agent instruction)
- Tags: control which tools are enabled and override voice/model

## Tag format

| Tag | Effect |
|---|---|
| `get_memory` | enables `get_memory` tool (orientation summary) |
| `get_memory_items` | enables `get_memory_items` tool (browse by kind) |
| `search_memory` | enables `search_memory` tool (keyword/semantic search) |
| `note_observation` | enables `note_observation` tool (write to memory — **in-memory only, not persisted**) |
| `voice:<name>` | overrides default voice (e.g. `voice:Kore`) |
| `model:<id>` | overrides default model (e.g. `model:google/gemini-live-2.5-flash-native-audio`) |

Any tag not matching `voice:` or `model:` prefix is treated as a tool name to enable.

## Fallback behaviour

If no `system_configuration` cell exists (list returns empty):
- `enabledTools` is empty → **all tools are enabled** (this is the unsafe default)
- `systemPrompt` falls back to `defaultVoiceSystemPrompt()`
- Voice and model use compiled-in defaults (`liveModelVertex` or `liveModelGoogleAI` depending on auth)

## Default models

- With GCP project (Vertex AI): `google/gemini-live-2.5-flash-native-audio`
- With Gemini API key: `gemini-2.0-flash-live-001`

## Where the cell lives

The cell is an `AwarenessMemoryCell` CR in the cluster, managed directly with `kubectl` — not via Helm. Name: `xray-system-configuration`, namespace: `xray-xray`.

To change config: edit the CR spec tags/content and the running pod picks it up on the next voice session start (no restart needed — `loadSessionConfig()` is called per-session).
