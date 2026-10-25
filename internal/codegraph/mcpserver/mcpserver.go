// Package mcpserver exposes the code graph over MCP so X-Ray's voice/chat layer
// and external coding agents can search and traverse code as their primary
// structural + semantic source.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/rs/zerolog"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/graph"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/index"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/ui"
)

// Updater re-indexes changed files into the live graph. Implemented by the
// Indexer; lets the update_code_graph tool refresh without a hook.
//
// skipped counts files this build cannot extract — code files in a CGO-free
// binary, whose Tree-sitter parser is a stub. They are left untouched in the
// graph. It is reported rather than folded into changed/removed so a caller can
// tell "your edit produced no nodes" from "this build cannot read your file".
type Updater interface {
	Update(ctx context.Context, paths []string) (changed, removed, skipped int, unowned []string, err error)
}

// Note is one thing an agent asked the server to remember, and Remembered is
// where it landed. They are declared HERE rather than in engine because engine
// imports this package (its Registry implements Resolver), so the seam has to
// sit on this side; engine aliases them.
type Note struct {
	Text  string            // required; the memory itself, embedded and previewed verbatim
	Title string            // optional short name
	Attrs map[string]string // optional caller facets (session, topic); the server adds "at" and "expires"
	TTL   time.Duration     // how long to keep it: 0 = the server default, TTLNever (negative) = forever
}

// Remembered reports where a note landed.
type Remembered struct {
	Map     string
	QN      string
	At      time.Time
	Expires time.Time // zero when the note never expires
	Notes   int       // Memory nodes in the map after this one
}

// Forgotten reports a note removed by forget.
type Forgotten struct {
	Map   string
	QN    string
	Notes int // Memory nodes left in the map
}

// Rememberer writes one note into a memory map so a later session can find it,
// and removes one on request. Implemented by engine.Registry; a nil Deps.Memory
// leaves the remember and forget tools unregistered rather than
// registered-and-failing.
type Rememberer interface {
	Remember(ctx context.Context, label string, n Note) (Remembered, error)
	// Forget removes the note qn from the map label. It refuses anything that
	// is not a Memory node in a notes map: forget is the undo of remember, not
	// a general delete.
	Forget(ctx context.Context, label, qn string) (Forgotten, error)
}

// Deps are the graph + index + updater the tools operate on.
//
// Mode A (local live server, New) sets Graph/Index/Updater to fixed instances.
// Mode B (read-only snapshot server, NewReadOnly) sets Resolver instead, and
// each tool resolves the right per-project graph/index per call.
type Deps struct {
	Graph    *graph.Graph
	Index    *index.Index
	Updater  Updater
	Resolver Resolver // set in read-only mode; nil in live mode
	Logger   zerolog.Logger

	// SkillsMap names the loaded map (via Resolver) that backs list_skills /
	// get_skill — a "Doc" node per SKILL.md. Empty disables both tools; only
	// meaningful alongside a non-nil Resolver (Mode B).
	SkillsMap string

	// Memory backs the remember tool and MemoryMap is the agent-notes map every
	// note goes into; either empty disables the tool. The caller cannot choose
	// the map: that would let an agent write prose into a code or catalog map.
	Memory    Rememberer
	MemoryMap string
	// MemoryTTL is the lifetime a note gets when it names none; it is stated
	// in the remember tool's description so the caller knows what silence
	// means. Zero reads as DefaultMemoryTTL, negative as never.
	MemoryTTL time.Duration
}

// Resolved is one map's graph + index, returned by a Resolver. It is the
// per-call substitute for Deps.Graph/Deps.Index in read-only (snapshot) mode.
type Resolved struct {
	Label string
	Graph *graph.Graph
	Index *index.Index

	// Summary and SummarySource are the map's router input and where it came
	// from ("explicit"|"readme"|"stats") — carried here (not just on /status)
	// so get_graph_stats/StatsDirect can surface them for diagnosis without a
	// second round trip through the registry. See engine.SnapshotHeader.
	Summary       string
	SummarySource string

	// RouteScore is how well this map matched the route — 1 when the caller
	// named it, else the cosine of its summary vector to the query. It is the
	// weight a caller merging several maps' hits must apply, because a hit's
	// own rank score is only meaningful WITHIN its map: every map's rank-1 hit
	// carries the same score, so merging on it alone round-robins the maps and
	// lifts an unrelated map's best hit above a good map's second. A resolve
	// that names a single map leaves this 1.
	RouteScore float64
}

