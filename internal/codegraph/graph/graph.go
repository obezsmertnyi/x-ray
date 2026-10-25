// Package graph is the in-memory code graph: nodes keyed by qualified name,
// edges, and the adjacency indexes that power callers/callees/impact queries.
// It is rebuilt from scratch on each service start, so there is no
// persistence layer here — at ~10^3-10^4 nodes a full re-parse is cheap.
package graph

import (
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/parser"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/sqlddl"
)

// Node is a stored code symbol plus its embedding (set after indexing).
type Node struct {
	parser.NodeInfo
	Repo string    // owning repository label (multi-repo serving)
	QN   string    // repo-qualified name (cached); unique across repos
	Vec  []float32 // embedding, nil until indexed
}

// Edge is a stored directed relationship.
type Edge struct {
	Kind   string
	Source string
	Target string
	File   string
	Line   int
	// Extra is the extractor's hints for the edge (parser.EdgeInfo.Extra),
	// carried through linking so a reader of the graph sees what the extractor
	// recorded — a compose env key's inline value, a SQL reference's kind —
	// rather than only its endpoints. Shared with the parsed result; read-only.
	Extra map[string]string
}

// Graph holds the whole code graph. It is read-mostly after Build; the mutex
// guards the rare full rebuild.
type Graph struct {
	mu    sync.RWMutex
	nodes map[string]*Node // qn -> node
	order []string         // stable insertion order

	out map[string][]Edge // source qn -> outgoing edges
	in  map[string][]Edge // target qn -> incoming edges
}

// New returns an empty graph.
func New() *Graph {
	return &Graph{
		nodes: map[string]*Node{},
		out:   map[string][]Edge{},
		in:    map[string][]Edge{},
	}
}

// RepoGraph is one repository's parsed nodes and edges, fed to Build.
type RepoGraph struct {
	Repo  string
	Nodes []parser.NodeInfo
	Edges []parser.EdgeInfo
}

// repoQN namespaces a qualified name by repo so identical paths in different
// repos never collide when many repos share one service.
func repoQN(repo, qn string) string {
	if repo == "" {
		return qn
	}
	return repo + "//" + qn
}

// LinkStats reports how call-edge resolution went, for observability.
type LinkStats struct {
	CallEdges     int
	ResolvedCalls int // rewritten to a qualified node QN
	ExternalCalls int // stdlib / third-party / unresolvable (kept as-is)
	ResolvedRatio float64
	// ResolvedByRule splits ResolvedCalls by the linker rule that produced each
	// edge (pkg, chain, recvPkg, recvType, bare — see link.go), so a build
	// shows which declared evidence its edges rest on.
	ResolvedByRule map[string]int
	// SQLRefs counts SQL references edges resolved to a declared schema object;
	// DroppedSQLRefs those that named no single declaration (see sqllink.go).
	SQLRefs        int
	DroppedSQLRefs int
	// TypeRefs counts declared type-name references (Java fields, parameters,
	// locals); ResolvedTypeRefs those resolved to a class (see typelink.go).
	TypeRefs         int
	ResolvedTypeRefs int
}

