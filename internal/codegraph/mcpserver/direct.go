package mcpserver

// Direct, MCP-free entry points into the code-graph read tools. These call the
// resolver/index/graph functions straight (no in-process MCP server+client hop)
// so an in-binary caller — e.g. the voice layer — can offer search_graph et al.
// as NATIVE tools, exactly like search_memory wraps memory.Search directly. The
// reply shapes are identical to the MCP tools (they share briefHit/NodeJSON and
// the same kind groups), so a result is the same whether reached over /mcp or
// in-process.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/graph"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/index"
)

// jsonString marshals v to an indented JSON string (the same shape jsonResult
// returns over MCP, minus the MCP envelope).
func jsonString(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// SearchCodeDirect runs a code search and returns the brief-hit JSON. project
// may be empty (semantic routing). It is the direct-call twin of the search_graph
// MCP tool.
func SearchCodeDirect(ctx context.Context, d Deps, mapName, query string, limit int) (string, error) {
	return SearchDirect(ctx, d, mapName, query, limit, codeKinds, index.Select{}, false)
}

// SearchDocsDirect is the direct-call twin of the search_graph_docs MCP tool.
func SearchDocsDirect(ctx context.Context, d Deps, mapName, query string, limit int) (string, error) {
	return SearchDirect(ctx, d, mapName, query, limit, docKinds, index.Select{}, false)
}

// SearchDirect runs a search restricted to an explicit kind set (nil/empty
// means every kind — KindsForFilter("") returns nil for exactly this reason)
// against Deps.Resolver (Mode B / snapshot registry) and returns the brief-hit
// JSON. It is the generic direct-call entry point SearchCodeDirect/
// SearchDocsDirect narrow with codeKinds/docKinds, and what the /search HTTP
// route (cmd/recall/servesnapshots.go) calls for the UI's "all" filter —
// going through the identical Resolve + Index.Search + RecordSearch path the
// MCP search tools use, so the search-metrics counters see UI searches too.
func SearchDirect(ctx context.Context, d Deps, mapName, query string, limit int, kinds map[string]bool, sel index.Select, cutoff bool) (string, error) {
	limit = clampLimit(limit, !sel.Empty())
	resolved, err := d.Resolver.Resolve(ctx, mapName, query)
	if err != nil {
		return "", err
	}
	var merged []index.Hit
	var notes []string
	selected := 0
	routed := make([]string, 0, len(resolved))
	for _, r := range resolved {
		routed = append(routed, r.Label)
		// A map holding none of the requested kinds is searched for what it does
		// hold, and the reply says so — the same widening runSearchRO applies, so
		// the UI's kind filter cannot answer a bare count: 0 where the MCP tool
		// answers with results.
		mapKinds, note := searchKinds(toolNameForKinds(kinds), kinds, searchedMap{Label: r.Label, Graph: r.Graph})
		// The widening note describes the TOOL's scope. A caller that named its
		// own kinds has superseded that scope, and reporting it then tells them
		// every kind was searched when in fact only theirs was.
		if note != "" && len(sel.Kinds) == 0 {
			notes = append(notes, note)
		}
		if !sel.Empty() {
			selected += r.Index.CountMatching(mapKinds, "", sel)
			// Same vocabulary note runSearchRO attaches, so the UI's search
			// cannot rank silently around an attribute the map does not carry.
			if n := attrVocabularyNote(r.Index, mapKinds, "", sel, r.Label); n != "" {
				notes = append(notes, n)
			}
		}
		hits, err := r.Index.Search(ctx, query, mapKinds, "", sel, limit)
		if err != nil {
			return "", err
		}
		if cutoff {
			hits = index.CutAtLargestGap(hits)
		}
		d.Resolver.RecordSearch(r.Label, len(hits))
		weighByRoute(hits, r.RouteScore)
		merged = append(merged, hits...)
	}
	// Re-rank merged cross-project hits by their fused rank score, then truncate
	// to limit. Single-map results come back in exactly the order Search
	// produced them.
	sortHitsByFused(merged)
	if len(merged) > limit {
		merged = merged[:limit]
	}
	results := make([]map[string]any, len(merged))
	for i, h := range merged {
		results[i] = briefHit(h)
	}
	out := map[string]any{
		"query":    query,
		"routedTo": routed,
		"count":    len(results),
		"results":  results,
	}
	if !sel.Empty() {
		out["selected"] = selected
	}
	if n := selectionNote(sel, selected, len(results)); n != "" {
		notes = append(notes, n)
	}
	if len(notes) > 0 {
		out["note"] = strings.Join(notes, " ")
	}
	return jsonString(out)
}

// toolNameForKinds names the caller in a widening note. SearchDirect is reached
// from the HTTP /search route and from in-binary callers, which have no MCP tool
// name of their own, so the note names the equivalent tool a reader would
// recognize.
func toolNameForKinds(kinds map[string]bool) string {
	switch {
	case len(kinds) == 0:
		return "search"
	case sameKinds(kinds, docKinds):
		return "search_graph_docs"
	default:
		return "search_graph"
	}
}

// sameKinds reports whether two kind sets hold the same kinds.
func sameKinds(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// SearchDirectIndex runs a search directly against one fixed Index — Mode A
// (the live single-graph server, cmd/recall/serve.go), which has no
// per-project Resolver to route through. repoFilter narrows to one repo
// namespace when the server serves more than one (-repo a=...,b=...); empty
// searches every served repo. Returns the identical brief-hit JSON shape
// SearchDirect/the MCP search tools return (no "routedTo" key, matching
// registerSearchCode/registerSearchDocs's own Mode A reply), and goes through
// the same ix.Search call those tools make, so it records into the same
// SearchStats.
func SearchDirectIndex(ctx context.Context, ix *index.Index, query, repoFilter string, limit int, kinds map[string]bool, sel index.Select, cutoff bool) (string, error) {
	limit = clampLimit(limit, !sel.Empty())
	hits, err := ix.Search(ctx, query, kinds, repoFilter, sel, limit)
	if err != nil {
		return "", err
	}
	if cutoff {
		hits = index.CutAtLargestGap(hits)
	}
	results := make([]map[string]any, len(hits))
	for i, h := range hits {
		results[i] = briefHit(h)
	}
	out := map[string]any{"query": query, "count": len(hits), "results": results}
	if !sel.Empty() {
		selected := ix.CountMatching(kinds, repoFilter, sel)
		out["selected"] = selected
		if n := selectionNote(sel, selected, len(hits)); n != "" {
			out["note"] = n
		}
	}
	return jsonString(out)
}

// RememberDirect writes one note into the agent-notes map (Deps.MemoryMap).
// Direct-call twin of the remember tool; like it, there is no map to choose.
// ttl is as ParseTTL returns it: 0 for the server default, TTLNever for never.
func RememberDirect(ctx context.Context, d Deps, text, title string, attrs map[string]string, ttl time.Duration) (string, error) {
	out, err := remember(ctx, d, text, title, attrs, ttl)
	if err != nil {
		return "", err
	}
	return jsonString(out)
}

// RecallDirect reads the newest notes (or, with a query, the best-matching
// ones) from the agent-notes map. Direct-call twin of the recall tool.
func RecallDirect(ctx context.Context, d Deps, query string, limit int) (string, error) {
	out, err := recall(ctx, d, query, limit)
	if err != nil {
		return "", err
	}
	return jsonString(out)
}

// ForgetDirect removes one note from the agent-notes map by qualified name.
// Direct-call twin of the forget tool.
func ForgetDirect(ctx context.Context, d Deps, qn string) (string, error) {
	out, err := forget(ctx, d, qn)
	if err != nil {
		return "", err
	}
	return jsonString(out)
}

// GetNodeDirect returns the full node JSON (content + relationships) for a qn.
// Direct-call twin of get_graph_node.
func GetNodeDirect(ctx context.Context, d Deps, qn string) (string, error) {
	if qn == "" {
		return "", fmt.Errorf("qn is required")
	}
	r, err := d.Resolver.ResolveByQN(ctx, qn)
	if err != nil {
		return "", err
	}
	out, ok := NodeJSON(r.Graph, qn)
	if !ok {
		return "", fmt.Errorf("node not found: %s", qn)
	}
	return jsonString(out)
}

// ImpactDirect returns the blast radius of changing seeds. Direct-call twin of
// get_graph_impact.
func ImpactDirect(ctx context.Context, d Deps, seeds []string, depth, maxNodes int) (string, error) {
	if len(seeds) == 0 {
		return "", fmt.Errorf("seeds is required")
	}
	r, err := d.Resolver.ResolveByQN(ctx, seeds[0])
	if err != nil {
		return "", err
	}
	return ImpactDirectGraph(r.Graph, seeds, depth, maxNodes)
}

// ImpactDirectGraph is ImpactDirect over a graph the caller already holds —
// `xray-memory impact` loads one from a snapshot with no Resolver. Keeping the
// defaults here means the CLI and get_graph_impact answer the same question.
func ImpactDirectGraph(g *graph.Graph, seeds []string, depth, maxNodes int) (string, error) {
	if len(seeds) == 0 {
		return "", fmt.Errorf("seeds is required")
	}
	if depth <= 0 {
		depth = 2
	}
	if maxNodes <= 0 {
		maxNodes = 200
	}
	return jsonString(impactResult(g, seeds, depth, maxNodes))
}

// StatsDirect returns code-graph stats for the project a query routes to (or an
// explicit project). Direct-call twin of get_graph_stats, including the same
// "_metrics" block.
func StatsDirect(ctx context.Context, d Deps, mapName, query string) (string, error) {
	resolved, err := d.Resolver.Resolve(ctx, mapName, query)
	if err != nil {
		return "", err
	}
	stats := make(map[string]any, len(resolved)+1)
	for _, r := range resolved {
		stats[r.Label] = mapStatsEntry(r)
	}
	stats["_metrics"] = d.Resolver.Metrics()
	return jsonString(stats)
}