// Resolver routes a tool call to the right loaded map's graph. This
// interface is domain-neutral: a "map" is any loaded graph — code, a service
// dependency graph, or another domain — identified by its label. Search tools
// route by (mapName, query): an explicit map, else a semantic match on the query,
// and may fan out to several maps. Node-keyed tools route by the map prefix of a
// namespaced qualified name.
//
// UpdateByPaths supports the writable worked-copy mode: a remote client edits the
// in-memory graph by file path; the registry routes each path to its owning map,
// re-parses + re-embeds only the changed nodes, and marks that map dirty so it is
// flushed back to its snapshot before eviction / on shutdown.
//
// RecordSearch and Metrics back the search-quality metrics surface
// (docs/todo/search-metrics.md): RecordSearch attributes one resolved map's
// search + hit count to the registry's per-map counters — mcpserver is the
// only layer that knows both a search's resolved map label (from Resolve)
// and its returned hit count (from that map's Index.Search), so it is the
// one that must report the pairing back. Metrics returns a JSON-friendly
// snapshot of everything the registry tracks (routing outcomes, per-map
// totals, map loads/evictions, and the shared query-mechanics search stats),
// consumed by get_graph_stats and the /metrics HTTP endpoint.
type Resolver interface {
	Resolve(ctx context.Context, mapName, query string) ([]Resolved, error)
	ResolveByQN(ctx context.Context, qn string) (Resolved, error)
	UpdateByPaths(ctx context.Context, paths []string) (changed, removed, skipped int, unowned []string, err error)
	RecordSearch(label string, hits int)
	Metrics() map[string]any
}

// New returns an HTTP handler exposing the recall MCP tools over a single
// live graph (Mode A — local in-repo server, includes update_code_graph).
func New(d Deps) http.Handler {
	s := server.NewMCPServer("recall-mcp", "0.1.0")

	registerSearchCode(s, d)
	registerSearchDocs(s, d)
	registerGetNode(s, d)
	registerImpact(s, d)
	registerRisk(s, d)
	registerStats(s, d)
	registerUpdate(s, d)
	registerRecall(s, d)
	registerRemember(s, d)

	// MCP App (SEP-1865): the interactive doc/code graph — a ui:// resource +
	// show_code_graph / code_graph_data tools. Renders inline in MCP-Apps hosts.
	ui.Register(s, d.Graph)

	return server.NewStreamableHTTPServer(s)
}

// NewReadOnly returns an HTTP handler exposing the snapshot-routed tools (Mode B
// — online long-term memory / worked copy). Reads route per call through
// d.Resolver to the right per-project snapshot graph; update_code_graph edits the
// in-memory worked copy (the registry flushes dirty projects back to disk before
// eviction / on shutdown). The name is retained for back-compat; the server is a
// persistent worked copy, not strictly read-only.
func NewReadOnly(d Deps) http.Handler {
	s := server.NewMCPServer("recall-mcp", "0.1.0")

	registerSearchCodeRO(s, d)
	registerSearchDocsRO(s, d)
	registerGetNodeRO(s, d)
	registerImpactRO(s, d)
	registerRiskRO(s, d)
	registerStatsRO(s, d)
	registerUpdateRO(s, d)
	registerSkillTools(s, d)
	registerRecall(s, d)
	registerRemember(s, d)

	return server.NewStreamableHTTPServer(s)
}

// RegisterReadTools adds the five READ-ONLY code-graph tools (search_graph,
// search_graph_docs, get_graph_node, get_graph_impact,
// get_graph_stats) to an EXISTING MCP server — it does NOT register
// update_code_graph. This lets a host application serve the code graph from its
// own auth-gated /mcp endpoint alongside its own tools, sharing one
// session/bearer/OAuth gate, scope map, and rate limiter, without owning a second
// HTTP server.
//
// It selects snapshot-routed (Resolver) or live (Graph/Index) handlers from the
// same flag the constructors use: a non-nil d.Resolver means snapshot mode. A
// static build runs snapshot mode (CGO-free); live mode is for the in-process
// recall CLI server.
func RegisterReadTools(s *server.MCPServer, d Deps) {
	if d.Resolver != nil {
		registerSearchCodeRO(s, d)
		registerSearchDocsRO(s, d)
		registerGetNodeRO(s, d)
		registerImpactRO(s, d)
		registerRiskRO(s, d)
		registerStatsRO(s, d)
		return
	}
	registerSearchCode(s, d)
	registerSearchDocs(s, d)
	registerGetNode(s, d)
	registerImpact(s, d)
	registerRisk(s, d)
	registerStats(s, d)
}

