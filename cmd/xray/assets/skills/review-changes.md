---
name: Review Changes
description: Review a change with the X-Ray code graph — impact and call context
---

## Review Changes

Review a change using the **X-Ray** code graph (the `xray` MCP server) for blast-radius
and call context, instead of reading whole files.

### Steps

1. `get_graph_impact` (`seeds` = the changed symbols' qns) — what else the
   change touches, with distance.
2. `get_graph_risk` (`qn`) per changed symbol — LOW/MEDIUM/HIGH from direct
   caller count and how many modules the radius spans. Use it to rank what to
   read closely.
3. `get_graph_node <qn>` — the body plus `called_by` (who depends on it) and
   `calls` (what it relies on).
4. `search_graph` — related code and tests. `search_graph_docs` — whether an ADR
   already decided this.
5. For untested or high-blast-radius changes, name the specific tests to add.

### Output

Group findings by risk (high/medium/low): what changed and why it matters, who
is affected, test-coverage status, and a merge recommendation.

### Retrieval discipline

- Search to locate, then `get_graph_node` the one hit. Don't pull many full
  bodies just to find something.
- `get_graph_stats` is a diagnostic for counts, not a warm-up.
