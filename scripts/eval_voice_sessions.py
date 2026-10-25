"""
Voice session evals for X-Ray using Phoenix SDK + Vertex AI.

Runs nine evals against Phoenix traces:

  Tool evals (require tool.* spans):
  1. tool_relevance   — did the agent pick the right tool for the user's query?
  2. tool_invocation  — were the tool arguments correct and well-formed?
  3. tool_response    — did the agent properly handle what the tool returned?
  4. groundedness     — is the AI response faithful to what the tools returned?
  5. refusal          — did the agent refuse/deflect despite having tool results?
                        Fast MatchesRegex pre-filter + LLM judge only on hits.

  Conversation evals (work on every voice turn, no tools required):
  6. response_quality — is the reply relevant and helpful given the user's utterance?
  7. conv_faithfulness— does the response stay grounded / avoid hallucination?
  8. tone_persona     — does it sound like the X-Ray persona (not a generic chatbot)?
  9. coherence        — does each turn follow naturally from the prior context?

Usage:
    python scripts/eval_voice_sessions.py [--phoenix-url URL] [--project PROJECT] \
        [--location LOCATION] [--session SESSION_ID] [--days N]

Requirements:
    pip install "arize-phoenix-evals>=2" arize-phoenix-client pandas

Auth:
    Application Default Credentials (gcloud auth application-default login)
"""
from __future__ import annotations

import argparse
import json
import logging
import os
from datetime import datetime, timedelta, timezone
from typing import Any

import pandas as pd
from phoenix.client import Client
import re

from phoenix.evals import LLM, ClassificationEvaluator, evaluate_dataframe
from phoenix.evals.metrics import (
    FaithfulnessEvaluator,
    MatchesRegex,
    RefusalEvaluator,
    ToolInvocationEvaluator,
    ToolResponseHandlingEvaluator,
    ToolSelectionEvaluator,
)
from phoenix.evals.metrics.faithfulness import FAITHFULNESS_CLASSIFICATION_EVALUATOR_CONFIG

logging.basicConfig(level=logging.WARNING)

PHOENIX_PROJECT = "xray"
GCP_PROJECT = "YOUR_GCP_PROJECT"
GCP_LOCATION = "global"
JUDGE_MODEL = "gemini-2.5-flash"

TOOL_DEFINITIONS = (
    "search_memory: Search the knowledge graph for items matching a query. "
    "Use for specific questions about projects, people, events, or news.\n"
    "get_memory: Get a broad overview of all memory items. "
    "Use for general 'what do you know' questions.\n"
    "get_memory_items: Browse memory items by category. "
    "Use to list all items of a specific type (e.g. all projects).\n"
    "note_observation: Record an ephemeral note for this session. "
    "Use when the user asks to remember something during the conversation.\n"
    "end_session: Disconnect the voice session. "
    "Use ONLY when the user explicitly says goodbye or asks to stop."
)

TOOL_SCHEMAS = """
search_memory:
  description: Search the knowledge graph for items matching a query.
  parameters:
    - query (required, string): the search query
    - subject (optional, string): scope results to a subject/owner; omit for global search

get_memory:
  description: Get a broad overview of all memory items.
  parameters: none

get_memory_items:
  description: Browse memory items by category/kind.
  parameters:
    - kind (optional, string): filter by kind (e.g. stable_fact, recent_change, event, system_identity)

note_observation:
  description: Record an ephemeral note for this session.
  parameters:
    - note (required, string): the text to record

end_session:
  description: Disconnect the voice session.
  parameters: none
"""


# ── 1. Fetch spans ────────────────────────────────────────────────────────────

def _parse_since(value: str) -> timedelta:
    """Parse a duration string into a timedelta.

    Accepts: 30m, 1h, 2h30m, 1d, 7 (bare int treated as days).
    """
    value = value.strip()
    if re.fullmatch(r"\d+", value):
        return timedelta(days=int(value))
    pattern = re.compile(r"(?:(\d+)d)?(?:(\d+)h)?(?:(\d+)m)?$")
    m = pattern.fullmatch(value)
    if not m or not any(m.groups()):
        raise argparse.ArgumentTypeError(
            f"Invalid duration '{value}'. Use formats like 30m, 1h, 2h30m, 1d, or a plain integer (days)."
        )
    days_v   = int(m.group(1) or 0)
    hours_v  = int(m.group(2) or 0)
    minutes_v = int(m.group(3) or 0)
    return timedelta(days=days_v, hours=hours_v, minutes=minutes_v)


def fetch_spans(client: Client, since: timedelta, session_id: str | None) -> pd.DataFrame:
    start = datetime.now(timezone.utc) - since
    df = client.spans.get_spans_dataframe(
        project_identifier=PHOENIX_PROJECT,
        start_time=start,
    )
    if session_id:
        df = df[df["attributes.session.id"] == session_id]
    return df


# ── 2. Build tool relevance dataframe ────────────────────────────────────────