// logged wraps a tool handler with one structured debug line PER CALL carrying
// the tool name, its arguments, the outcome (error vs result size in bytes), and
// the wall-clock latency. This is the only per-request observability for the MCP
// surface; enable it with -log-level=debug. The wrapper is uniform across every
// tool so coverage and field names never drift between handlers.
func logged(d Deps, tool string, h server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		t0 := time.Now()
		res, err := h(ctx, req)

		ev := d.Logger.Debug().Str("tool", tool).Dur("latency", time.Since(t0))
		// Arguments, compactly — the metric that explains the latency/result.
		if args := req.GetArguments(); len(args) > 0 {
			if b, mErr := json.Marshal(args); mErr == nil {
				ev = ev.RawJSON("args", b)
			}
		}
		switch {
		case err != nil:
			ev.Err(err).Msg("tool call failed")
		case res != nil && res.IsError:
			ev.Bool("toolError", true).Int("resultBytes", resultBytes(res)).Msg("tool call error")
		default:
			ev.Int("resultBytes", resultBytes(res)).Msg("tool call")
		}
		return res, err
	}
}

// resultBytes is the size of a tool result's text payload — a cheap proxy for
// "how much did this call return" (result count is tool-specific; bytes is not).
func resultBytes(res *mcp.CallToolResult) int {
	if res == nil {
		return 0
	}
	n := 0
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			n += len(tc.Text)
		}
	}
	return n
}

// readOnlyTool returns the annotations every recall tool carries: they only
// READ a local in-memory graph — no mutation (non-destructive), repeatable
// (idempotent), and no external/network effects (closed-world). Without these,
// MCP clients default to the unsafe assumption (destructive + open-world) and
// flag the tools accordingly in their pickers.
func readOnlyTool() mcp.ToolOption {
	return mcp.WithToolAnnotation(mcp.ToolAnnotation{
		ReadOnlyHint:    boolPtr(true),
		DestructiveHint: boolPtr(false),
		IdempotentHint:  boolPtr(true),
		OpenWorldHint:   boolPtr(false),
	})
}

func boolPtr(b bool) *bool { return &b }

// Lean-reply tuning. Every reply stays small: search returns ranked POINTERS
// (qn + short preview), get_graph_node returns identity + relationships (capped),
// never raw file dumps. The graph answers "where / how does this connect"; the
// caller reads the file at file:line for literal source.
const (
	defaultSearchLimit = 5   // results per search reply
	maxSearchLimit     = 20  // hard cap so a caller can't request limit=500
	maxPreviewChars    = 200 // per-result preview cap
	maxQueryGraphEdges = 50  // edges per query_graph reply

	// maxSelectionLimit caps a reply that SELECTS a named set (an attr filter)
	// rather than ranking the whole map by relevance. It is higher than
	// maxSearchLimit because the two truncations do not mean the same thing:
	// cutting a ranked list drops a less-relevant tail, while cutting a
	// selection drops members the caller asked for and changes the answer. It
	// is still bounded — the set is read by an agent with a context window, and
	// a reply is a pointer list, not a data dump — and a selection cut by it
	// says so, with the set's real size, rather than truncating silently.
	maxSelectionLimit = 100
)

// clampLimit bounds a requested reply size. A selection gets the larger cap;
// see maxSelectionLimit for why the two differ.
func clampLimit(requested int, selecting bool) int {
	max := maxSearchLimit
	if selecting {
		max = maxSelectionLimit
	}
	if requested <= 0 {
		return defaultSearchLimit
	}
	if requested > max {
		return max
	}
	return requested
}

