---
name: Explore Codebase
description: Navigate and understand codebase structure using the X-Ray code graph
---

## Explore Codebase

Use the **X-Ray** code-graph MCP tools (the `xray` MCP server) to explore and understand the
codebase before falling back to Grep/Glob/Read.

### Steps

1. `search_graph` — find functions, classes, types, files or K8s manifests by
   meaning (the "how / what's deployed"). Returns ranked pointers: qn,
   file:line, short preview.
2. `search_graph_docs` — find the rationale behind the code: ADRs, todos,
   READMEs (the "why / what"). The preview alone answers most "why" questions.
3. `get_graph_node <qn>` — the full body of the one node a search returned,
   **plus its relations in both directions** (`calls`, `called_by`, `imports`).
   One call gives content and connections; there is no separate trace tool.
4. `get_graph_impact` (`seeds`) — blast radius of changing a symbol.
5. `get_graph_risk` (`qn`) — LOW/MEDIUM/HIGH triage from caller count and how
   many modules the radius spans. Coarse; use `get_graph_impact` for detail.
6. `get_graph_stats` — node/edge counts, languages, and the map's router
   summary. A diagnostic, not a warm-up step.

### Token efficiency

- Search to **locate**, then fetch **one** node. Replies are deliberately lean
  (short previews, 5 results by default); `limit` raises the count.
- `get_graph_node` on a `Doc` qn returns the whole document in one call — don't
  walk its sections one at a time.
- Qualified names are repo-namespaced: `repo//path::Symbol`. Pass `map` (aliases
  `repo`/`project`) to scope a search when several maps are served; omit it and
  the query is routed semantically to the best-matching map.