def build_tool_relevance_df(df: pd.DataFrame) -> pd.DataFrame:
    """One row per non-trivial tool call.

    ToolSelectionEvaluator expects: input, available_tools, agent_tool_selection
    - input               : user utterance or AI context from nearest ancestor span
    - available_tools     : static tool catalogue
    - agent_tool_selection: tool name that was actually called
    """
    tool_spans = df[
        df["name"].str.startswith("tool.")
        & (df["name"] != "tool.end_session")
    ].copy()
    if tool_spans.empty:
        return pd.DataFrame()

    span_lookup = df.set_index("context.span_id")

    rows = []
    for span_id, tool in tool_spans.iterrows():
        question = _find_context(tool, span_lookup)
        if not question:
            continue
        rows.append({
            "span_id": span_id,
            "session_id": tool.get("attributes.session.id", ""),
            "input": question,
            "available_tools": TOOL_DEFINITIONS,
            "tool_selection": tool["attributes.tool.name"],
        })

    if not rows:
        return pd.DataFrame()
    return pd.DataFrame(rows).set_index("span_id")


# ── 3a. Build tool invocation dataframe ──────────────────────────────────────

def build_tool_invocation_df(df: pd.DataFrame) -> pd.DataFrame:
    """One row per non-trivial tool call that has recorded input arguments.

    ToolInvocationEvaluator expects: input, available_tools, tool_selection
    - input          : user utterance from nearest ancestor span
    - available_tools: tool schemas (with parameter definitions)
    - tool_selection : tool call as string, e.g. search_memory(query="blog posts")
    """
    if "attributes.input.value" not in df.columns:
        return pd.DataFrame()
    tool_spans = df[
        df["name"].str.startswith("tool.")
        & (df["name"] != "tool.end_session")
        & df["attributes.input.value"].notna()
    ].copy()
    if tool_spans.empty:
        return pd.DataFrame()

    span_lookup = df.set_index("context.span_id")

    rows = []
    for span_id, tool in tool_spans.iterrows():
        question = _find_context(tool, span_lookup)
        if not question:
            continue
        raw_args = tool.get("attributes.input.value", "{}")
        try:
            args = json.loads(raw_args)
            args_str = ", ".join(f'{k}="{v}"' for k, v in args.items()) if args else ""
        except (json.JSONDecodeError, TypeError):
            args_str = raw_args
        tool_name = tool.get("attributes.tool.name", tool["name"].replace("tool.", ""))
        tool_call = f"{tool_name}({args_str})"

        rows.append({
            "span_id": span_id,
            "session_id": tool.get("attributes.session.id", ""),
            "input": question,
            "available_tools": TOOL_SCHEMAS,
            "tool_selection": tool_call,
        })

    if not rows:
        return pd.DataFrame()
    return pd.DataFrame(rows).set_index("span_id")


# ── 3b. Build tool response handling dataframe ────────────────────────────────

def build_tool_response_df(df: pd.DataFrame, ground_df: pd.DataFrame) -> pd.DataFrame:
    """One row per tool call that has input args recorded and a matching agent response.

    ToolResponseHandlingEvaluator expects: input, tool_call, tool_result, output
    - input      : user utterance
    - tool_call  : tool name + args string, e.g. search_memory(query="blog posts")
    - tool_result: summarised tool output (not raw JSON — can be huge)
    - output     : agent response after the tool call (from ground_df burst grouping)

    ground_df already has the correct input/output pairs per burst — reuse it
    to avoid re-implementing the burst-window logic here.
    """
    if "attributes.input.value" not in df.columns or "attributes.output.value" not in df.columns:
        return pd.DataFrame()
    tool_spans = df[
        df["name"].str.startswith("tool.")
        & (df["name"] != "tool.end_session")
        & df["attributes.input.value"].notna()
        & df["attributes.output.value"].notna()
    ].copy()
    if tool_spans.empty or ground_df.empty:
        return pd.DataFrame()

    # Build a lookup: output span_id → (input, output) from ground_df
    # ground_df.index is the output span_id; we need to map tool → output span
    # Strategy: for each tool span, find the ground_df row whose input matches
    # the same session and is the closest turn (ground_df has one row per burst)
    span_lookup = df.set_index("context.span_id")

    rows = []
    for span_id, tool in tool_spans.iterrows():
        question = _find_context(tool, span_lookup)
        if not question:
            continue

        raw_args = tool.get("attributes.input.value", "{}")
        try:
            args = json.loads(raw_args)
            args_str = ", ".join(f'{k}="{v}"' for k, v in args.items()) if args else ""
        except (json.JSONDecodeError, TypeError):
            args_str = str(raw_args)[:200]
        tool_name = tool.get("attributes.tool.name", tool["name"].replace("tool.", ""))
        tool_call = f"{tool_name}({args_str})"
        tool_result = _summarise_tool_result(str(tool["attributes.output.value"]))

        # Find matching agent response from ground_df (same session, input matches question)
        session_id = tool.get("attributes.session.id", "")
        candidates = ground_df[ground_df["session_id"] == session_id]
        match = candidates[candidates["input"] == question]
        if match.empty:
            # Fallback: any candidate in session with non-generic input
            match = candidates[candidates["input"] != "(voice query)"]
        if match.empty:
            continue
        output_val = match.iloc[0]["output"]

        rows.append({
            "span_id": span_id,
            "session_id": session_id,
            "input": question,
            "tool_call": tool_call,
            "tool_result": tool_result,
            "output": output_val,
        })

    if not rows:
        return pd.DataFrame()
    return pd.DataFrame(rows).drop_duplicates(subset="span_id").set_index("span_id")