// capPreview trims a result preview to maxPreviewChars runes, appending an
// ellipsis when truncated, so a single doc/manifest body cannot flood the reply.
// Rune-based so a multi-byte character is never split (which would emit invalid
// UTF-8 into the JSON reply).
func capPreview(s string) string {
	if utf8.RuneCountInString(s) <= maxPreviewChars {
		return s
	}
	r := []rune(s)
	return string(r[:maxPreviewChars]) + "…"
}

func jsonResult(v any) (*mcp.CallToolResult, error) {
	b, _ := json.MarshalIndent(v, "", "  ")
	return mcp.NewToolResultText(string(b)), nil
}

// Node-kind groups for the two search surfaces. Code search covers source
// symbols, K8s manifests and SQL schema objects (all answer "how / what's
// deployed / what's stored"); doc search covers prose (the "why / what").
var (
	codeKinds = map[string]bool{
		"Func": true, "Class": true, "Type": true, "Test": true,
		"File": true, "Manifest": true,
		"Table": true, "View": true, "Routine": true, "Trigger": true, "Sequence": true,
		"Deployment": true, "Config": true, "Image": true,
	}
	docKinds = map[string]bool{
		"Doc": true, "Section": true,
	}
)

// KindsForFilter maps a UI-facing filter value onto the SAME codeKinds/docKinds
// groups search_graph/search_graph_docs already search over — the /search HTTP
// route's kind param (mirroring the browser UI's All/Docs/Code segmented
// control) reuses this rather than redefining the split. "code" and "docs"
// narrow to their respective group; anything else (including "" and "all")
// searches every kind, matching search_graph's own unfiltered default.
func KindsForFilter(filter string) map[string]bool {
	switch filter {
	case "code":
		return codeKinds
	case "docs":
		return docKinds
	default:
		return nil
	}
}

// Search is ALWAYS brief: one call returns ranked pointers (qn, kind, title,
// file:line, score, short preview) — cheap, no knobs. The qn then feeds
// get_graph_node (relations, both directions) or get_graph_impact; the caller
// reads the file at file:line for literal source.

// briefHit renders one hit as a lean pointer with a short preview. repo is
// included whenever the node carries one, so a caller that resolves several
// maps at once (the /search HTTP route, cross-project semantic routing) can
// still tell which map produced each hit without a second lookup.
func briefHit(h index.Hit) map[string]any {
	out := map[string]any{
		"qn":      h.QN,
		"kind":    h.Kind,
		"title":   h.Title,
		"file":    h.File,
		"line":    h.Line,
		"score":   h.Score,
		"preview": capPreview(h.Preview),
	}
	if h.Repo != "" {
		out["repo"] = h.Repo
	}
	// The containing node, for a child hit. A part was a dead end without it:
	// the academy map answered "most experienced student" with five Answer
	// parts, and nothing in the hit led back to the Student entity holding the
	// years attribute short of a get_graph_node per hit. Omitted for top-level
	// nodes, so a code hit's shape is unchanged.
	if h.Parent != "" {
		out["parent"] = h.Parent
	}
	return out
}

// mapStatsEntry composes one resolved map's stats + router-summary diagnosis
// for get_graph_stats / StatsDirect — the two callers share this so the shapes
// can't drift (mirrors the existing comment on Resolver.Metrics). Nested under
// "stats" rather than flattened, so the pre-existing graph.Stats shape
// (Nodes/Edges/ByKind/ByEdge/Embedded) is untouched and "summary"/
// "summarySource" sit alongside it. Summary/SummarySource are "" for a Mode A
// (live, non-snapshot) resolve, since only the snapshot pipeline populates them.
func mapStatsEntry(r Resolved) map[string]any {
	out := map[string]any{
		"stats":         r.Graph.Stats(),
		"summary":       r.Summary,
		"summarySource": r.SummarySource,
	}
	// The attribute keys each kind carries — the vocabulary attr/order need.
	// Present only for a map whose nodes carry attributes (a catalog), so a
	// code map's entry is unchanged.
	if a := attrStats(r.Graph); a != nil {
		out["attrs"] = a
	}
	return out
}

// argBool reads an optional boolean argument, defaulting to false so a caller
// that says nothing keeps the full ranked list.
func argBool(args map[string]any, key string) bool {
	b, _ := args[key].(bool)
	return b
}

