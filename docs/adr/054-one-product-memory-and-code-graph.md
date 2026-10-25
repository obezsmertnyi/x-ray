# ADR-054: One Product, Authored and Derived Memory

**Status:** Implemented
**Date:** 2026-10-24
**Builds on:** [ADR-043](043-code-graph.md) (code graph), [ADR-045](045-xray-code-snapshots-per-project-graphs.md) (maps as read tools), [ADR-053](053-graduate-to-v1-and-align-with-core.md) (memory store)

## Context

X-Ray carried an embedded copy of a code graph: `xray recall`, the same parser,
engine and index that later became the standalone `xray-memory` project. The
two copies drifted. The standalone one gained encrypted snapshots, loopback
access control, agent notes (`remember`, `recall`, `forget`), the `search`,
`impact`, `services` and `keygen` commands, and extractors for Compose files,
Dockerfiles, application config and SQL. It was about 1.6 times the size of the
embedded copy in the packages both had.

Two products with overlapping names, two release lines and one maintained copy
of the code is the wrong shape for a public release. The memory server already
mounts code-graph maps and gives them to voice and MCP, so the two halves are
used together.

## Decision

**X-Ray is one product: an agentic memory substrate for local and
Kubernetes-native use.** It holds two kinds of memory behind one MCP endpoint.

- **Authored memory:** typed items written by agents and people, stored as
  `MemoryCell` objects in a cluster or in process memory on a laptop.
- **Derived memory:** a graph of code, documentation and manifests, rebuilt
  from sources that already exist and served from snapshots.

The standalone code is the source of truth for the second kind.

- Its packages live under `internal/codegraph`, replacing `internal/recall`.
- Its commands are `xray graph <sub>`. Bare `xray` is still the memory server,
  so charts and scripts that run it keep working.
- Two builds from one source. The static build (`make build`, ko, no CGO) is the
  cluster image and carries every command that reads snapshots. The CGO build
  (`make graph-build`) adds `serve`, `snapshot` and `dump <dir>`, which parse.
  The parser image is a tag of the same repository, `:<version>-parser`.
- One chart per workload: `xray` for the memory server and `xray-graph` for a
  snapshot server on its own. The `xray-recall` chart is gone. (The `xray-graph`
  chart was later removed too: ADR-060.)
- The memory server opens encrypted snapshots with `--mental-maps-snapshot-key`.
- The MCP server key in the install snippet is `xray`.
- Public Go packages, so other projects use X-Ray without the server's
  internal packages. `pkg/memory` and `pkg/codegraph` run the memory store and
  the code-graph engine in the caller's process, with no server and no cluster.
  `pkg/xray` is a typed MCP client for a running X-Ray. The in-process packages
  and the client implement the same two interfaces, `xray.Memory` and
  `xray.CodeGraph`, so an agent moves between them by changing one line. The
  implementations stay in `internal`; the packages are thin facades over them.

## Not carried over

- The standalone retrieval evals. They are tied to that repository's own symbol
  names and build path and need rework before they run here.
- The `recall` and `maps` Make targets for per-repo source images. The maps image
  script (`hack/maps-oci.sh`) came across.

## Consequences

- One repository, one release line and one set of images and charts in GitHub
  Packages.
- The static image grows by the age library and the extractors. The parser sits
  behind the build tag, so the cluster image still carries no C code.
- An existing standalone user changes the command (`xray-memory serve` becomes
  `xray graph serve`) and, if they use it, the MCP server key. Snapshots are the
  same format and load unchanged.
