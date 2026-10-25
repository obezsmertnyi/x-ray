# ADR-055: The Web UI Is Part of the Binary

**Status:** Implemented
**Date:** 2026-10-25
**Builds on:** [ADR-054](054-one-product-memory-and-code-graph.md) (one product)

## Context

X-Ray's web UI lived in its own repository and its own Go binary, which served a
static Next.js export and reverse-proxied the API. Over time the UI was cut over
to the Triage controller's `/v2` surface and its support for X-Ray's own `/v1`
was removed, so the UI could no longer talk to the X-Ray server at all. A user of
X-Ray had a server with no UI, and a UI with no server.

The server still carries the pieces of an embedded UI: it serves `/manifest.json`
and `/sw.js`, and `NewServer` takes a filesystem to serve.

## Decision

**The web UI is part of the `xray` binary.**

- The sources are in `ui/`. `make ui` builds the static export and stages it in
  `internal/webui/web/`, and `//go:embed` compiles it in. `internal/webui` serves
  it at `/` and resolves export routes the way Next.js lays them out.
- The server sends `/v1/*`, `/healthz`, the PWA files, `/mcp` and the OAuth
  endpoints to the API and everything else to the UI, so the browser sees one
  origin. The voice WebSocket, the session cookie and the Origin check all need
  that, and embedding provides it without a proxy.
- Each HTML document is served with a script-src of the hashes of its own inline
  scripts. `'unsafe-inline'` remains for styles only.
- The UI speaks the API of the server it is built into, `/v1` by default. The
  `/v1` dialect (voice route, server-sent events, per-user notes, no tenants) was
  restored from the commit that removed it, and `NEXT_PUBLIC_API_PREFIX=/v2`
  builds the same sources for a Triage deployment.
- `make build` builds the UI first; `UI=0` skips it for an API-only binary with no
  Node requirement, and `--ui=false` turns it off at run time. A checkout with no
  built UI still compiles, because `web/.gitkeep` satisfies the embed directive.

## Consequences

- One binary, one image and one chart give the memory service and its UI, locally
  (`make run`) and in a cluster.
- The release workflow needs Node to build the export before ko compiles it in.
- The UI carries two API dialects. A change to a route has to be made in
  `lib/api.ts`, where `endpoints` is the one place they differ, and
  `ui/__tests__/dialect.test.ts` pins the `/v1` side.
- The separate `xray-ui` repository is superseded for X-Ray.
