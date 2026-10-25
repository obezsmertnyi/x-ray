package mcpserver

// Read-only (Mode B) tool registrations. Each tool resolves the right
// per-project graph+index per call through d.Resolver, instead of closing over a
// single fixed graph: search tools route by (project, query); node-keyed tools
// route by the project prefix of a qualified name. No update_code_graph here —
// snapshots are immutable. The reply shaping (lean pointers, capped previews,
// capped relationships) is identical to the live tools, reusing the same helpers.

import (
	"context"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/index"
)

// weighByRoute scales a map's hits by how well that map matched the route, so
// hits from different maps become comparable. A hit's Fused score is a rank
// score computed WITHIN its own map, which makes every map's rank-1 hit carry
// the same value: merging on it alone round-robins the maps, putting an
// unrelated map's best hit above a good map's second. Scaling is monotonic, so
// a map's own order is untouched.
func weighByRoute(hits []index.Hit, routeScore float64) {
	for i := range hits {
		hits[i].Fused *= routeScore
	}
}

// sortHitsByFused orders hits merged from several maps by the score each map's
// Search already ordered its own list by — weighByRoute having first made those
// scores comparable across maps. It must NOT fall back to Hit.Score: a fused
// list holds hits a cosine never would, and re-sorting on cosine buries exactly
// those. The sort is stable and carries no tiebreak for the same reason: equal
// scores keep the order the map delivered them in.
func sortHitsByFused(hits []index.Hit) {
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Fused > hits[j].Fused })
}

// labelLister is the optional Resolver capability of naming its registered maps.
// A Resolver that serves a single unnamed graph does not implement it.
type labelLister interface{ Labels() []string }

// mapArgDesc documents the explicit-routing argument shared by the read-only
// search tools, and NAMES the registered maps so a caller can bind a map the
// user mentioned ("project Team") to its label. Without that list a model has
// no way to know the argument has a value worth filling, and writes the map into
// the query text instead — where it routes far worse than it does here. "map" is
// the current noun; "project"/"repo" are kept as back-compat aliases (a loaded
// map originated as a code project/repo). The list is the set registered at
// startup; a map added later still routes when named, via Registry.Resolve.
func mapArgDesc(d Deps) string {
	desc := "The map to search. When the user names a map or project, pass its label HERE rather than writing it into query; matching is case-insensitive. Omit to route the query semantically to the best-matching map."
	if l, ok := d.Resolver.(labelLister); ok {
		if labels := l.Labels(); len(labels) > 0 {
			desc += " Registered maps: " + strings.Join(labels, ", ") + "."
		}
	}
	return desc + " (Aliases: project, repo.)"
}

// routeMap extracts the explicit map selector from a tool's args, accepting
// "map" (preferred) or the "project"/"repo" back-compat aliases.
func routeMap(args map[string]any) string {
	if m, _ := args["map"].(string); m != "" {
		return m
	}
	if p, _ := args["project"].(string); p != "" {
		return p
	}
	p, _ := args["repo"].(string)
	return p
}

