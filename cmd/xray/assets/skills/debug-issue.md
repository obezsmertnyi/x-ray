---
name: Debug Issue
description: Systematically debug issues using the X-Ray code graph
---

## Debug Issue

Use the **X-Ray** code graph (the `xray` MCP server) to trace and debug issues.

### Steps

1. `search_graph` — find code related to the issue; take the best-scoring qn.
2. `get_graph_node <qn>` — its full body **and** its relations both ways.
   `called_by` is the path in (who could trigger this); `calls` is the path out
   (what it could be failing through). Walk the chain by feeding a related qn
   back into `get_graph_node`.
3. `get_graph_impact` (`seeds` = the suspect symbols) — what else the fault
   could reach, with distance per node.
4. `search_graph_docs` — known behaviour and rationale for the area (ADRs,
   todos). A bug is often a documented decision you haven't read yet.

### Tips

- Read both directions around a symbol before concluding: a wrong caller and a
  wrong callee look identical from the symbol itself.
- Any qn from a search hit feeds straight into `get_graph_node`,
  `get_graph_impact` and `get_graph_risk`.
- `get_graph_stats` is for counts when you need them, not a required first step.