// argStr reads an optional string argument, tolerating a missing or non-string
// value as empty rather than erroring — the MCP schema already marks it optional.
func argStr(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

func runSearch(ctx context.Context, d Deps, toolName string, args map[string]any, kinds map[string]bool) (*mcp.CallToolResult, error) {
	query, _ := args["query"].(string)
	repo, _ := args["repo"].(string)
	sel, err := parseSelect(args)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	requested := 0
	if l, ok := args["limit"].(float64); ok {
		requested = int(l)
	}
	limit := clampLimit(requested, !sel.Empty())
	kinds, note := searchKinds(toolName, kinds, searchedMap{Graph: d.Graph})
	hits, err := d.Index.Search(ctx, query, kinds, repo, sel, limit)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if argBool(args, "cutoff") {
		hits = index.CutAtLargestGap(hits)
	}
	results := make([]map[string]any, len(hits))
	for i, h := range hits {
		results[i] = briefHit(h)
	}
	out := map[string]any{"query": query, "count": len(hits), "results": results}
	if !sel.Empty() {
		selected := d.Index.CountMatching(kinds, repo, sel)
		out["selected"] = selected
		// An order or attr naming an attribute the set does not carry is a
		// vocabulary mismatch the ranking cannot show; the note names it.
		if n := attrVocabularyNote(d.Index, kinds, repo, sel, ""); n != "" {
			note = strings.TrimSpace(note + " " + n)
		}
		if n := selectionNote(sel, selected, len(hits)); n != "" {
			note = strings.TrimSpace(note + " " + n)
		}
	}
	if note != "" {
		out["note"] = note
	}
	return jsonResult(out)
}

// search_graph — find source symbols and K8s manifests by meaning ("how").
func registerSearchCode(s *server.MCPServer, d Deps) {
	s.AddTool(mcp.NewTool("search_graph",
		readOnlyTool(),
		mcp.WithDescription("Find nodes in a loaded mental map by meaning — code (functions, classes, files, K8s manifests), services, or another domain. Returns ranked pointers (qn, file:line, short preview). Feed a qn to get_graph_node for full content + relations in both directions (what it depends on and what depends on it), or get_graph_impact for blast radius. For code repos, rationale/decisions live in search_graph_docs."),
		mcp.WithString("query", mcp.Description(searchQueryDesc), mcp.Required()),
		mcp.WithString("repo", mcp.Description("Optional repository filter when several repos are served")),
		mcp.WithNumber("limit", mcp.Description(searchLimitDesc)),
		mcp.WithString("kind", mcp.Description(searchKindDesc)),
		mcp.WithString("attr", mcp.Description(searchAttrDesc)),
		mcp.WithString("order", mcp.Description(searchOrderDesc)),
		mcp.WithBoolean("cutoff", mcp.Description("Drop the ranked tail at its steepest score drop, keeping only hits above the cliff. Adapts to how many answers the query has; trades recall for precision")),
	), logged(d, "search_graph", func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return runSearch(ctx, d, "search_graph", req.GetArguments(), codeKinds)
	}))
}

// search_graph_docs — find documentation prose by meaning ("why / what").
func registerSearchDocs(s *server.MCPServer, d Deps) {
	s.AddTool(mcp.NewTool("search_graph_docs",
		readOnlyTool(),
		mcp.WithDescription("Find DOCUMENTATION by meaning: ADRs, todos, READMEs and their sections — the rationale, decisions, design, and conventions (the why and what). Returns ranked pointers (qn, file:line, short preview). The preview answers most 'why' questions; read the file at file:line for the full text, or feed the qn to get_graph_node for its place in the graph. For implementation, use search_graph."),
		mcp.WithString("query", mcp.Description("Natural-language description of the topic, decision, or concept you are looking for")),
		mcp.WithString("repo", mcp.Description("Optional repository filter when several repos are served")),
		mcp.WithNumber("limit", mcp.Description(searchLimitDesc)),
		mcp.WithString("kind", mcp.Description(searchKindDesc)),
		mcp.WithString("attr", mcp.Description(searchAttrDesc)),
		mcp.WithString("order", mcp.Description(searchOrderDesc)),
		mcp.WithBoolean("cutoff", mcp.Description("Drop the ranked tail at its steepest score drop, keeping only hits above the cliff. Adapts to how many answers the query has; trades recall for precision")),
	), logged(d, "search_graph_docs", func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return runSearch(ctx, d, "search_graph_docs", req.GetArguments(), docKinds)
	}))
}

