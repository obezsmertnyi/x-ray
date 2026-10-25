# ADR-056: Mental Maps Pulled From a Registry (`graphRef`)

**Status:** Implemented

## Context

A code graph (or any other map) is a `.graph.gob.gz` snapshot. Until now the
memory server got one only as a file in `--mental-maps-dir` or inline in a
`mental_map` cell (`data.graph`, base64). An etcd object holds about 1.5 MiB, so
the inline form stops at roughly 480 nodes, and a file needs a volume, which the
memory chart does not mount. The usual answer was a second deployment, the
`xray-graph` chart, with the snapshots in an image volume, plus a remote MCP entry
so the voice model could reach it.

The Triage core already solves this with a cell that points at the map:
`spec.data.graphRef`, a digest-pinned OCI reference that the controller pulls.

## Decision

A `mental_map` cell may carry `data.graphRef` (plain text in `bytes`, mime
`text/plain`) instead of `data.graph`. The memory server pulls the artifact and
registers its graph as an in-memory map, the same registry the file snapshots and
inline cells use, so one deployment answers `search_memory` and the code-graph
tools over MCP and by voice.

- **Same shape as the core.** Key `graphRef`, value `name@sha256:...`. Inline
  `graph` wins when both are present.
- **Digest-pinned.** A tag is refused: it can move, and a map that changes under a
  running server cannot be reproduced.
- **The artifact** is an ordinary image whose layers hold exactly one regular file
  ending `.graph.gob.gz` (a `make maps-image` image qualifies). None, two, or one
  over 256 MiB is an error.
- **Pulled once per reference.** The reference's hash is the change check, so a
  replay of the cell does not pull again, and a new digest replaces the map. A
  failed pull is retried every 30 seconds and logged once per reference.
- **Credentials.** `--registry-pull-secret` names a `dockerconfigjson` Secret in
  the cell's namespace; the process's docker config is the fallback.
- **Encryption** is unchanged: an age-encrypted snapshot opens with
  `--mental-maps-snapshot-key`, and stays ciphertext in the registry.
- **Requirements.** `--mental-maps-dir` (it may be empty or absent: it turns the
  registry on) and the embedder the map was built with, since the snapshot carries
  vectors and the server embeds only queries.

## Consequences

- A map ships as an image and loads from one cell. No second deployment, no volume.
- The `xray-graph` chart is no longer needed and is removed (ADR-060).
- The digest to pin is the one the registry reports after the push, not the one
  `make maps-image` prints: a push tool may rewrite the manifest.
- Cost: the server holds the maps in memory (about 62 MiB for a 4,000-node map).

## Tests

`TestFetchGraphArtifact_*` (against an in-process registry), `TestParseGraphRef_RequiresADigest`,
`TestSyncMapCell_GraphRef`, `TestSyncMapCell_GraphRefMustBePinned`.