// runSearchRO routes a search to the resolved project(s) and merges their hits.
// With fanout==1 there is a single resolved index; with fanout>1 the per-project
// hit lists are merged and re-ranked by score before truncation to limit.
func runSearchRO(ctx context.Context, d Deps, toolName string, args map[string]any, kinds map[string]bool) (*mcp.CallToolResult, error) {
	query, _ := args["query"].(string)
	mapName := routeMap(args)
	sel, aerr := parseSelect(args)
	if aerr != nil {
		return mcp.NewToolResultError(aerr.Error()), nil
	}
	requested := 0
	if l, ok := args["limit"].(float64); ok {
		requested = int(l)
	}
	limit := clampLimit(requested, !sel.Empty())

	resolved, err := d.Resolver.Resolve(ctx, mapName, query)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Gather hits across resolved projects, then re-rank by score and truncate.
	// A map that holds none of this tool's kinds is searched for what it does
	// hold (searchKinds), and the reply says so.
	var merged []index.Hit
	var notes []string
	selected := 0
	for _, r := range resolved {
		mapKinds, note := searchKinds(toolName, kinds, searchedMap{Label: r.Label, Graph: r.Graph})
		// The widening note describes the TOOL's scope. A caller that named its
		// own kinds has superseded that scope, and reporting it then tells them
		// every kind was searched when in fact only theirs was.
		if note != "" && len(sel.Kinds) == 0 {
			notes = append(notes, note)
		}
		if !sel.Empty() {
			selected += r.Index.CountMatching(mapKinds, "", sel)
			// An order or attr naming an attribute this map's set does not
			// carry is a vocabulary mismatch the ranking cannot show; the note
			// names it, per map, since each map has its own vocabulary.
			if n := attrVocabularyNote(r.Index, mapKinds, "", sel, r.Label); n != "" {
				notes = append(notes, n)
			}
		}
		hits, err := r.Index.Search(ctx, query, mapKinds, "", sel, limit)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if argBool(args, "cutoff") {
			hits = index.CutAtLargestGap(hits)
		}
		d.Resolver.RecordSearch(r.Label, len(hits))
		weighByRoute(hits, r.RouteScore)
		merged = append(merged, hits...)
	}
	sortHitsByFused(merged)
	if len(merged) > limit {
		merged = merged[:limit]
	}

	results := make([]map[string]any, len(merged))
	for i, h := range merged {
		results[i] = briefHit(h)
	}
	routed := make([]string, len(resolved))
	for i, r := range resolved {
		routed[i] = r.Label
	}
	out := map[string]any{"query": query, "routedTo": routed, "count": len(merged), "results": results}
	if !sel.Empty() {
		out["selected"] = selected
		// A selection that matched nothing because the map does not use the
		// name asked for is the one empty reply that is a vocabulary mismatch
		// rather than an absent answer; naming the kinds it DOES hold turns a
		// dead end into the next query.
		if selected == 0 && len(resolved) > 0 {
			if n := emptySelectionNote(sel, resolved[0].Graph); n != "" {
				notes = append(notes, n)
			}
		}
	}
	if n := selectionNote(sel, selected, len(merged)); n != "" {
		notes = append(notes, n)
	}
	if len(notes) > 0 {
		out["note"] = strings.Join(notes, " ")
	}
	return jsonResult(out)
}

func registerSearchCodeRO(s *server.MCPServer, d Deps) {
	s.AddTool(mcp.NewTool("search_graph",
		readOnlyTool(),
		mcp.WithDescription("Find nodes in a loaded mental map by meaning — a graph of code (functions, classes, files, K8s manifests), services (a service dependency map), or another domain. Returns ranked pointers (qn, file:line where applicable, short preview). Feed a qn to get_graph_node for full content + relations in both directions (what it depends on and what depends on it), or get_graph_impact for blast radius. For code repos, rationale/decisions live in search_graph_docs."),
		mcp.WithString("query", mcp.Description(searchQueryDesc), mcp.Required()),
		mcp.WithString("map", mcp.Description(mapArgDesc(d))),
		mcp.WithString("project", mcp.Description("Alias for map (back-compat)")),
		mcp.WithString("repo", mcp.Description("Alias for map (back-compat)")),
		mcp.WithNumber("limit", mcp.Description(searchLimitDesc)),
		mcp.WithString("kind", mcp.Description(searchKindDesc)),
		mcp.WithString("attr", mcp.Description(searchAttrDesc)),
		mcp.WithString("order", mcp.Description(searchOrderDesc)),
		mcp.WithBoolean("cutoff", mcp.Description("Drop the ranked tail at its steepest score drop, keeping only hits above the cliff. Adapts to how many answers the query has; trades recall for precision")),
	), logged(d, "search_graph", func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return runSearchRO(ctx, d, "search_graph", req.GetArguments(), codeKinds)
	}))
}

