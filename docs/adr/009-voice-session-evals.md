# ADR-009: Evaluation Strategy for X-Ray Voice Sessions

**Status:** Implemented  
**Todo:** [voice-session-evals](../todo/voice-session-evals.md)  

> **Changes (2026-06-09):** The LLM-as-judge model named below,
> `gemini-2.0-flash-001`, is superseded by `gemini-2.5-flash` (the 2.0 family was
> retired by Google on 2025-12-09). The evaluation strategy below is unchanged;
> only the judge model identifier moves to `gemini-2.5-flash`.

## Context

X-Ray is a voice AI interface backed by a knowledge graph (memory store). Each voice session consists of:
- A user conversing with Gemini Live via WebSocket
- The agent calling memory tools (`search_memory`, `get_memory_items`, `get_memory`, `note_observation`, `end_session`) to answer questions
- Responses grounded in memory cells retrieved from the HNSW vector index

With Phoenix tracing now operational (ADR-008), every session produces structured spans with `input.value`, `output.value`, tool names, and tool outputs. This data is sufficient to run automated LLM-as-judge evaluations without any additional instrumentation.

The question is: **which failure modes matter most for this system, and which evals catch them?**

## Failure Modes

| Failure | Symptom | Detectable by |
|---------|---------|---------------|
| Agent answers from training data instead of calling a tool | Plausible-sounding response with no tool span child | Tool call rate check |
| Agent calls the wrong tool for the query | `get_memory` called for a specific search query | ToolSelectionEvaluator |
| Agent passes wrong arguments to a tool (e.g. wrong subject) | Tool returns empty results despite items existing | ToolInvocationEvaluator |
| Agent response contradicts or ignores tool output | "I don't know" when tool returned relevant data | FaithfulnessEvaluator |
| Agent response invents facts not present in tool output | Response cites details not in any retrieved memory cell | FaithfulnessEvaluator |
| Session ends prematurely | `end_session` fires mid-conversation | Session length + end_session timing check |

## Decision

Implement two automated evals, both LLM-as-judge using `gemini-2.0-flash-001` via Vertex AI, run as a batch script against Phoenix span data.

### Eval 1 — Tool Relevance (`ToolSelectionEvaluator`)

**What it checks**: given the user's utterance and the list of available tools, did the agent select the right tool?

**Why this matters**: Gemini Live sometimes defaults to `get_memory` (broad overview) when `search_memory` (targeted lookup) would be more precise, or vice versa. This eval catches systematic tool confusion.

**Data source**: `tool.*` spans — `input` from parent `llm.gemini_realtime` span, `available_tools` from static tool list, `tool_selection` from span name.

**Signal**: score 1.0 = correct tool for the context, 0.0 = wrong tool. Pass rate per session.

### Eval 2 — Groundedness (`FaithfulnessEvaluator`)

**What it checks**: is the agent's response faithful to what the memory tools actually returned?

**Why this matters**: the agent's value proposition is surfacing knowledge graph content, not generating from training data. Any response that invents or contradicts retrieved memory cells is a trust failure.

**Data source**: `llm.gemini_realtime` spans with at least one child tool span — `input` (user query), `output` (AI response), `context` (concatenated `output.value` from all child tool spans in the same turn).

**Signal**: score 1.0 = response is grounded in retrieved context, 0.0 = hallucination or context ignored. Pass rate per session.

## Why Not Other Evals

| Eval | Reason skipped |
|------|---------------|
| **Correctness** | No ground-truth Q&A dataset exists yet; requires human labeling |
| **Tool invocation** (argument quality) | Tool argument structure is simple (subject + query string); ToolSelectionEvaluator covers the higher-value case |
| **Latency SLO** | Observable directly from span timestamps; no LLM judge needed |
| **Session completion** | Boolean check on presence of `tool.end_session`; no LLM judge needed |
| **Toxicity / safety** | Out of scope for an internal enterprise tool |

## Eval Infrastructure

- **Judge model**: `gemini-2.5-flash` via Vertex AI global endpoint (`YOUR_GCP_PROJECT`, `location=global`). `gemini-2.5-flash` is not available in `europe-west4` for this project but responds via `global`. Uses Application Default Credentials — no extra secrets needed.
- **Span source**: Phoenix HTTP API (`phoenix-svc.monitoring.svc.cluster.local:6006`), project `xray`.
- **Result storage**: scores logged back as Phoenix span annotations via `log_span_annotations_dataframe` — visible in the Annotations column per span and aggregated in the Sessions tab Feedback column.
- **Trigger**: manual (`python scripts/eval_voice_sessions.py`); can be scheduled once the script is stable.

## Consequences

- Both evals are fully automated — no human labels required.
- Results are visible in Phoenix alongside the traces that produced them.
- `gemini-2.0-flash-001` as judge introduces model-in-the-loop cost per eval run (~2 LLM calls per tool span for tool relevance, ~1 per grounded turn for faithfulness).
- Evals are retrospective (run against existing traces) not inline — they do not block sessions.
- Future: add a golden evalset for correctness once enough sessions accumulate with user feedback annotations.