# ── 3. Build groundedness dataframe ──────────────────────────────────────────

def build_groundedness_df(df: pd.DataFrame) -> pd.DataFrame:
    """One row per conversational turn where a tool was called and the agent responded.

    FaithfulnessEvaluator expects: input, output, context

    Gemini Live emits multiple llm.gemini_realtime spans per turn:
      - one with input.value set (user utterance, no output)
      - one with output.value set (AI response, no input)
      - tool spans interleaved between them (siblings under voice.session)

    We group by trace + 5-second burst windows. A burst is: one or more tool
    spans plus the output span that immediately follows them. The input span
    that immediately precedes the tool(s) provides the user utterance.
    """
    if "context.trace_id" not in df.columns or "start_time" not in df.columns:
        return pd.DataFrame()

    tool_spans = df[
        df["name"].str.startswith("tool.")
        & (df["name"] != "tool.end_session")
    ].copy()
    if tool_spans.empty:
        return pd.DataFrame()

    llm_spans = df[df["name"] == "llm.gemini_realtime"].copy()
    llm_with_output = llm_spans[
        llm_spans["attributes.output.value"].notna()
        & (llm_spans["attributes.output.value"].str.strip() != "")
    ]
    llm_with_input = llm_spans[
        llm_spans["attributes.input.value"].notna()
        & (llm_spans["attributes.input.value"].str.strip() != "")
    ]

    rows = []
    for trace_id, trace_tools in tool_spans.groupby("context.trace_id"):
        trace_output = llm_with_output[llm_with_output["context.trace_id"] == trace_id]
        trace_input = llm_with_input[llm_with_input["context.trace_id"] == trace_id]

        # Group tools into bursts: tools within 5s of each other form one turn
        tool_times = trace_tools["start_time"].sort_values()
        bursts: list[pd.DataFrame] = []
        current: list = []
        for tid, t in tool_times.items():
            if not current or (t - trace_tools.loc[current[-1], "start_time"]) < pd.Timedelta(seconds=5):
                current.append(tid)
            else:
                bursts.append(trace_tools.loc[current])
                current = [tid]
        if current:
            bursts.append(trace_tools.loc[current])

        for burst in bursts:
            burst_start = burst["start_time"].min()
            burst_end = burst["start_time"].max()

            # Output span: first llm span that starts after the burst ends (within 10s)
            after = trace_output[
                (trace_output["start_time"] >= burst_start)
                & (trace_output["start_time"] <= burst_end + pd.Timedelta(seconds=10))
            ]
            if after.empty:
                continue
            output_span = after.sort_values("start_time").iloc[0]
            output_val = str(output_span["attributes.output.value"]).strip()

            # Input span: last llm span with input that started before or at burst_start (within 10s)
            before = trace_input[
                trace_input["start_time"] <= burst_start + pd.Timedelta(seconds=2)
            ]
            if not before.empty:
                input_val = str(before.sort_values("start_time").iloc[-1]["attributes.input.value"]).strip()
            else:
                input_val = "(voice query)"

            context_parts = _collect_tool_outputs(burst)
            if not context_parts:
                continue

            rows.append({
                "span_id": output_span.name,
                "session_id": output_span.get("attributes.session.id", ""),
                "input": input_val,
                "output": output_val,
                "context": "\n".join(context_parts),
            })

    if not rows:
        return pd.DataFrame()
    # Deduplicate: one output span can only belong to one turn
    return pd.DataFrame(rows).drop_duplicates(subset="span_id").set_index("span_id")


# ── 3c. Build refusal dataframe ───────────────────────────────────────────────

# Patterns that suggest the agent declined to answer despite having tool results.
# MatchesRegex pre-filter: cheap, no LLM. Only suspicious rows go to RefusalEvaluator.
_REFUSAL_PATTERN = re.compile(
    r"(?i)"
    r"i (don'?t|do not) (have|know|find|see|have access to)"
    r"|i('m| am) (not sure|unable|sorry)"
    r"|i cannot (find|provide|access|give)"
    r"|no (information|data|results|items?) (found|available|returned)"
    r"|i (couldn'?t|could not) find"
    r"|that('s| is) (not|outside) (something i|my)"
    r"|i don'?t have (that|any) information"
)

_REFUSAL_SCREENER = MatchesRegex(pattern=_REFUSAL_PATTERN, name="refusal_screen")


