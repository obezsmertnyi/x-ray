# Changelog

## [Unreleased]

## [2.2.0] - 2026-10-25

First public release.

### Added
- **Typed memory** on Kubernetes. Items are `MemoryCell` custom resources in the API group `xray.triageagent.dev/v1`, each with a kind, a scope (`public` or `private`), a subject, tags, a confidence, typed links and an optional expiry. etcd persists, replicates and audits them.
- **Recall.** Search fuses keyword and semantic ranking, collapses near-duplicate facts and weighs query words by how distinctive they are. With an embedding endpoint it builds an HNSW index per subject; without one it falls back to keyword search.
- **Provenance.** Every item records who wrote it. A write that names a person is `human`, anything else is `derived`, and a model's write never replaces a person's. A refuted item retires the items drawn from it.
- **Code graph** (`xray graph`). Go and ten other languages, Markdown and YAML become one graph per repository with resolved call and import edges, served as `search_graph`, `get_graph_node`, `get_graph_impact` and `get_graph_risk`. Snapshots can be age-encrypted. Tree-sitter grammars are C, so only the CGO build (`xray-parser`, the `-parser` image) parses.
- **Mental maps from a registry.** A `mental_map` cell with `spec.data.graphRef` (a digest-pinned OCI reference) is pulled and registered, so one deployment answers `search_memory` and the code-graph tools. `--registry-pull-secret` names a private registry's credentials, and `--mental-maps-snapshot-key` opens an encrypted snapshot.
- **Web UI**, embedded in the binary and served at `/`: the memory graph, search, per-kind lists, voice and a live view of recalls, lit the way optogenetics lights a neuron.
- **Voice** over a Gemini Live WebSocket, default model `gemini-3.8-live` (on Vertex, in the `eu` and `us` multi-regions; set `--voice-location`). A session opens with the user's newest notes and session memories. Each answer is limited to 6 tool calls and each session to 40, and `end_session` hangs up after the goodbye.
- **Sign-in off** (`--dev`) runs as a built-in user, so voice, personal notes and the live stream work locally.
- **Interfaces.** MCP over streamable HTTP with OAuth 2.1 or a shared secret, a REST API, a server-sent event stream, and Go packages to embed the memory store and the code graph in an agent, with an adapter for Google ADK.
- **Access control.** GitHub or Microsoft Entra sign-in, an optional GitHub org and team restriction, and read and write scopes for MCP clients.
- **Traces.** OpenTelemetry export with `--otel-endpoint`, `--otel-project` and `--otel-token-secret` (a bearer token read from a Secret), second-sized latency buckets, and a `voice first audio` log line per turn.
- **One Helm chart**, `xray`, and a static `xray` image plus a CGO `-parser` image, published to `ghcr.io/triageagent-dev` and built with ko.
