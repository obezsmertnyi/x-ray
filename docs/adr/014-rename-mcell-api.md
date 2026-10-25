# ADR-014: Rename AwarenessMemoryCell → MemoryCell, triageagent.dev → xray.ai

**Status:** Implemented  
## Context

The `AwarenessMemoryCell` type and `triageagent.dev` API group were inherited from the
Triage project. X-Ray is a standalone product; the names are misleading and
unnecessarily verbose. The shortName `mcell` is already in the kubebuilder marker.

## Decisions

### 1. Type renames (Go)

| Old | New |
|-----|-----|
| `AwarenessMemoryCell` | `MemoryCell` |
| `AwarenessMemoryCellSpec` | `MemoryCellSpec` |
| `AwarenessMemoryCellStatus` | `MemoryCellStatus` |
| `AwarenessMemoryCellList` | `MemoryCellList` |
| `AwarenessMemoryCellDocument` | `MemoryCellDocument` |

### 2. API group

`triageagent.dev` → `xray.ai` in `groupversion_info.go` and all RBAC manifests.

### 3. Kubernetes resource name

`awarenessmemorycells` → `memorycells` in RBAC rules.

### 4. Import alias

All consumers of `api/v1alpha1` use import alias `mcell` instead of `triagev1alpha1`.

### 5. File rename

`api/v1alpha1/awarenessmemorycell_types.go` → `api/v1alpha1/memorycell_types.go`

## Consequences

**Positive:**
- Names reflect the xray domain, not the Triage origin.
- `mcell` alias is short and matches the existing kubectl shortName.
- No API surface change — JSON field names, spec shape, and CRD version (`v1alpha1`) are unchanged.

**Migration required:**
- Existing CRs in cluster are stored as `kind: AwarenessMemoryCell` under group
  `triageagent.dev`. After this change the CRD must be re-applied with the new kind/group.
  Existing CRs must be recreated or migrated (export → delete → re-apply with new apiVersion).
- The `zz_generated.deepcopy.go` file is hand-updated here (controller-gen not available in
  this repo's toolchain); re-run `controller-gen object:headerFile="hack/boilerplate.go.txt" paths="./..."` if toolchain is available.