def build_refusal_df(ground_df: pd.DataFrame) -> pd.DataFrame:
    """Rows from ground_df where the agent output looks like a refusal/deflection.

    Two-stage approach:
      Stage 1 (free): MatchesRegex screens for refusal-like phrases in output.
      Stage 2 (LLM):  RefusalEvaluator confirms on the suspicious subset only.

    RefusalEvaluator expects: input, output
    - input  : user utterance
    - output : agent response

    We only evaluate turns where the tool actually returned results (non-empty
    context) — a refusal on an empty tool result is expected behaviour, not a bug.
    """
    if ground_df.empty:
        return pd.DataFrame()

    # Only turns where tool returned something meaningful
    has_context = ground_df["context"].str.strip() != ""
    candidates = ground_df[has_context][["session_id", "input", "output"]].copy()
    if candidates.empty:
        return pd.DataFrame()

    # Stage 1: fast regex screen — keep only rows that look like refusals
    screen_results = evaluate_dataframe(
        dataframe=candidates,
        evaluators=[_REFUSAL_SCREENER],
    )
    screen_results.index = candidates.index

    # MatchesRegex returns packed dict in score column (same v2 format as LLM evals)
    score_col = next((c for c in screen_results.columns if "score" in c), None)
    if score_col:
        def _extract_score(v: Any) -> float:
            if isinstance(v, dict):
                return float(v.get("score") or 0)
            try:
                return float(v) if v is not None else 0.0
            except (TypeError, ValueError):
                return 0.0
        scores = screen_results[score_col].apply(_extract_score)
        suspicious = candidates[scores > 0]
    else:
        suspicious = candidates  # can't screen, pass all through

    return suspicious


def _find_context(span: pd.Series, span_lookup: pd.DataFrame) -> str:
    """Walk up parent chain, return first non-empty input or output text."""
    parent_id = span.get("parent_id")
    for _ in range(4):
        if not parent_id or pd.isna(parent_id) or parent_id not in span_lookup.index:
            break
        parent = span_lookup.loc[parent_id]
        for col in ("attributes.input.value", "attributes.output.value"):
            val = parent.get(col)
            if pd.notna(val) and str(val).strip():
                return str(val).strip()
        parent_id = parent.get("parent_id")
    return ""


def _summarise_tool_result(raw: str) -> str:
    """Compact tool output to a judge-readable summary (no huge JSON blobs)."""
    try:
        parsed = json.loads(raw)
        if isinstance(parsed, dict) and "items" in parsed:
            item_texts = []
            for item in parsed["items"][:5]:
                title = item.get("title", "")
                content = item.get("content", "")
                item_texts.append(f"{title}: {content}" if content else title)
            return "\n---\n".join(item_texts)
        # Other dict shapes: just key=value pairs, values truncated
        if isinstance(parsed, dict):
            return "; ".join(f"{k}={str(v)[:80]}" for k, v in list(parsed.items())[:8])
    except (json.JSONDecodeError, TypeError):
        pass
    return str(raw)[:500]


def _collect_tool_outputs(tool_spans: pd.DataFrame) -> list[str]:
    parts = []
    for _, child in tool_spans.iterrows():
        raw = child.get("attributes.output.value")
        if pd.isna(raw) or not raw:
            continue
        try:
            parsed = json.loads(raw)
            if isinstance(parsed, dict) and "items" in parsed:
                # Pass full content for each item so the judge sees what the agent saw
                item_texts = []
                for item in parsed["items"][:5]:
                    title = item.get("title", "")
                    content = item.get("content", "")
                    item_texts.append(f"{title}: {content}" if content else title)
                parts.append(f"[{child['name']}]\n" + "\n---\n".join(item_texts))
            else:
                parts.append(f"[{child['name']}] {str(raw)}")
        except (json.JSONDecodeError, TypeError):
            parts.append(f"[{child['name']}] {str(raw)}")
    return parts


# ── 3d. Conversation-turn evals (no tool dependency) ─────────────────────────

XRAY_PERSONA = (
    "X-Ray is a warm, knowledgeable AI persona for a technology organisation. "
    "X-Ray speaks naturally and conversationally, like a well-informed colleague — "
    "never robotic, never sycophantic. X-Ray refers to itself in first person. "
    "X-Ray is concise but substantive; it never pads responses with filler phrases."
)