// Build atomically swaps in a graph assembled from one or more repositories,
// running cross-file call resolution so calls edges point at qualified nodes
// instead of bare names. Lock-free readers see the old graph until the swap
// completes. It returns resolution stats.
func (g *Graph) Build(repos ...RepoGraph) LinkStats {
	ng := New()
	ls := LinkStats{ResolvedByRule: map[string]int{}}

	for _, r := range repos {
		for _, n := range r.Nodes {
			qn := repoQN(r.Repo, n.Qualified())
			if _, ok := ng.nodes[qn]; ok {
				continue // first declaration wins
			}
			ng.nodes[qn] = &Node{NodeInfo: n, Repo: r.Repo, QN: qn}
			ng.order = append(ng.order, qn)
		}

		// Cross-file linker: resolve calls edges against this repo's symbol table
		// (nodes for the symbols, edges for the imports that scope visibility).
		st := buildSymbolTable(r.Nodes, r.Edges, r.Repo)
		var sqlt *sqlTable
		for _, e := range r.Edges {
			src := repoQN(r.Repo, e.Source)
			tgt := repoQN(r.Repo, e.Target)

			if ref := e.Extra[sqlddl.ExtraRef]; ref != "" {
				if sqlt == nil {
					sqlt = buildSQLTable(r.Nodes, r.Repo)
				}
				var ok bool
				if from := e.Extra[sqlddl.ExtraFrom]; from != "" {
					src, ok = sqlt.resolve(from, sqlddl.RefRelation, e.File)
				} else {
					ok = true
				}
				if ok {
					tgt, ok = sqlt.resolve(ref, e.Extra[sqlddl.ExtraRefKind], e.File)
				}
				if !ok || src == tgt {
					ls.DroppedSQLRefs++
					continue
				}
				ls.SQLRefs++
			}

			if e.Kind == parser.EdgeReferences && e.Extra["typeName"] != "" {
				ls.TypeRefs++
				if resolved, ok := st.resolveTypeRef(e); ok {
					tgt = resolved
					ls.ResolvedTypeRefs++
				}
			}

			if e.Kind == "calls" {
				ls.CallEdges++
				if resolved, rule := st.resolveCallTarget(e); rule != "" {
					tgt = resolved
					ls.ResolvedCalls++
					ls.ResolvedByRule[rule]++
				} else {
					ls.ExternalCalls++
				}
			}

			ed := Edge{Kind: e.Kind, Source: src, Target: tgt, File: e.File, Line: e.Line, Extra: e.Extra}
			ng.out[src] = append(ng.out[src], ed)
			ng.in[tgt] = append(ng.in[tgt], ed)
		}
	}

	if ls.CallEdges > 0 {
		ls.ResolvedRatio = float64(ls.ResolvedCalls) / float64(ls.CallEdges)
	}

	g.mu.Lock()
	g.nodes, g.order, g.out, g.in = ng.nodes, ng.order, ng.out, ng.in
	g.mu.Unlock()
	return ls
}

// Get returns a node by qualified name.
func (g *Graph) Get(qn string) (*Node, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	n, ok := g.nodes[qn]
	return n, ok
}

// Nodes returns all nodes in stable order (callers must not mutate).
func (g *Graph) Nodes() []*Node {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]*Node, 0, len(g.order))
	for _, qn := range g.order {
		out = append(out, g.nodes[qn])
	}
	return out
}

// SetVec attaches an embedding to a node by qn.
func (g *Graph) SetVec(qn string, vec []float32) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if n, ok := g.nodes[qn]; ok {
		n.Vec = vec
	}
}

// Stats reports node/edge counts by kind.
type Stats struct {
	Nodes    int
	Edges    int
	ByKind   map[string]int
	ByEdge   map[string]int
	Embedded int
}

// DependencyEdges counts the edges impact and risk traverse (impactEdgeKinds).
// Zero means the map is structure-only — contains edges alone, as in a document
// collection — and a blast radius is not defined for it.
func (s Stats) DependencyEdges() int {
	n := 0
	for kind := range impactEdgeKinds {
		n += s.ByEdge[kind]
	}
	return n
}

func (g *Graph) Stats() Stats {
	g.mu.RLock()
	defer g.mu.RUnlock()
	s := Stats{ByKind: map[string]int{}, ByEdge: map[string]int{}}
	for _, qn := range g.order {
		n := g.nodes[qn]
		s.Nodes++
		s.ByKind[n.Kind]++
		if n.Vec != nil {
			s.Embedded++
		}
	}
	for _, edges := range g.out {
		for _, e := range edges {
			s.Edges++
			s.ByEdge[e.Kind]++
		}
	}
	return s
}

// AttrKeysByKind reports, per node kind, the attribute keys its nodes carry and
// how many carry each. Kinds whose nodes carry no attribute (every parsed code
// kind) are absent, so a code map's stats say nothing new. It is the vocabulary
// a caller needs BEFORE asking a catalog map to filter or order by an attribute:
// the academy map was ordered by "experience" when the key was "years", and
// nothing short of reading a node back with get_graph_node could have said so.
func (g *Graph) AttrKeysByKind() map[string]map[string]int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := map[string]map[string]int{}
	for _, qn := range g.order {
		n := g.nodes[qn]
		if len(n.Attrs) == 0 {
			continue
		}
		keys := out[n.Kind]
		if keys == nil {
			keys = map[string]int{}
			out[n.Kind] = keys
		}
		for k := range n.Attrs {
			keys[k]++
		}
	}
	return out
}

