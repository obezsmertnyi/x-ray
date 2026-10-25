---
name: Refactor Safely
description: Plan refactoring using X-Ray dependency and impact analysis
---

## Refactor Safely

Use the **X-Ray** code graph (the `xray` MCP server) to understand dependencies before
refactoring. (the graph is read-only analysis — it does not apply edits; make the
edits yourself, then refresh the graph with `update_code_graph`.)

### Steps

1. `search_graph` — locate the symbol(s) to change.
2. `get_graph_node <qn>` — the body plus `called_by` (every site a rename or
   signature change must update — that is your edit list) and `calls` (what it
   depends on).
3. `get_graph_risk` (`qn`) — a fast LOW/MEDIUM/HIGH read before you commit to
   the change.
4. `get_graph_impact` (`seeds`) — the full blast radius for a structural change.
5. Make the edits.
6. `update_code_graph` (`paths`) — re-index the changed files so search, node
   and impact reflect the new state. Each path is routed to its owning map.

### Safety

- Enumerate callers via `get_graph_node`'s `called_by` before a rename. The
  graph resolves cross-file calls, so this is more complete than a text search —
  but it only knows what it parsed: dynamic dispatch and reflection are invisible.
- `update_code_graph` needs the CGO build (it parses). Against a static build it
  refuses, and the graph silently goes stale relative to your edits.
