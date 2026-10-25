# ADR-007: OpenTelemetry Tracing for Voice Sessions

**Status:** Implemented  
## Context

X-Ray's voice sessions (Gemini Live via `internal/xray/voice.go`) and tool calls (`voice_tools.go`) have no distributed tracing. Debugging latency, tool call failures, or token usage requires grepping logs. The `go.opentelemetry.io/otel` package is already present as an indirect dependency.

The Triage core implements a full OTEL tracing stack for Gemini Live sessions (`internal/voice/session.go`) and tool spans (`internal/triageagent/agent.go`), using OpenInference span conventions and OTLP gRPC export. This is the reference implementation.

## Decision

Add OpenTelemetry tracing to X-Ray, modelled on the Triage core's implementation:

1. **`internal/observability/otel.go`** — `ConfigureOTel(endpoint, projectName string)` initialising a global `TracerProvider` with OTLP gRPC export, parent-based sampling, and OpenInference span kind normalisation. Returns early (no-op tracer) when endpoint is empty.
2. **`cmd/xray/main.go`** — add two flags to the existing `var ()` block and call `ConfigureOTel` before `NewServer`:
   - `--otel-endpoint` (`otelEndpoint string`) — OTLP gRPC endpoint; empty = disabled
   - `--otel-project` (`otelProject string`) — sets `openinference.project.name` resource attribute (e.g. `"xray"`)
3. **`charts/xray-xray/values.yaml`** — add `tracing:` block (same pattern as `embedding:`):
   ```yaml
   tracing:
     endpoint: ""
     projectName: "xray-xray"
   ```
4. **Spans in `internal/xray/voice.go`**:
   - `llm.gemini_realtime` — emitted per audio turn on each `UsageMetadata` event; dual attribute sets for Phoenix + standard OTel backends:
     - OpenTelemetry GenAI semconv: `gen_ai.system=vertex_ai`, `gen_ai.request.model`, `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`, `gen_ai.usage.total_tokens`
     - OpenInference: `llm.token_count.prompt`, `llm.token_count.completion`, `openinference.span.kind=LLM`
     - Session: `session.id`, `input.value` (transcript, clipped), `output.value` (transcript, clipped)
   - `llm.session_summary` — emitted once on session close with cumulative token totals; span timestamp = session start time.
5. **Spans in `internal/xray/voice_tools.go`** — `tool.<name>` per tool call; attributes: tool name, input/output (clipped), `openinference.span.kind=TOOL`.

## Consequences

- Traces are opt-in via `--otel-endpoint`; when unset the global tracer is a no-op and there is zero overhead.
- OTLP gRPC is the only supported export protocol. Phoenix listens on gRPC port 4318 (non-standard; default gRPC is 4317).
- Token usage per session becomes observable without log scraping.
- Any OpenInference-compatible backend (Phoenix, Arize, Grafana Tempo) can consume the traces.
- Adds `go.opentelemetry.io/otel/sdk` and `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc` as direct dependencies (already transitive).

## Addendum: GenAI client metrics (Prometheus, pull-based)