def _build_conv_turns(df: pd.DataFrame) -> pd.DataFrame:
    """Pair consecutive llm.gemini_realtime input/output spans into conversation turns.

    Returns a DataFrame with columns: session_id, input, output, prior_context
    where prior_context is the previous turn's output (or empty for turn 1).
    Index is the output span_id.
    """
    if "context.trace_id" not in df.columns or "start_time" not in df.columns:
        return pd.DataFrame()

    llm_spans = df[df["name"] == "llm.gemini_realtime"].copy()
    llm_with_output = llm_spans[
        llm_spans["attributes.output.value"].notna()
        & (llm_spans["attributes.output.value"].str.strip() != "")
    ].copy()
    llm_with_input = llm_spans[
        llm_spans["attributes.input.value"].notna()
        & (llm_spans["attributes.input.value"].str.strip() != "")
    ].copy()

    if llm_with_output.empty:
        return pd.DataFrame()

    rows = []
    for trace_id in llm_with_output["context.trace_id"].unique():
        outputs = llm_with_output[llm_with_output["context.trace_id"] == trace_id].sort_values("start_time")
        inputs = llm_with_input[llm_with_input["context.trace_id"] == trace_id].sort_values("start_time")

        prev_output = ""
        for _, out_span in outputs.iterrows():
            # Nearest input span that started before this output
            before = inputs[inputs["start_time"] <= out_span["start_time"]]
            if before.empty:
                input_val = "(voice query)"
            else:
                input_val = str(before.iloc[-1]["attributes.input.value"]).strip()

            output_val = str(out_span["attributes.output.value"]).strip()
            rows.append({
                "span_id": out_span.name,
                "session_id": out_span.get("attributes.session.id", ""),
                "input": input_val,
                "output": output_val,
                "prior_context": prev_output,
            })
            prev_output = output_val

    if not rows:
        return pd.DataFrame()
    return pd.DataFrame(rows).drop_duplicates(subset="span_id").set_index("span_id")


_RESPONSE_QUALITY_TEMPLATE = [
    {
        "role": "system",
        "content": (
            "You are evaluating a voice AI assistant called X-Ray. "
            "The user's message was transcribed by STT and may contain minor errors. "
            "Judge whether the assistant's response is relevant and helpful given the user's query."
        ),
    },
    {
        "role": "user",
        "content": (
            "User query: {input}\n\n"
            "Assistant response: {output}\n\n"
            "Is this response relevant and genuinely helpful? "
            "Answer 'helpful' if yes, 'unhelpful' if not. Then briefly explain why."
        ),
    },
]

_CONV_FAITHFULNESS_TEMPLATE = [
    {
        "role": "system",
        "content": (
            "You are evaluating a voice AI assistant called X-Ray for factual faithfulness. "
            "STT transcription errors are expected — treat phonetically plausible near-matches as equivalent. "
            "Judge whether the response makes claims that go beyond what the user asked or introduces unsupported facts."
        ),
    },
    {
        "role": "user",
        "content": (
            "User query: {input}\n\n"
            "Assistant response: {output}\n\n"
            "Does the response stay faithful (no unsupported claims, no hallucinated facts)? "
            "Answer 'faithful' if yes, 'unfaithful' if not. Then briefly explain."
        ),
    },
]

_TONE_PERSONA_TEMPLATE = [
    {
        "role": "system",
        "content": (
            f"You are evaluating whether an AI assistant matches its defined persona.\n\n"
            f"Persona definition:\n{XRAY_PERSONA}"
        ),
    },
    {
        "role": "user",
        "content": (
            "Assistant response: {output}\n\n"
            "Does this response match the X-Ray persona — warm, natural, knowledgeable, concise, not generic? "
            "Answer 'on_persona' if yes, 'off_persona' if not. Then briefly explain."
        ),
    },
]

_COHERENCE_TEMPLATE = [
    {
        "role": "system",
        "content": (
            "You are evaluating conversational coherence for a voice AI assistant called X-Ray. "
            "Check whether the latest response flows naturally from the prior turn and the user's current query."
        ),
    },
    {
        "role": "user",
        "content": (
            "Prior assistant turn: {prior_context}\n\n"
            "User query: {input}\n\n"
            "Current assistant response: {output}\n\n"
            "Does this response follow naturally and coherently? "
            "Answer 'coherent' if yes, 'incoherent' if not. Then briefly explain."
        ),
    },
]


def build_conv_eval_dfs(df: pd.DataFrame) -> tuple[pd.DataFrame, pd.DataFrame, pd.DataFrame, pd.DataFrame]:
    """Build the four conversation-turn eval dataframes from llm.gemini_realtime spans."""
    turns = _build_conv_turns(df)
    if turns.empty:
        empty = pd.DataFrame()
        return empty, empty, empty, empty

    quality_df  = turns[["session_id", "input", "output"]].copy()
    faithful_df = turns[["session_id", "input", "output"]].copy()
    tone_df     = turns[["session_id", "output"]].copy()
    coherence_df = turns[["session_id", "input", "output", "prior_context"]].copy()
    return quality_df, faithful_df, tone_df, coherence_df


# ── 4. Init LLM judge ────────────────────────────────────────────────────────

# Prepended to the faithfulness prompt to handle STT transcription errors.
# Voice input is transcribed by STT and may contain phonetic mishearings
# (e.g. "REC" instead of "RAG", "wrecks" instead of "Rex"). The judge must
# treat phonetically plausible near-matches as equivalent when assessing faithfulness.
_FAITHFULNESS_SYSTEM_PROMPT = (
    "You are evaluating AI voice assistant responses. "
    "The user's query was transcribed from speech by a speech-to-text (STT) engine "
    "and may contain transcription errors — words that sound similar to the intended word "
    "but are spelled differently (e.g. 'REC' instead of 'RAG', 'wrecks' instead of 'Rex', "
    "'sea' instead of 'see'). "
    "When judging faithfulness, treat phonetically plausible near-matches as equivalent. "
    "Do NOT mark a response as unfaithful solely because a proper noun or technical term "
    "in the response differs from the transcribed query by a plausible STT mishearing."
)