func registerSearchDocsRO(s *server.MCPServer, d Deps) {
	s.AddTool(mcp.NewTool("search_graph_docs",
		readOnlyTool(),
		mcp.WithDescription("Find DOCUMENTATION by meaning: ADRs, todos, READMEs and their sections — the rationale, decisions, design, and conventions (the why and what). Returns ranked pointers (qn, file:line, short preview). The preview answers most 'why' questions; read the file at file:line for the full text, or feed the qn to get_graph_node for its place in the graph. For implementation, use search_graph."),
		mcp.WithString("query", mcp.Description("Natural-language description of the topic, decision, or concept you are looking for")),
		mcp.WithString("map", mcp.Description(mapArgDesc(d))),
		mcp.WithString("project", mcp.Description("Alias for map (back-compat)")),
		mcp.WithString("repo", mcp.Description("Alias for map (back-compat)")),
		mcp.WithNumber("limit", mcp.Description(searchLimitDesc)),
		mcp.WithString("kind", mcp.Description(searchKindDesc)),
		mcp.WithString("attr", mcp.Description(searchAttrDesc)),
		mcp.WithString("order", mcp.Description(searchOrderDesc)),
		mcp.WithBoolean("cutoff", mcp.Description("Drop the ranked tail at its steepest score drop, keeping only hits above the cliff. Adapts to how many answers the query has; trades recall for precision")),
	), logged(d, "search_graph_docs", func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return runSearchRO(ctx, d, "search_graph_docs", req.GetArguments(), docKinds)
	}))
}

func registerGetNodeRO(s *server.MCPServer, d Deps) {
	s.AddTool(mcp.NewTool("get_graph_node",
		readOnlyTool(),
		mcp.WithDescription("Return the full content of one node by its qualified name (qn) from a search hit: a doc's complete text, a section's body, a manifest's YAML, or a code symbol's signature — plus its relationships (calls, called_by, imports). One call gives the node's content and how it connects."),
		mcp.WithString("qn", mcp.Description("Qualified name from a search hit (e.g. repo//docs/adr/043.md or repo//internal/foo.go::Bar.Baz)"), mcp.Required()),
	), logged(d, "get_graph_node", func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		qn, _ := req.GetArguments()["qn"].(string)
		if qn == "" {
			return mcp.NewToolResultError("qn is required"), nil
		}
		r, err := d.Resolver.ResolveByQN(ctx, qn)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		out, ok := NodeJSON(r.Graph, qn)
		if !ok {
			return mcp.NewToolResultError("node not found: " + qn), nil
		}
		return jsonResult(out)
	}))
}

func registerImpactRO(s *server.MCPServer, d Deps) {
	s.AddTool(mcp.NewTool("get_graph_impact",
		readOnlyTool(),
		mcp.WithDescription("Compute the blast radius of changing one or more symbols: a bidirectional traversal over calls/imports/etc. returning impacted symbols and their distance. Use before editing to understand what a change touches."),
		mcp.WithArray("seeds", mcp.Description("Qualified names of the symbols being changed"), mcp.Required()),
		mcp.WithNumber("depth", mcp.Description("Max traversal depth (default 2)")),
		mcp.WithNumber("max_nodes", mcp.Description("Cap on impacted nodes (default 200)")),
	), logged(d, "get_graph_impact", func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		seeds := toStrings(args["seeds"])
		if len(seeds) == 0 {
			return mcp.NewToolResultError("seeds is required"), nil
		}
		// All seeds must belong to one project's graph; route by the first.
		r, err := d.Resolver.ResolveByQN(ctx, seeds[0])
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		depth := 2
		if dpt, ok := args["depth"].(float64); ok && dpt > 0 {
			depth = int(dpt)
		}
		maxNodes := 200
		if mn, ok := args["max_nodes"].(float64); ok && mn > 0 {
			maxNodes = int(mn)
		}
		return jsonResult(impactResult(r.Graph, seeds, depth, maxNodes))
	}))
}