// GraphDump is the lean whole-graph export for visualization: node titles +
// edges, with no node bodies (Body/Content are omitted — the UI lazy-fetches a
// single node's full content via /node on click). At ~10^3 nodes this serializes
// to a few hundred KB, small enough for the UI to fetch once and lay out locally.
type GraphDump struct {
	Nodes []DumpNode `json:"nodes"`
	Edges []DumpEdge `json:"edges"`
}

// DumpNode is one node in a GraphDump — identity + placement, no body.
type DumpNode struct {
	QN    string `json:"qn"`
	Repo  string `json:"repo,omitempty"`
	Kind  string `json:"kind"`
	Title string `json:"title"`
	File  string `json:"file"`
	Line  int    `json:"line"`
	Lang  string `json:"lang,omitempty"`
}

// DumpEdge is one directed edge in a GraphDump. Both endpoints are guaranteed to
// be real nodes in the same dump (see Dump).
type DumpEdge struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Kind   string `json:"kind"`
}

// Dump exports the whole graph as titles + edges for a force-directed UI. It
// emits an edge only when BOTH endpoints are real nodes — the same reasoning as
// ImpactRadius skipping non-node call targets: unresolved bare symbols (e.g. a
// common "Get") aren't nodes, and rendering them as phantom hubs would merge
// unrelated call sites and clutter the layout. Read-only; takes the read lock.
// DisplayTitle is a node's name as every surface prints it: qualified by its
// parent when it has one. A child's bare Name is not unique — sibling sets
// repeat it under every parent — so an unqualified title renders distinct nodes
// as duplicates. Single definition of the rule: index.nodeTitle calls this.
func DisplayTitle(n *Node) string {
	if n.ParentName != "" {
		return n.ParentName + "." + n.Name
	}
	return n.Name
}

func (g *Graph) Dump() GraphDump {
	g.mu.RLock()
	defer g.mu.RUnlock()

	d := GraphDump{
		Nodes: make([]DumpNode, 0, len(g.order)),
		Edges: make([]DumpEdge, 0),
	}
	for _, qn := range g.order {
		n := g.nodes[qn]
		d.Nodes = append(d.Nodes, DumpNode{
			QN:    n.QN,
			Repo:  n.Repo,
			Kind:  n.Kind,
			Title: DisplayTitle(n),
			File:  n.FilePath,
			Line:  n.LineStart,
			Lang:  n.Language,
		})
	}
	for _, edges := range g.out {
		for _, e := range edges {
			if _, ok := g.nodes[e.Target]; !ok {
				continue // drop edges to unresolved (non-node) targets
			}
			d.Edges = append(d.Edges, DumpEdge{Source: e.Source, Target: e.Target, Kind: e.Kind})
		}
	}
	return d
}

// Callees returns outgoing edges of a node, optionally filtered by relation.
func (g *Graph) Callees(qn string, relation string) []Edge {
	return g.adjacent(g.out, qn, relation)
}

// Callers returns incoming edges of a node, optionally filtered by relation.
func (g *Graph) Callers(qn string, relation string) []Edge {
	return g.adjacent(g.in, qn, relation)
}

func (g *Graph) adjacent(m map[string][]Edge, qn, relation string) []Edge {
	g.mu.RLock()
	defer g.mu.RUnlock()
	var out []Edge
	for _, e := range m[qn] {
		if relation == "" || e.Kind == relation {
			out = append(out, e)
		}
	}
	return out
}

// ImpactNode is a node reached by an impact-radius BFS, with its distance.
type ImpactNode struct {
	QN    string
	Depth int
}

// impactEdgeKinds are the edge kinds ImpactRadius traverses — real dependency
// relationships only. "contains" (File -> its declared symbols) is deliberately
// excluded: it is structural/grouping metadata, not a dependency edge, and
// walking it turns any large file into a hub that fans out to every unrelated
// sibling symbol it declares, drowning the real call/import blast radius.
var impactEdgeKinds = map[string]bool{
	parser.EdgeCalls:      true,
	parser.EdgeImports:    true,
	parser.EdgeInherits:   true,
	parser.EdgeImplements: true,
	parser.EdgeReferences: true,
}

