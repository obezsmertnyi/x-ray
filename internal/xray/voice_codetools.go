package xray

// Native (in-process) code-graph voice tools. These wrap the recall read
// functions DIRECTLY — exactly as defSearchMemory wraps memory.Search — with no
// MCP server, no in-process MCP client, and no protocol handshake. The functions
// in internal/recall/mcpserver/direct.go are plain Go calls into the resolver/
// index/graph, so the voice model reaches search_graph et al. with zero MCP hop.

import (
	"context"

	"github.com/rs/zerolog"
	"google.golang.org/genai"

	recallmcp "github.com/triageagent-dev/x-ray/v2/internal/codegraph/mcpserver"
)

// buildMemsnapVoiceTools returns the five read-only recall tools as native
// voiceToolDefs backed by the memory snapshot. deps must be non-nil.
//
// Headroom compression is applied via compressToolResult (headroomLimit, the
// same budget as the built-in and remote-MCP paths) to get_graph_node ONLY. That
// is the tool returning one body — a whole ADR, README, or source/YAML node —
// which is the token-heavy payload headroom targets (markdown docs, manifests,
// code). When xray-code was embedded as native tools (ADR-046) this wiring was
// missed and those bodies reached Gemini Live raw; now over-budget bodies
// truncate (full node still retrievable by qn — the reversible CCR contract),
// and the thin JSON envelope (qn, file, line, calls) survives the strip intact.
//
// The other four tools are DELIBERATELY exempt, because a flat char budget on a
// JSON LIST does not "compress" — it truncates the array mid-list, dropping
// entries while the envelope still claims count:N (live logs showed search_graph
// 2000B→500B cutting 5 hits to ~1.5). That is the row-dropping hazard ADR-038
// avoided for search_memory. Each is already bounded by its own list-aware
// limit: search_graph/search_graph_docs by the `limit` param, get_graph_impact
// by max_nodes; get_graph_stats is a small, BOUNDED object per resolved map —
// counts plus one router-summary string capped at maxSummaryChars (1200,
// engine/snapshot.go) — not the unbounded free text headroom targets, and the
// number of maps in one reply is itself capped by -route-fanout.
func buildMemsnapVoiceTools(deps *recallmcp.Deps, headroomLimit int, logger zerolog.Logger) []voiceToolDef {
	d := *deps
	strArg := func(args map[string]any, k string) string { s, _ := args[k].(string); return s }
	intArg := func(args map[string]any, k string) int {
		if f, ok := args[k].(float64); ok {
			return int(f)
		}
		return 0
	}
	// hr wraps a recall Direct call (named tool) so its result passes through
	// the headroom content budget, logging the "headroom compression" line just
	// like the built-in and remote-MCP paths. Errors pass through uncompressed;
	// headroomLimit<=0 is a passthrough. Returns a func so the (out, err) pair
	// from the Direct call can be forwarded in one expression.
	hr := func(tool string) func(string, error) (string, error) {
		return func(out string, err error) (string, error) {
			if err != nil {
				return out, err
			}
			return compressToolResult(out, tool, headroomLimit, logger), nil
		}
	}

	return []voiceToolDef{
		{
			decl: &genai.FunctionDeclaration{
				Name:        "search_graph",
				Description: "Find nodes in a loaded mental map by meaning — a graph of code (functions, classes, files, manifests), services (a service dependency map), or another domain. Returns ranked pointers (qn, short preview). Feed a qn to get_graph_node for the full content and relationships.",
				Parameters: &genai.Schema{
					Type:     genai.TypeObject,
					Required: []string{"query"},
					Properties: map[string]*genai.Schema{
						"query":   {Type: genai.TypeString, Description: "Natural-language description of what you are looking for (a symbol, a service, a concept)"},
						"project": {Type: genai.TypeString, Description: "Optional map/project to search; omit to route by meaning"},
						"limit":   {Type: genai.TypeInteger, Description: "Max results (default 5, max 20)"},
					},
				},
			},
			// Exempt from the char budget: returns a list of pointers, sized by `limit`.
			handler: func(ctx context.Context, args map[string]any) (string, error) {
				return recallmcp.SearchCodeDirect(ctx, d, strArg(args, "project"), strArg(args, "query"), intArg(args, "limit"))
			},
		},
		{
			decl: &genai.FunctionDeclaration{
				Name:        "search_graph_docs",
				Description: "Find DOCUMENTATION by meaning: ADRs, todos, READMEs and their sections — the rationale and decisions (the why/what). Returns ranked pointers; read the file at file:line or feed the qn to get_graph_node for the full text.",
				Parameters: &genai.Schema{
					Type:     genai.TypeObject,
					Required: []string{"query"},
					Properties: map[string]*genai.Schema{
						"query":   {Type: genai.TypeString, Description: "Natural-language description of the topic or decision"},
						"project": {Type: genai.TypeString, Description: "Optional project/repo to search; omit to route by meaning"},
						"limit":   {Type: genai.TypeInteger, Description: "Max results (default 5, max 20)"},
					},
				},
			},
			// Exempt from the char budget: returns a list of pointers, sized by `limit`.
			handler: func(ctx context.Context, args map[string]any) (string, error) {
				return recallmcp.SearchDocsDirect(ctx, d, strArg(args, "project"), strArg(args, "query"), intArg(args, "limit"))
			},
		},
		{
			decl: &genai.FunctionDeclaration{
				Name:        "get_graph_node",
				Description: "Return one node's full content by its qn from a search_graph hit — a code symbol's body, a doc's text, a manifest's YAML, or a service's description — PLUS its relations in BOTH directions: what it depends on / calls (calls, downstream) and what depends on / calls it (called_by, upstream). Use this to answer 'what does X depend on', 'what depends on X / breaks if X fails', or 'what does X call/use'. Pass only a qn that appeared in a search result.",
				Parameters: &genai.Schema{
					Type:     genai.TypeObject,
					Required: []string{"qn"},
					Properties: map[string]*genai.Schema{
						"qn": {Type: genai.TypeString, Description: "Qualified name from a search hit (e.g. repo//internal/foo.go::Bar.Baz)"},
					},
				},
			},
			handler: func(ctx context.Context, args map[string]any) (string, error) {
				return hr("get_graph_node")(recallmcp.GetNodeDirect(ctx, d, strArg(args, "qn")))
			},
		},
		{
			decl: &genai.FunctionDeclaration{
				Name:        "get_graph_impact",
				Description: "Compute the blast radius of changing one or more symbols: a bidirectional traversal over calls/imports returning impacted symbols and their distance.",
				Parameters: &genai.Schema{
					Type:     genai.TypeObject,
					Required: []string{"seeds"},
					Properties: map[string]*genai.Schema{
						"seeds":     {Type: genai.TypeArray, Description: "Qualified names of the symbols being changed", Items: &genai.Schema{Type: genai.TypeString}},
						"depth":     {Type: genai.TypeInteger, Description: "Max traversal depth (default 2)"},
						"max_nodes": {Type: genai.TypeInteger, Description: "Cap on impacted nodes (default 200)"},
					},
				},
			},
			handler: func(ctx context.Context, args map[string]any) (string, error) {
				var seeds []string
				if raw, ok := args["seeds"].([]any); ok {
					for _, s := range raw {
						if str, ok := s.(string); ok {
							seeds = append(seeds, str)
						}
					}
				}
				// Exempt: returns a node list already bounded by max_nodes.
				return recallmcp.ImpactDirect(ctx, d, seeds, intArg(args, "depth"), intArg(args, "max_nodes"))
			},
		},
		{
			decl: &genai.FunctionDeclaration{
				Name:        "get_graph_stats",
				Description: "Return statistics for a loaded map: node and edge counts by kind. Pass project/map, or omit and pass a query to stat the one it routes to.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"project": {Type: genai.TypeString, Description: "Project to report on"},
						"query":   {Type: genai.TypeString, Description: "If project is omitted, the project this query routes to is reported"},
					},
				},
			},
			// Exempt: small fixed counts object, no free-text body to strip.
			handler: func(ctx context.Context, args map[string]any) (string, error) {
				return recallmcp.StatsDirect(ctx, d, strArg(args, "project"), strArg(args, "query"))
			},
		},
	}
}