// get_graph_node — return one node's full CONTENT (the graph's own stored text:
// whole doc, section body, manifest YAML, or code signature) plus its
// relationships (calls/called_by/imports). Content comes from memory, not a file
// read.
func registerGetNode(s *server.MCPServer, d Deps) {
	s.AddTool(mcp.NewTool("get_graph_node",
		readOnlyTool(),
		mcp.WithDescription("Return the full content of one node by its qualified name (qn) from a search hit: a doc's complete text, a section's body, a manifest's YAML, or a code symbol's signature — plus its relationships (calls, called_by, imports). One call gives the node's content and how it connects."),
		mcp.WithString("qn", mcp.Description("Qualified name from a search hit (e.g. repo//docs/adr/043.md or repo//internal/foo.go::Bar.Baz)"), mcp.Required()),
	), logged(d, "get_graph_node", func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		qn, _ := req.GetArguments()["qn"].(string)
		if qn == "" {
			return mcp.NewToolResultError("qn is required"), nil
		}
		out, ok := NodeJSON(d.Graph, qn)
		if !ok {
			return mcp.NewToolResultError("node not found: " + qn), nil
		}
		return jsonResult(out)
	}))
}

// NodeJSON builds the get_graph_node reply for one qn against a specific graph:
// identity + full content (from memory) + capped relationships. Shared by the
// MCP get_graph_node tools and the browser /node endpoint so all three return the
// identical shape. Returns (nil,false) when the qn is not a node.
func NodeJSON(g *graph.Graph, qn string) (map[string]any, bool) {
	n, ok := g.Get(qn)
	if !ok {
		return nil, false
	}
	out := map[string]any{
		"qn":         n.QN,
		"repo":       n.Repo,
		"kind":       n.Kind,
		"title":      mcpNodeTitle(n),
		"file":       n.FilePath,
		"line_start": n.LineStart,
		"line_end":   n.LineEnd,
		"language":   n.Language,
		"content":    nodeContent(g, n),
	}
	// The node's own properties. A catalog entry carries its facts here as
	// key/values (years, role, tier); code nodes carry none. Without this the
	// only way to read a property is to parse it back out of the prose in
	// content, which a caller cannot do reliably and cannot compare across
	// nodes at all.
	if len(n.Attrs) > 0 {
		out["attrs"] = n.Attrs
	}
	// Relationships as secondary metadata — the connections this node has.
	// calls/called_by/imports keep their established key names because the three
	// tool surfaces and the browser UI read them by those names.
	if v := edgeTargets(g.Callees(qn, "calls")); len(v) > 0 {
		out["calls"] = v
	}
	if v := edgeSources(g.Callers(qn, "calls")); len(v) > 0 {
		out["called_by"] = v
	}
	if v := edgeTargets(g.Callees(qn, "imports")); len(v) > 0 {
		out["imports"] = v
	}
	// The node's PARENT. Incoming "contains" only: a node has one parent, so this
	// is a single entry, while the outgoing direction (a file's symbols, an
	// entry's fields) runs to hundreds and already drives nodeContent. A child
	// node carries only its own text, so without the way back a search hit on one
	// is a dead end — the caller can read the part and never reach the facts the
	// entity holds.
	if v := edgeSources(g.Callers(qn, "contains")); len(v) > 0 {
		out["contained_by"] = v
	}
	// Every OTHER relation the map declares is surfaced under its own name, so a
	// catalog's vocabulary is not limited to the relations the code extractors
	// happen to emit: a "peer" or "owns" link reaches callers instead of existing
	// in the graph unreported. Incoming edges take a "_by" suffix, mirroring
	// calls/called_by. Each list is capped by edgeEnds (maxRelated) so a densely
	// linked node cannot bloat the reply.
	for rel, v := range groupEdgeEnds(g.Callees(qn, ""), true) {
		out[rel] = v
	}
	for rel, v := range groupEdgeEnds(g.Callers(qn, ""), false) {
		out[rel+"_by"] = v
	}
	return out, true
}