// ImpactRadius runs a bidirectional BFS from the given seed qualified names over
// dependency edge kinds (calls/imports/inherits/implements/references — see
// impactEdgeKinds), up to maxDepth, capped at maxNodes. This is the in-memory
// equivalent of code-review-graph's recursive-CTE blast-radius query.
func (g *Graph) ImpactRadius(seeds []string, maxDepth, maxNodes int) []ImpactNode {
	g.mu.RLock()
	defer g.mu.RUnlock()

	visited := map[string]int{}
	var frontier []string
	for _, s := range seeds {
		if _, seen := visited[s]; !seen {
			visited[s] = 0
			frontier = append(frontier, s)
		}
	}

	for depth := 1; depth <= maxDepth && len(frontier) > 0; depth++ {
		var next []string
		for _, qn := range frontier {
			neighbors := make([]string, 0)
			for _, e := range g.out[qn] {
				if !impactEdgeKinds[e.Kind] {
					continue
				}
				neighbors = append(neighbors, e.Target)
			}
			for _, e := range g.in[qn] {
				if !impactEdgeKinds[e.Kind] {
					continue
				}
				neighbors = append(neighbors, e.Source)
			}
			for _, nb := range neighbors {
				if _, seen := visited[nb]; seen {
					continue
				}
				// Only traverse through REAL graph nodes. Unresolved call targets
				// (bare common names like "Get"/"Store") are not nodes; treating
				// them as hubs would merge dozens of unrelated call sites and
				// blow up the blast radius. Skip them entirely.
				if _, isNode := g.nodes[nb]; !isNode {
					continue
				}
				visited[nb] = depth
				next = append(next, nb)
				if len(visited) >= maxNodes {
					return collectImpact(visited)
				}
			}
		}
		frontier = next
	}
	return collectImpact(visited)
}

// RiskReport buckets a symbol's change risk into a coarse LOW/MEDIUM/HIGH
// signal, derived from its direct caller count and how many distinct modules
// (directories) its blast radius reaches. It is a heuristic, not a precise
// cost model — it exists so a caller gets a signal instead of having to
// eyeball a raw impact list themselves.
type RiskReport struct {
	QN            string
	Level         string // LOW, MEDIUM, HIGH
	DirectCallers int
	ModuleSpread  int
	ImpactedCount int
	Modules       []string
}

// riskModule returns the directory a qualified name's file belongs to, used
// as the "module" unit for risk's spread metric: repo//dir/file.go::Symbol ->
// dir. File/Doc nodes (qn == their own path) resolve the same way.
func riskModule(qn string) string {
	file, _, _ := strings.Cut(qn, "::")
	if _, rest, ok := strings.Cut(file, "//"); ok {
		file = rest
	}
	return path.Dir(file)
}

// Risk scores the change risk of one symbol. Thresholds are deliberately
// simple (caller count + module spread, not a weighted model) — the goal is a
// quick triage signal, not a precise estimate.
func (g *Graph) Risk(qn string, maxDepth, maxNodes int) RiskReport {
	direct := g.Callers(qn, parser.EdgeCalls)

	impacted := g.ImpactRadius([]string{qn}, maxDepth, maxNodes)
	modSet := map[string]bool{}
	for _, im := range impacted {
		if im.QN == qn {
			continue
		}
		modSet[riskModule(im.QN)] = true
	}
	modules := make([]string, 0, len(modSet))
	for m := range modSet {
		modules = append(modules, m)
	}
	sort.Strings(modules)

	level := "LOW"
	switch {
	case len(direct) > 10 || len(modules) > 5:
		level = "HIGH"
	case len(direct) > 2 || len(modules) > 2:
		level = "MEDIUM"
	}

	return RiskReport{
		QN: qn, Level: level, DirectCallers: len(direct),
		ModuleSpread: len(modules), ImpactedCount: len(impacted), Modules: modules,
	}
}

func collectImpact(visited map[string]int) []ImpactNode {
	out := make([]ImpactNode, 0, len(visited))
	for qn, d := range visited {
		out = append(out, ImpactNode{QN: qn, Depth: d})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Depth != out[j].Depth {
			return out[i].Depth < out[j].Depth
		}
		return out[i].QN < out[j].QN
	})
	return out
}

// NodesForFile returns all nodes declared in the given file path.
func (g *Graph) NodesForFile(file string) []*Node {
	g.mu.RLock()
	defer g.mu.RUnlock()
	var out []*Node
	for _, qn := range g.order {
		if g.nodes[qn].FilePath == file {
			out = append(out, g.nodes[qn])
		}
	}
	return out
}
