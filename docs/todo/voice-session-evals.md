# TODO: Voice Session Evals (Phoenix + Vertex AI)

Script: `scripts/eval_voice_sessions.py`

## Steps

- [ ] **1. Pull tool spans from Phoenix**
  Fetch spans from `GET /v1/spans` filtered to `project=xray`, names matching `tool.*`.
  Columns needed: `span_id`, `name`, `attributes.tool.name`, `input.value`, `output.value`, `parent_id`.
  Join parent `llm.gemini_realtime` span to get the user query that triggered the tool call.

- [ ] **2. Build `tool_relevance_df`**
  One row per tool call.
  Columns: `input` (user utterance from parent LLM span), `available_tools` (static list: `search_memory`, `get_memory`, `get_memory_items`, `note_observation`, `end_session`), `tool_selection` (tool name from span).

- [ ] **3. Build `groundedness_df`**
  One row per LLM turn that called at least one tool.
  Columns: `input` (user utterance), `output` (AI response), `context` (concatenated tool outputs from child tool spans).

- [ ] **4. Init Vertex AI LLM judge**
  ```python
  from phoenix.evals import LLM
  llm = LLM(provider="google", model="gemini-2.5-flash",
            project="YOUR_GCP_PROJECT", location="global")
  ```
  Uses Application Default Credentials.

- [ ] **5. Run `ToolSelectionEvaluator`**
  ```python
  from phoenix.evals.metrics import ToolSelectionEvaluator
  tool_scores = ToolSelectionEvaluator(llm=llm).evaluate_dataframe(tool_relevance_df)
  ```
  Returns `score` (1.0/0.0), `label` (correct/incorrect), `explanation` per row.

- [ ] **6. Run `FaithfulnessEvaluator`**
  ```python
  from phoenix.evals.metrics import FaithfulnessEvaluator
  faith_scores = FaithfulnessEvaluator(llm=llm).evaluate_dataframe(groundedness_df)
  ```
  Returns `score` (1.0/0.0), `label` (faithful/unfaithful), `explanation` per row.

- [ ] **7. Log results back to Phoenix**
  ```python
  client.spans.log_span_annotations_dataframe(dataframe=annotated_df)
  ```
  Scores appear in the Annotations column next to each span in Phoenix UI.
  Enables the Feedback column in the Sessions tab.

- [ ] **8. Print per-session summary**
  Table: session_id | tool_relevance_pass_rate | groundedness_pass_rate | total_tool_calls | total_turns

## Config

| Variable | Value |
|---|---|
| Phoenix HTTP | `http://phoenix-svc.monitoring.svc.cluster.local:6006` |
| Phoenix project | `xray` |
| GCP project | `YOUR_GCP_PROJECT` |
| GCP location | `global` (judge only; embeddings stay on `europe-west4`) |
| LLM judge | `gemini-2.5-flash` |

## Dependencies

```
arize-phoenix-evals>=3
arize-phoenix-client
google-auth
pandas
```