def _build_faithfulness_evaluator(llm: LLM) -> ClassificationEvaluator:
    """FaithfulnessEvaluator with a system prompt that accounts for STT transcription errors."""
    base_messages = [
        msg.model_dump() for msg in FAITHFULNESS_CLASSIFICATION_EVALUATOR_CONFIG.messages
    ]
    system_message = {"role": "system", "content": _FAITHFULNESS_SYSTEM_PROMPT}
    prompt_with_system = [system_message] + base_messages

    return ClassificationEvaluator(
        name=FAITHFULNESS_CLASSIFICATION_EVALUATOR_CONFIG.name,
        llm=llm,
        prompt_template=prompt_with_system,
        choices=FAITHFULNESS_CLASSIFICATION_EVALUATOR_CONFIG.choices,
        direction=FAITHFULNESS_CLASSIFICATION_EVALUATOR_CONFIG.optimization_direction,
        input_schema=FaithfulnessEvaluator.FaithfulnessInputSchema,
    )


def _build_conv_evaluator(name: str, template: list[dict], choices: list[str], llm: LLM) -> ClassificationEvaluator:
    return ClassificationEvaluator(
        name=name,
        llm=llm,
        prompt_template=template,
        choices=choices,
        direction="maximize",
    )


def build_model(project: str, location: str) -> LLM:
    """Phoenix LLM wrapper using Vertex AI + Application Default Credentials."""
    vtx = {"vertexai": True, "project": project, "location": location}
    return LLM(
        provider="google",
        model=JUDGE_MODEL,
        sync_client_kwargs=vtx,
        async_client_kwargs=vtx,
    )


# ── 7. Log results back to Phoenix ────────────────────────────────────────────

def _unpack_eval_result(row: pd.Series, prefix: str) -> dict:
    """Handle both flat columns (label/score) and the v2 dict-in-column format.

    evaluate_dataframe v2 stores the entire EvaluationResult as a dict inside
    a single column named '<prefix>_score'. Extract label, score, explanation
    from that dict, falling back to top-level columns for older API shapes.
    """
    packed = row.get(f"{prefix}_score")
    if isinstance(packed, dict):
        label = str(packed.get("label", ""))
        score_val = packed.get("score")
        expl = str(packed.get("explanation", ""))
    else:
        label = str(row.get(f"{prefix}_label") or row.get("label") or "")
        score_val = packed if packed is not None else row.get("score")
        expl = str(row.get(f"{prefix}_explanation") or row.get("explanation") or "")

    try:
        score = float(score_val) if score_val is not None else None
    except (TypeError, ValueError):
        score = None

    return {"label": label, "score": score, "explanation": expl[:1000]}

# ── 5 + 6. Run evals ─────────────────────────────────────────────────────────

EvalSpec = tuple[str, pd.DataFrame, str, list[str]]  # (eval_name, df, prefix, valid_labels)


def run_evals(
    model: LLM,
    tool_df: pd.DataFrame,
    invoc_df: pd.DataFrame,
    resp_df: pd.DataFrame,
    ground_df: pd.DataFrame,
    refusal_df: pd.DataFrame,
    quality_df: pd.DataFrame,
    faithful_df: pd.DataFrame,
    tone_df: pd.DataFrame,
    coherence_df: pd.DataFrame,
) -> tuple[pd.DataFrame, ...]:
    specs: list[tuple[str, pd.DataFrame, object]] = [
        ("tool_relevance",    tool_df,     ToolSelectionEvaluator(llm=model)),
        ("tool_invocation",   invoc_df,    ToolInvocationEvaluator(llm=model)),
        ("tool_response",     resp_df,     ToolResponseHandlingEvaluator(llm=model)),
        ("groundedness",      ground_df,   _build_faithfulness_evaluator(model)),
        ("refusal",           refusal_df,  RefusalEvaluator(llm=model)),
        ("response_quality",  quality_df,  _build_conv_evaluator(
            "response_quality", _RESPONSE_QUALITY_TEMPLATE, ["helpful", "unhelpful"], model)),
        ("conv_faithfulness", faithful_df, _build_conv_evaluator(
            "conv_faithfulness", _CONV_FAITHFULNESS_TEMPLATE, ["faithful", "unfaithful"], model)),
        ("tone_persona",      tone_df,     _build_conv_evaluator(
            "tone_persona", _TONE_PERSONA_TEMPLATE, ["on_persona", "off_persona"], model)),
        ("coherence",         coherence_df, _build_conv_evaluator(
            "coherence", _COHERENCE_TEMPLATE, ["coherent", "incoherent"], model)),
    ]
    results = []
    for name, df, evaluator in specs:
        if df.empty:
            results.append(pd.DataFrame())
            continue
        print(f"Running {name} eval on {len(df)} rows...")
        res = evaluate_dataframe(dataframe=df, evaluators=[evaluator])
        res.index = df.index
        results.append(res)
    return tuple(results)


