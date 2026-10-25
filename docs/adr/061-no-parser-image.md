# ADR-061: No Parser Image

**Status:** Implemented

## Context

ADR-054 published a second image, `:<version>-parser`: the same `cmd/xray` built
with CGO so it can parse. ko does not cross-compile C, so its release job needed a
linux/amd64 host with a C compiler, and it compiled every Tree-sitter grammar on
each release. Nothing in the chart runs it. The server in a cluster is the static
build, which reads snapshots and parses nothing.

## Decision

The release publishes the static `xray` image and the `xray` chart. The parser
image, its release job, `deploy/parser` and `make graph-push` are removed.

## Consequences

- Parsing is a workstation or CI step: `make graph-build` produces `bin/xray-parser`,
  and `xray graph snapshot` writes the snapshot that a map image or a
  `mental_map` cell carries.
- The CGO build is still tested in CI, so the grammars stay covered.
- A slow or broken grammar build can no longer hold up a release.
