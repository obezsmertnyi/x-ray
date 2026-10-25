# ADR-060: One Chart

**Status:** Implemented

## Context

ADR-054 shipped two charts: `xray` for the memory server and `xray-graph` for a
snapshot server on its own, with maps mounted as image volumes. ADR-056 lets the
memory server pull a map from a registry itself, from one `mental_map` cell, so
the second deployment, its own embedder sidecar and the remote MCP wiring to reach
it are no longer needed to serve a map.

## Decision

The repository publishes one chart, `xray`. The `xray-graph` chart, its CI render
checks and its release steps are removed.

## Consequences

- Maps load into the memory server (`data.graph`, `data.graphRef`, or files in
  `--mental-maps-dir`) and are served on its MCP endpoint, by voice and over REST.
- A standalone snapshot server is still one command from the same image,
  `xray graph serve-snapshots`; it has no chart.
- Image volumes (Kubernetes 1.33) are no longer a requirement of any chart.
- ADR-054 is unchanged apart from a pointer.