// namedRelations are the relations NodeJSON already reports under fixed keys;
// groupEdgeEnds skips them so they are not also emitted generically.
var namedRelations = map[string]bool{"calls": true, "imports": true, "contains": true}

// groupEdgeEnds buckets edges by relation kind and renders each bucket's ends.
// "contains" is in the named set because NodeJSON reports the direction that is
// useful on its own: the parent, as contained_by. The outgoing direction stays
// unreported — it is structural (a file's symbols, a doc's sections), already
// drives nodeContent, and listing it would make a large file's reply the
// opposite of lean.
func groupEdgeEnds(edges []graph.Edge, target bool) map[string][]string {
	byRel := map[string][]graph.Edge{}
	for _, e := range edges {
		if namedRelations[e.Kind] {
			continue
		}
		byRel[e.Kind] = append(byRel[e.Kind], e)
	}
	if len(byRel) == 0 {
		return nil
	}
	out := make(map[string][]string, len(byRel))
	for rel, es := range byRel {
		if v := edgeEnds(es, target); len(v) > 0 {
			out[rel] = v
		}
	}
	return out
}

// nodeContent returns the node's full content. A Doc has no body of its own —
// reassemble it from its sections' full bodies, in order. Section and Manifest
// nodes carry their full Body; a code node has only its signature (Content).
func nodeContent(g *graph.Graph, n *graph.Node) string {
	if n.Kind == "Doc" {
		var b strings.Builder
		for _, e := range g.Callees(n.QN, "contains") {
			if c, ok := g.Get(e.Target); ok && c.Kind == "Section" {
				b.WriteString(c.Body)
				b.WriteString("\n\n")
			}
		}
		return strings.TrimSpace(b.String())
	}
	if n.Body != "" {
		return n.Body
	}
	if n.Content == "" && n.Kind == "File" {
		return fileOutline(g, n)
	}
	return n.Content
}

// maxOutline caps the symbols a File node lists.
const maxOutline = 80

// fileOutline is what a File node says for itself: it has no body, so list the
// symbols it holds, each with the qn to fetch. Without it a caller that asks for
// the file gets an empty string, and a model re-asks for the same file in a loop
// hoping for text that is not there.
func fileOutline(g *graph.Graph, n *graph.Node) string {
	var b strings.Builder
	b.WriteString("A file has no body of its own. Its symbols (call get_graph_node on a qn for the code):")
	shown := 0
	for _, e := range g.Callees(n.QN, "contains") {
		c, ok := g.Get(e.Target)
		if !ok {
			continue
		}
		if shown == maxOutline {
			b.WriteString("\n- ...")
			break
		}
		fmt.Fprintf(&b, "\n- %s %s (lines %d-%d) qn=%s", c.Kind, mcpNodeTitle(c), c.LineStart, c.LineEnd, c.QN)
		shown++
	}
	if shown == 0 {
		return ""
	}
	return b.String()
}

// maxRelated caps each relationship list in a get_graph_node reply so a hub node
// can't flood the response; the count is what matters for understanding.
const maxRelated = 100

// edgeTargets / edgeSources collect the distinct other-ends of a set of edges,
// capped at maxRelated.
func edgeTargets(edges []graph.Edge) []string { return edgeEnds(edges, true) }
func edgeSources(edges []graph.Edge) []string { return edgeEnds(edges, false) }

func edgeEnds(edges []graph.Edge, target bool) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range edges {
		v := e.Source
		if target {
			v = e.Target
		}
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
		if len(out) >= maxRelated {
			break
		}
	}
	return out
}

// mcpNodeTitle mirrors index.nodeTitle so get_graph_node reports the same display name
// search results use (parent-qualified for methods, path for File nodes).
func mcpNodeTitle(n *graph.Node) string {
	if n.Kind == "File" {
		return n.FilePath
	}
	if n.ParentName != "" {
		return n.ParentName + "." + n.Name
	}
	return n.Name
}

// get_graph_impact — blast radius of changing a set of symbols/files.
func registerImpact(s *server.MCPServer, d Deps) {
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
		depth := 2
		if dpt, ok := args["depth"].(float64); ok && dpt > 0 {
			depth = int(dpt)
		}
		maxNodes := 200
		if mn, ok := args["max_nodes"].(float64); ok && mn > 0 {
			maxNodes = int(mn)
		}
		return jsonResult(impactResult(d.Graph, seeds, depth, maxNodes))
	}))
}

