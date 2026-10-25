# ADR-059: The API Group Is `xray.triageagent.dev`

**Status:** Implemented

## Context

`MemoryCell` lived in the group `xray.ai` (ADR-014 moved it there from
`triageagent.dev`; ADR-053 graduated it to `v1`). A Kubernetes API group is a DNS
name, and the convention is a domain the project controls. `xray.ai` is not one
the project holds, and the project is published under `triageagent.dev`.

## Decision

The group is `xray.triageagent.dev`: `apiVersion: xray.triageagent.dev/v1`, CRD
`memorycells.xray.triageagent.dev`, short names `memcell` and `mcell`. The labels
the server puts on the cells it writes use the same prefix
(`xray.triageagent.dev/managed`, `xray.triageagent.dev/kind`), and the chart's RBAC
grants the new group.

## Consequences

- **Breaking, with no conversion.** A cell stored under `xray.ai` is not read.
  An existing install creates the new CRD (Helm applies `crds/` only at first
  install, so apply it by hand on upgrade, or from `config/crd/`) and copies its
  cells across, changing `apiVersion`. There is no dual-group mode.
- **The Triage core must move with it.** The core and X-Ray share the type and
  the CRD name; a deployment that runs both needs both on the same group.
- ADR-014 and ADR-053 keep the names they were written with.