func registerRiskRO(s *server.MCPServer, d Deps) {
	s.AddTool(mcp.NewTool("get_graph_risk",
		readOnlyTool(),
		mcp.WithDescription("Score the change risk of one symbol as LOW/MEDIUM/HIGH, from its direct caller count and how many distinct modules (directories) its blast radius reaches. A coarse triage signal, not a precise cost model — use get_graph_impact for the full node-by-node blast radius."),
		mcp.WithString("qn", mcp.Description("Qualified name of the symbol being changed"), mcp.Required()),
		mcp.WithNumber("depth", mcp.Description("Max traversal depth for the underlying impact scan (default 2)")),
		mcp.WithNumber("max_nodes", mcp.Description("Cap on impacted nodes scanned (default 200)")),
	), logged(d, "get_graph_risk", func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		qn, _ := args["qn"].(string)
		if qn == "" {
			return mcp.NewToolResultError("qn is required"), nil
		}
		r, err := d.Resolver.ResolveByQN(ctx, qn)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		depth := 2
		if dpt, ok := args["depth"].(float64); ok && dpt > 0 {
			depth = int(dpt)
		}
		maxNodes := 200
		if mn, ok := args["max_nodes"].(float64); ok && mn > 0 {
			maxNodes = int(mn)
		}
		return jsonResult(r.Graph.Risk(qn, depth, maxNodes))
	}))
}

// registerUpdateRO exposes update_code_graph against the worked copy: a remote
// client re-indexes changed files; the registry routes each path to its project,
// re-parses + re-embeds only changed nodes, and marks the project dirty for
// flush-on-evict. Mutating (read-only:false), idempotent for unchanged files.
func registerUpdateRO(s *server.MCPServer, d Deps) {
	s.AddTool(mcp.NewTool("update_code_graph",
		mcp.WithToolAnnotation(mcp.ToolAnnotation{
			ReadOnlyHint:    boolPtr(false),
			DestructiveHint: boolPtr(false),
			IdempotentHint:  boolPtr(true),
			OpenWorldHint:   boolPtr(false),
		}),
		mcp.WithDescription("Re-index changed files into the worked-copy code graph (re-parse, re-resolve dependents, re-embed only changed nodes) so search/trace/impact reflect your latest edits. Each path is routed to its owning project; that project's snapshot is flushed back to disk before it is evicted. Returns how many files changed/were removed."),
		mcp.WithArray("paths", mcp.Description("File paths to re-index. Absolute, or relative to a snapshotted repo root — a relative path must name an existing file under exactly one served repo; to record a DELETION pass an absolute path. Paths that resolve to no served repo come back in \"unowned\"."), mcp.Required()),
	), logged(d, "update_code_graph", func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		paths := toStrings(req.GetArguments()["paths"])
		if len(paths) == 0 {
			return mcp.NewToolResultError("paths is required"), nil
		}
		changed, removed, skipped, unowned, err := d.Resolver.UpdateByPaths(ctx, paths)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return jsonResult(updateResult(changed, removed, skipped, unowned))
	}))
}

func registerStatsRO(s *server.MCPServer, d Deps) {
	s.AddTool(mcp.NewTool("get_graph_stats",
		readOnlyTool(),
		mcp.WithDescription("Return graph statistics for a loaded map: node and edge counts by kind, how many nodes are embedded, and the map's semantic-router summary (\"summary\") plus where it came from (\"summarySource\": explicit/readme/stats) — the text a query with no explicit map is actually routed against. Pass map to pick one by name; omit it to stat the map the query routes to. Also includes a \"_metrics\" block: process-lifetime search-quality metrics for the whole server (hybrid search counters, routing outcomes, per-map search/hit totals, map load/eviction counts) — see /metrics for the same numbers over plain HTTP."),
		mcp.WithString("map", mcp.Description(mapArgDesc(d))),
		mcp.WithString("project", mcp.Description("Alias for map (back-compat)")),
		mcp.WithString("repo", mcp.Description("Alias for map (back-compat)")),
		mcp.WithString("query", mcp.Description("If map is omitted, the map this query routes to is reported")),
	), logged(d, "get_graph_stats", func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		mapName := routeMap(args)
		query, _ := args["query"].(string)
		resolved, err := d.Resolver.Resolve(ctx, mapName, query)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		stats := make(map[string]any, len(resolved)+1)
		for _, r := range resolved {
			stats[r.Label] = mapStatsEntry(r)
		}
		// Reserved key, prefixed so it can never collide with a real map label:
		// process-lifetime search metrics for the whole server, not just the
		// map(s) resolved above. Additive — every existing key is untouched.
		stats["_metrics"] = d.Resolver.Metrics()
		return jsonResult(stats)
	}))
}