# ── 7. Log results back to Phoenix ────────────────────────────────────────────

def log_results(
    client: Client,
    tool_df: pd.DataFrame,      tool_results: pd.DataFrame,
    invoc_df: pd.DataFrame,     invoc_results: pd.DataFrame,
    resp_df: pd.DataFrame,      resp_results: pd.DataFrame,
    ground_df: pd.DataFrame,    ground_results: pd.DataFrame,
    refusal_df: pd.DataFrame,   refusal_results: pd.DataFrame,
    quality_df: pd.DataFrame,   quality_results: pd.DataFrame,
    faithful_df: pd.DataFrame,  faithful_results: pd.DataFrame,
    tone_df: pd.DataFrame,      tone_results: pd.DataFrame,
    coherence_df: pd.DataFrame, coherence_results: pd.DataFrame,
) -> None:
    annotation_specs = [
        ("tool_relevance",    tool_results,     "tool_selection",        ("correct",    "incorrect")),
        ("tool_invocation",   invoc_results,    "tool_invocation",       ("correct",    "incorrect")),
        ("tool_response",     resp_results,     "tool_response_handling",("correct",    "incorrect")),
        ("groundedness",      ground_results,   "faithfulness",          ("faithful",   "unfaithful")),
        ("refusal",           refusal_results,  "refusal",               ("refused",    "answered")),
        ("response_quality",  quality_results,  "response_quality",      ("helpful",    "unhelpful")),
        ("conv_faithfulness", faithful_results, "conv_faithfulness",     ("faithful",   "unfaithful")),
        ("tone_persona",      tone_results,     "tone_persona",          ("on_persona", "off_persona")),
        ("coherence",         coherence_results,"coherence",             ("coherent",   "incoherent")),
    ]

    rows: list[dict[str, Any]] = []
    for ann_name, results, prefix, valid_labels in annotation_specs:
        if results.empty:
            continue
        for span_id, row in results.iterrows():
            result = _unpack_eval_result(row, prefix)
            if result["label"] not in valid_labels:
                continue
            rows.append({
                "span_id": span_id,
                "name": ann_name,
                "annotator_kind": "LLM",
                **result,
            })

    if not rows:
        print("No annotations to log.")
        return

    ann_df = pd.DataFrame(rows).set_index("span_id")
    # Phoenix client converts score via float() — pd.NA (from StringDtype) becomes nan,
    # which httpx cannot JSON-serialise. Drop score column when all values are None/NA.
    if "score" in ann_df.columns and ann_df["score"].isna().all():
        ann_df = ann_df.drop(columns=["score"])
    elif "score" in ann_df.columns:
        ann_df["score"] = pd.to_numeric(ann_df["score"], errors="coerce")
    client.spans.log_span_annotations_dataframe(dataframe=ann_df, sync=True)
    print(f"Logged {len(rows)} annotations to Phoenix.")


# ── 8. Summary ────────────────────────────────────────────────────────────────

def print_summary(
    tool_df: pd.DataFrame,      tool_results: pd.DataFrame,
    invoc_df: pd.DataFrame,     invoc_results: pd.DataFrame,
    resp_df: pd.DataFrame,      resp_results: pd.DataFrame,
    ground_df: pd.DataFrame,    ground_results: pd.DataFrame,
    refusal_df: pd.DataFrame,   refusal_results: pd.DataFrame,
    quality_df: pd.DataFrame,   quality_results: pd.DataFrame,
    faithful_df: pd.DataFrame,  faithful_results: pd.DataFrame,
    tone_df: pd.DataFrame,      tone_results: pd.DataFrame,
    coherence_df: pd.DataFrame, coherence_results: pd.DataFrame,
) -> None:
    def labels_for(results: pd.DataFrame, prefix: str) -> pd.Series:
        if results.empty:
            return pd.Series(dtype=str)
        return results.apply(lambda row: _unpack_eval_result(row, prefix)["label"], axis=1)

    tool_labels     = labels_for(tool_results,     "tool_selection")
    invoc_labels    = labels_for(invoc_results,    "tool_invocation")
    resp_labels     = labels_for(resp_results,     "tool_response_handling")
    ground_labels   = labels_for(ground_results,   "faithfulness")
    refusal_labels  = labels_for(refusal_results,  "refusal")
    quality_labels  = labels_for(quality_results,  "response_quality")
    faithful_labels = labels_for(faithful_results, "conv_faithfulness")
    tone_labels     = labels_for(tone_results,     "tone_persona")
    coherence_labels= labels_for(coherence_results,"coherence")

    sessions: set[str] = set()
    for df in (tool_df, invoc_df, resp_df, ground_df, refusal_df,
               quality_df, faithful_df, tone_df, coherence_df):
        if not df.empty and "session_id" in df.columns:
            sessions |= set(df["session_id"].unique())

    def score_col(df: pd.DataFrame, labels: pd.Series, pass_label: str, sid: str) -> str:
        if df.empty or labels.empty:
            return "—"
        mask = df["session_id"] == sid
        sel = labels.loc[df.index[mask]]
        total = len(sel)
        passed = int((sel == pass_label).sum())
        return f"{passed}/{total}" if total else "—"

    rows = []
    for sid in sorted(sessions):
        rows.append({
            "session_id":        sid,
            # conversation evals
            "response_quality":  score_col(quality_df,   quality_labels,  "helpful",    sid),
            "conv_faithfulness": score_col(faithful_df,  faithful_labels, "faithful",   sid),
            "tone_persona":      score_col(tone_df,      tone_labels,     "on_persona", sid),
            "coherence":         score_col(coherence_df, coherence_labels,"coherent",   sid),
            # tool evals
            "tool_relevance":    score_col(tool_df,    tool_labels,    "correct",  sid),
            "tool_invocation":   score_col(invoc_df,   invoc_labels,   "correct",  sid),
            "tool_response":     score_col(resp_df,    resp_labels,    "correct",  sid),
            "groundedness":      score_col(ground_df,  ground_labels,  "faithful", sid),
            "refusal":           score_col(refusal_df, refusal_labels, "answered", sid),
        })

    print("\n=== Eval Summary ===")
    if rows:
        print(pd.DataFrame(rows).set_index("session_id").to_string())
    else:
        print("No results.")
    print()