Spans give per-trace detail but are expensive to aggregate for dashboards/alerting, and the trace backend (Phoenix) does not store metrics. So metrics are kept **entirely separate from the trace export**: a Prometheus-backed `MeterProvider` (`observability.ConfigureMetrics`) is exposed at `GET /metrics` on a dedicated `--metrics-address` (default `:9090`), scraped in-cluster — nothing is pushed to the OTLP trace endpoint. The instruments (from the [GenAI metrics semantic conventions](https://opentelemetry.io/docs/specs/semconv/gen-ai/gen-ai-metrics/), plus two X-Ray-specific ones):

- `gen_ai.client.token.usage` — Int64 histogram, `{token}`, per turn split by `gen_ai.token.type` (`input`/`output`).
- `gen_ai.client.operation.time_to_first_token` — Float64 histogram, `s`. **TTFT**: from the user's `audioStreamEnd` to the first model audio chunk — the perceived responsiveness of the voice agent. This replaced an earlier `operation.duration` that mistakenly timed only the output-streaming window (and logged 0s for tool-only turns).
- `gen_ai.client.operation.time_per_output_token` — Float64 histogram, `s`. **TPOT**: per-turn mean = (turn end − first audio) ÷ output tokens. With TTFT it decomposes latency into "time to start" (TTFT) and "streaming speed" (TPOT). Recorded only for turns that produced audio output.
- `gen_ai.client.tool.duration` — Float64 histogram, `s`, labeled `gen_ai.tool.name`. Per-tool-call execution latency (recorded alongside the existing in-memory `TrackToolCall`).
- `gen_ai.client.session.duration` — Float64 histogram, `s`. Full voice session wall-time, recorded at session close.
- `xray.memory.search.duration` — Float64 histogram, `s`, labeled `search.mode` (`lexical` for the no-network fast path, `fused` when semantic/HNSW ran). Recorded in `MemoryStore.Search`.

The GenAI instruments carry `gen_ai.operation.name`, `gen_ai.provider.name=gcp.vertex_ai`, `gen_ai.request.model`. Record helpers (`observability.RecordGenAI*` / `RecordMemorySearchDuration`) no-op when no MeterProvider is installed.

These histograms drive latency SLOs via standard PromQL, e.g. `histogram_quantile(0.95, rate(gen_ai_client_operation_time_to_first_token_seconds_bucket[5m]))` for p95 TTFT, or per-tool/per-search-mode p95 by adding `by (gen_ai_tool_name)` / `by (search_mode)`.

The metrics port is **not** added to the Service or HTTPRoute, so `/metrics` is reachable only by in-cluster scrapers; the pod carries `prometheus.io/scrape` annotations. Helm: `metrics.enabled` / `metrics.port`.

Note: these metrics follow the **current** semconv (`gen_ai.provider.name`, no `total_tokens`), which differs from the span attributes above (`gen_ai.system`, `…total_tokens`) — the spans were intentionally left unchanged to avoid disturbing Phoenix/OpenInference rendering. All `go.opentelemetry.io/otel*` packages aligned to v1.44.0; adds the `go.opentelemetry.io/otel/exporters/prometheus` dependency.

### Metric reference

All are histograms exposed at `/metrics`. Prometheus name suffixes `_sum`/`_count`/`_bucket` apply; `.` in the instrument name becomes `_`.

| Instrument | Unit | Labels | What it measures |
|------------|------|--------|------------------|
| `gen_ai.client.token.usage` | `{token}` | `gen_ai.token.type` (input/output), operation, provider, model | Tokens per turn, split by type |
| `gen_ai.client.operation.time_to_first_token` | `s` | operation, provider, model | **TTFT** — last input transcription → first model audio (per user-initiated turn) |
| `gen_ai.client.operation.time_per_output_token` | `s` | operation, provider, model | **TPOT** — (turn end − first audio) ÷ output tokens |
| `gen_ai.client.tool.duration` | `s` | `gen_ai.tool.name` | Per-tool-call execution latency |
| `gen_ai.client.session.duration` | `s` | operation=`voice_session`, provider, model | Full voice session wall-time |
| `xray.memory.search.duration` | `s` | `search.mode` (lexical/fused) | Memory search latency |

Notes on cardinality / counting:
- Metrics are **cumulative since process start** and aggregate across all sessions — they are not per-session (per-session detail lives in traces, keyed by `session.id`). Derive averages with `_sum/_count`, percentiles with `histogram_quantile`.
- **TTFT/TPOT record once per user-initiated turn**, not per model turn. Tool-result continuations and multi-part model replies produce model audio without a preceding input transcription, so they do not re-arm the TTFT anchor (by design — there is no user-wait to measure). Expect `time_to_first_token_count` ≈ number of user utterances, which is typically lower than `token.usage_count` (number of metered model turns).

### Example PromQL

```promql
# p95 TTFT over 5m
histogram_quantile(0.95, rate(gen_ai_client_operation_time_to_first_token_seconds_bucket[5m]))

# p95 TPOT
histogram_quantile(0.95, rate(gen_ai_client_operation_time_per_output_token_seconds_bucket[5m]))

# avg input tokens per turn
rate(gen_ai_client_token_usage_sum{gen_ai_token_type="input"}[5m])
  / rate(gen_ai_client_token_usage_count{gen_ai_token_type="input"}[5m])

# avg session duration
rate(gen_ai_client_session_duration_seconds_sum[1h])
  / rate(gen_ai_client_session_duration_seconds_count[1h])

# p95 tool latency by tool
histogram_quantile(0.95, sum by (gen_ai_tool_name, le) (rate(gen_ai_client_tool_duration_seconds_bucket[5m])))

# p95 search latency by mode
histogram_quantile(0.95, sum by (search_mode, le) (rate(xray_memory_search_duration_seconds_bucket[5m])))
```

### Sample readout (one session, verified live)

| Metric | Total / obs | Average |
|--------|-------------|---------|
| TTFT | 2.18 s / 3 | 0.73 s per response |
| TPOT | 0.0223 s / 3 | 7.4 ms / token |
| Session duration | 35.8 s / 1 | 35.8 s |
| Token usage — input | 14,644 / 9 turns | 1,627 |
| Token usage — output | 902 / 8 turns | 113 |
| Search — `lexical` | 7.06 ms / 1 | — |
