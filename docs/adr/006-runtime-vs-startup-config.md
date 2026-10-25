# ADR-006: Runtime vs Startup Configuration

**Status:** Implemented  
## Context

Two categories of configuration exist in X-Ray: values that can change between sessions without a restart, and values that are baked into the process at startup and cannot change.

## Decision

### Runtime config (per-session, via `system_configuration` cell)

Loaded fresh on every voice session start by `loadSessionConfig()`. Can be updated in the CR and takes effect on the next session — no pod restart needed.

| Setting | How |
|---|---|
| System prompt | cell `content` field |
| Enabled tools | tags matching tool names |
| Voice name | `voice:<name>` tag |
| Voice model | `model:<id>` tag |

See ADR-003 for full details.

### Startup config (flags only, requires redeploy)

These are wired into the process at init and cannot be overridden at runtime from a cell:

| Setting | Flag | Why not runtime-configurable |
|---|---|---|
| Embedding model | `--embedding-model` | The HNSW vector index is built against a specific model's output dimensionality at startup. Changing the model mid-flight would corrupt the index — all vectors would need to be re-embedded. |
| Embedding endpoint | `--embedding-endpoint` | Same reason — tied to the indexer lifecycle. |
| GCP project / location | `--gcp-project`, `--gcp-location` | Auth and API client initialised once at startup. |
| HTTP / MCP listen address | `--http-address`, `--mcp-address` | Bound at process start. |

## Consequence

To change the embedding model: update `values.yaml`, `make push`, `helm upgrade`. All existing vectors are invalid after the change — the HNSW index must be rebuilt (restart causes CellSyncer to re-embed all cells).

To change voice behaviour: edit the `xray-system-configuration` AwarenessMemoryCell CR. No restart needed.