# ── main ──────────────────────────────────────────────────────────────────────

def main() -> None:
    parser = argparse.ArgumentParser(description="X-Ray voice session evals")
    parser.add_argument("--phoenix-url", default=os.getenv("PHOENIX_BASE_URL", "http://localhost:6006"))
    parser.add_argument("--project", default=GCP_PROJECT)
    parser.add_argument("--location", default=GCP_LOCATION)
    parser.add_argument("--session", default=None, help="Restrict to a specific session ID")
    parser.add_argument(
        "--since", default="1h", metavar="DURATION",
        type=_parse_since,
        help="Look-back window: 30m, 1h, 2h30m, 1d, 7d, or bare int (days). Default: 7d",
    )
    args = parser.parse_args()

    print(f"Phoenix : {args.phoenix_url}  project={PHOENIX_PROJECT}")
    print(f"Judge   : {JUDGE_MODEL} @ {args.location}  gcp={args.project}")
    print(f"Since   : {args.since}")
    print()

    client = Client(base_url=args.phoenix_url)

    # 1. fetch spans
    df = fetch_spans(client, args.since, args.session)
    print(f"Spans   : {len(df)} total  {df['name'].value_counts().to_dict()}")

    # 2–3. build eval frames
    tool_df    = build_tool_relevance_df(df)
    ground_df  = build_groundedness_df(df)
    invoc_df   = build_tool_invocation_df(df)
    resp_df    = build_tool_response_df(df, ground_df)
    refusal_df = build_refusal_df(ground_df)
    quality_df, faithful_df, tone_df, coherence_df = build_conv_eval_dfs(df)
    print(
        f"Rows    : tool_relevance={len(tool_df)}  tool_invocation={len(invoc_df)}"
        f"  tool_response={len(resp_df)}  groundedness={len(ground_df)}"
        f"  refusal_candidates={len(refusal_df)}\n"
        f"          response_quality={len(quality_df)}  conv_faithfulness={len(faithful_df)}"
        f"  tone_persona={len(tone_df)}  coherence={len(coherence_df)}"
    )

    if all(d.empty for d in (tool_df, invoc_df, resp_df, ground_df,
                              quality_df, faithful_df, tone_df, coherence_df)):
        print("No eval data — run some voice sessions first.")
        return

    # 4. init judge
    model = build_model(args.project, args.location)

    # 5–6. run evals
    (tool_results, invoc_results, resp_results, ground_results, refusal_results,
     quality_results, faithful_results, tone_results, coherence_results) = run_evals(
        model, tool_df, invoc_df, resp_df, ground_df, refusal_df,
        quality_df, faithful_df, tone_df, coherence_df,
    )

    # 7. log back to Phoenix
    log_results(
        client,
        tool_df,     tool_results,
        invoc_df,    invoc_results,
        resp_df,     resp_results,
        ground_df,   ground_results,
        refusal_df,  refusal_results,
        quality_df,  quality_results,
        faithful_df, faithful_results,
        tone_df,     tone_results,
        coherence_df,coherence_results,
    )

    # 8. summary
    print_summary(
        tool_df,     tool_results,
        invoc_df,    invoc_results,
        resp_df,     resp_results,
        ground_df,   ground_results,
        refusal_df,  refusal_results,
        quality_df,  quality_results,
        faithful_df, faithful_results,
        tone_df,     tone_results,
        coherence_df,coherence_results,
    )


if __name__ == "__main__":
    main()