// get_graph_risk — coarse LOW/MEDIUM/HIGH change-risk signal for one symbol.
func registerRisk(s *server.MCPServer, d Deps) {
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
		depth := 2
		if dpt, ok := args["depth"].(float64); ok && dpt > 0 {
			depth = int(dpt)
		}
		maxNodes := 200
		if mn, ok := args["max_nodes"].(float64); ok && mn > 0 {
			maxNodes = int(mn)
		}
		return jsonResult(d.Graph.Risk(qn, depth, maxNodes))
	}))
}

// get_graph_stats — quick sanity / overview.
func registerStats(s *server.MCPServer, d Deps) {
	s.AddTool(mcp.NewTool("get_graph_stats",
		readOnlyTool(),
		mcp.WithDescription("Return code graph statistics: node and edge counts by kind, and how many nodes are embedded."),
	), logged(d, "get_graph_stats", func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return jsonResult(d.Graph.Stats())
	}))
}

// update_code_graph — re-index changed files into the live graph. The ONLY
// write tool: it mutates the in-memory graph (read-only:false). Use it to make
// the graph reflect edits you just made when a file-watch hook did not run —
// pass the changed file paths (absolute, or relative to a served repo root).
func registerUpdate(s *server.MCPServer, d Deps) {
	s.AddTool(mcp.NewTool("update_code_graph",
		mcp.WithToolAnnotation(mcp.ToolAnnotation{
			ReadOnlyHint:    boolPtr(false),
			DestructiveHint: boolPtr(false), // replaces a file's nodes in place; not destructive to unrelated data
			IdempotentHint:  boolPtr(true),  // re-indexing the same unchanged file is a no-op
			OpenWorldHint:   boolPtr(false),
		}),
		mcp.WithDescription("Re-index changed files into the live code graph (re-parse, re-resolve dependents, re-embed only changed nodes) so search/trace/impact reflect your latest edits. Use after editing files if the auto-update hook did not run. Returns how many files changed/were removed."),
		mcp.WithArray("paths", mcp.Description("File paths to re-index. Absolute, or relative to a served repo root — a relative path must name an existing file under exactly one served repo; to record a DELETION pass an absolute path. Paths that resolve to no served repo come back in \"unowned\"."), mcp.Required()),
	), logged(d, "update_code_graph", func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if d.Updater == nil {
			return mcp.NewToolResultError("update not available (no updater wired)"), nil
		}
		paths := toStrings(req.GetArguments()["paths"])
		if len(paths) == 0 {
			return mcp.NewToolResultError("paths is required"), nil
		}
		changed, removed, skipped, unowned, err := d.Updater.Update(ctx, paths)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return jsonResult(updateResult(changed, removed, skipped, unowned))
	}))
}

func toStrings(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		if s, ok := x.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// updateResult shapes an update_code_graph reply. skipped and unowned are
// omitted when empty so the common case stays a two-field object, and each
// carries a plain-language note when present: a model that sees only a number
// will not infer that files were left stale on purpose, and may report the
// graph as refreshed. The two have different causes and different remedies —
// skipped means "this build cannot parse it", unowned means "I never found the
// file you named" — so they are never merged into one counter.
func updateResult(changed, removed, skipped int, unowned []string) map[string]any {
	out := map[string]any{"changed": changed, "removed": removed}
	var notes []string
	if skipped > 0 {
		out["skipped"] = skipped
		notes = append(notes, "skipped files need the Tree-sitter code parser, which this build does not carry; "+
			"they were left unchanged in the graph. Markdown and YAML are indexed normally. "+
			"To re-index code, use a CGO build or rebuild the snapshot offline with `xray-memory snapshot`.")
	}
	if len(unowned) > 0 {
		out["unowned"] = unowned
		notes = append(notes, "unowned paths resolved to no served repo and were NOT indexed — the graph is "+
			"unchanged for them. Check the spelling, or pass an absolute path: a relative path is resolved "+
			"against each served repo root and must name an existing file under exactly one of them.")
	}
	if len(notes) > 0 {
		out["note"] = strings.Join(notes, " ")
	}
	return out
}
