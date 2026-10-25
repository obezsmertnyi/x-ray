package xray

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// GraphOption narrows a graph call.
type GraphOption func(*graphOptions)

type graphOptions struct {
	mapName string
	kind    string
	limit   int
	attr    string
	order   string
	cutoff  bool
}

// OnMap scopes a call to one map. Without it a search is routed by meaning to
// the best-matching map.
func OnMap(name string) GraphOption { return func(o *graphOptions) { o.mapName = name } }

// OfKind keeps only nodes of one kind, for example "Func", "Class" or "Doc".
func OfKind(kind string) GraphOption { return func(o *graphOptions) { o.kind = kind } }

// Limit caps the number of hits (default 5, at most 20 on the server).
func Limit(n int) GraphOption { return func(o *graphOptions) { o.limit = n } }

// Attr keeps nodes whose attribute matches, written "key=value".
func Attr(kv string) GraphOption { return func(o *graphOptions) { o.attr = kv } }

// OrderBy sorts by an attribute, written "key" or "key:desc".
func OrderBy(order string) GraphOption { return func(o *graphOptions) { o.order = order } }

// Cutoff drops the ranked tail at its steepest score drop.
func Cutoff() GraphOption { return func(o *graphOptions) { o.cutoff = true } }

func buildGraph(opts []GraphOption) graphOptions {
	var g graphOptions
	for _, o := range opts {
		o(&g)
	}
	return g
}

func (g graphOptions) args(a map[string]any) map[string]any {
	// A live server names the repository "repo"; a snapshot server "map" (and
	// accepts "repo" as an alias). Both are sent so either answers.
	put(a, "map", g.mapName)
	put(a, "repo", g.mapName)
	put(a, "kind", g.kind)
	put(a, "limit", g.limit)
	put(a, "attr", g.attr)
	put(a, "order", g.order)
	if g.cutoff {
		a["cutoff"] = true
	}
	return a
}

// Hit is one graph search result: a lean pointer to a node.
type Hit struct {
	// QN is the qualified name, repo//path::Symbol. Pass it to Node or Impact.
	QN      string  `json:"qn"`
	Kind    string  `json:"kind"`
	Title   string  `json:"title"`
	File    string  `json:"file"`
	Line    int     `json:"line"`
	Score   float64 `json:"score"`
	Preview string  `json:"preview"`
	Repo    string  `json:"repo,omitempty"`
	// Parent is the containing node of a child hit.
	Parent string `json:"parent,omitempty"`
}

// SearchResult is the reply to SearchGraph and SearchGraphDocs.
type SearchResult struct {
	Query   string `json:"query"`
	Count   int    `json:"count"`
	Results []Hit  `json:"results"`
	// Note explains an empty or unusual result, such as an unknown attribute.
	Note string `json:"note,omitempty"`
}

// SearchGraph finds code and manifests by meaning.
func (c *Client) SearchGraph(ctx context.Context, query string, opts ...GraphOption) (*SearchResult, error) {
	return c.searchGraph(ctx, "search_graph", query, opts)
}

// SearchGraphDocs finds documentation prose by meaning: ADRs, READMEs, todos.
func (c *Client) SearchGraphDocs(ctx context.Context, query string, opts ...GraphOption) (*SearchResult, error) {
	return c.searchGraph(ctx, "search_graph_docs", query, opts)
}

func (c *Client) searchGraph(ctx context.Context, tool, query string, opts []GraphOption) (*SearchResult, error) {
	var out SearchResult
	if err := c.call(ctx, tool, buildGraph(opts).args(map[string]any{"query": query}), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Node is the full body of one graph node and its relations.
type Node struct {
	QN        string            `json:"qn"`
	Repo      string            `json:"repo"`
	Kind      string            `json:"kind"`
	Title     string            `json:"title"`
	File      string            `json:"file"`
	LineStart int               `json:"line_start"`
	LineEnd   int               `json:"line_end"`
	Language  string            `json:"language"`
	Content   string            `json:"content"`
	Attrs     map[string]string `json:"attrs,omitempty"`
	// Calls, CalledBy and Imports are qualified names, in both directions.
	Calls    []string `json:"calls,omitempty"`
	CalledBy []string `json:"called_by,omitempty"`
	Imports  []string `json:"imports,omitempty"`
}

// Node returns the full content of the node qn and its relations.
func (c *Client) Node(ctx context.Context, qn string) (*Node, error) {
	var out Node
	if err := c.call(ctx, "get_graph_node", map[string]any{"qn": qn}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ImpactNode is a node a change reaches, and how many hops away it is.
type ImpactNode struct {
	QN    string `json:"QN"`
	Depth int    `json:"Depth"`
}

// Impact is the blast radius of changing some symbols.
type Impact struct {
	Seeds    []string     `json:"seeds"`
	Depth    int          `json:"depth"`
	Count    int          `json:"count"`
	Impacted []ImpactNode `json:"impacted"`
}

// Impact returns what a change to the seed symbols reaches over calls and
// imports. depth and maxNodes of 0 use the server defaults (2 and 200).
func (c *Client) Impact(ctx context.Context, seeds []string, depth, maxNodes int) (*Impact, error) {
	args := map[string]any{"seeds": seeds}
	put(args, "depth", depth)
	put(args, "max_nodes", maxNodes)
	var out Impact
	if err := c.call(ctx, "get_graph_impact", args, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Risk is a coarse change-risk signal for one symbol.
type Risk struct {
	QN string `json:"QN"`
	// Level is LOW, MEDIUM or HIGH.
	Level         string   `json:"Level"`
	DirectCallers int      `json:"DirectCallers"`
	ModuleSpread  int      `json:"ModuleSpread"`
	ImpactedCount int      `json:"ImpactedCount"`
	Modules       []string `json:"Modules"`
}

// Risk scores the change risk of the symbol qn.
func (c *Client) Risk(ctx context.Context, qn string, depth, maxNodes int) (*Risk, error) {
	args := map[string]any{"qn": qn}
	put(args, "depth", depth)
	put(args, "max_nodes", maxNodes)
	var out Risk
	if err := c.call(ctx, "get_graph_risk", args, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GraphStats returns node and edge counts, languages and the router summary of
// a map, as the server reports them.
func (c *Client) GraphStats(ctx context.Context, opts ...GraphOption) (json.RawMessage, error) {
	g := buildGraph(opts)
	args := map[string]any{}
	put(args, "map", g.mapName)
	put(args, "repo", g.mapName)
	return c.Call(ctx, "get_graph_stats", args)
}

// UpdateResult is the outcome of UpdateGraph.
type UpdateResult struct {
	Changed int `json:"changed"`
	Removed int `json:"removed"`
	Skipped int `json:"skipped,omitempty"`
	// Unowned lists the paths that belong to no served repository; they are
	// never read.
	Unowned []string `json:"unowned,omitempty"`
}

// UpdateGraph re-indexes changed files in the running server. The server refuses
// it when its snapshot directory is read-only.
func (c *Client) UpdateGraph(ctx context.Context, paths ...string) (*UpdateResult, error) {
	var out UpdateResult
	if err := c.call(ctx, "update_code_graph", map[string]any{"paths": paths}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Skills lists the operating procedures a map carries as documents.
func (c *Client) Skills(ctx context.Context) (json.RawMessage, error) {
	return c.Call(ctx, "list_skills", nil)
}

// Skill returns one operating procedure by name.
func (c *Client) Skill(ctx context.Context, name string) (json.RawMessage, error) {
	return c.Call(ctx, "get_skill", map[string]any{"name": name})
}

// Note is an agent note, as Recall returns it.
type Note struct {
	QN      string            `json:"qn"`
	At      string            `json:"at"`
	Text    string            `json:"text"`
	Title   string            `json:"title,omitempty"`
	Expires string            `json:"expires,omitempty"`
	Attrs   map[string]string `json:"attrs,omitempty"`
}

// RememberOption sets optional fields of a note.
type RememberOption func(*rememberOptions)

type rememberOptions struct {
	title string
	attrs map[string]string
	ttl   string
}

// NoteTitle names the note; it becomes part of its qualified name.
func NoteTitle(title string) RememberOption { return func(o *rememberOptions) { o.title = title } }

// NoteAttrs adds facets, for example about=<name>, that Recall can find it by.
func NoteAttrs(attrs map[string]string) RememberOption {
	return func(o *rememberOptions) { o.attrs = attrs }
}

// NoteTTL sets the lifetime: a duration such as 7d, 24h or 90m, or "never".
func NoteTTL(ttl string) RememberOption { return func(o *rememberOptions) { o.ttl = ttl } }

// Remembered is the outcome of Remember.
type Remembered struct {
	Map   string `json:"map"`
	QN    string `json:"qn"`
	At    string `json:"at"`
	Notes int    `json:"notes"`
}

// Remember writes a short note into the server's notes map, so a later session
// can read it back. The server must run with -writable.
func (c *Client) Remember(ctx context.Context, text string, opts ...RememberOption) (*Remembered, error) {
	var o rememberOptions
	for _, f := range opts {
		f(&o)
	}
	args := map[string]any{"text": text}
	put(args, "title", o.title)
	put(args, "ttl", o.ttl)
	if len(o.attrs) > 0 {
		pairs := make([]string, 0, len(o.attrs))
		for k, v := range o.attrs {
			pairs = append(pairs, fmt.Sprintf("%s=%s", k, v))
		}
		args["attrs"] = strings.Join(pairs, ",")
	}
	var out Remembered
	if err := c.call(ctx, "remember", args, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Recalled is the reply to Recall.
type Recalled struct {
	Map   string `json:"map"`
	Count int    `json:"count"`
	Notes []Note `json:"notes"`
	// Note says why the reply is empty or how it is ordered. Notes are what
	// people said, not the record of a fact.
	Note string `json:"note"`
}

// Recall reads notes back: newest first, or ranked by query when one is given.
func (c *Client) Recall(ctx context.Context, query string, limit int) (*Recalled, error) {
	args := map[string]any{}
	put(args, "query", query)
	put(args, "limit", limit)
	var out Recalled
	if err := c.call(ctx, "recall", args, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Forgotten is the outcome of Forget.
type Forgotten struct {
	Map   string `json:"map"`
	QN    string `json:"qn"`
	Notes int    `json:"notes"`
}

// Forget removes one note by the qualified name Remember returned. The server
// must run with -writable.
func (c *Client) Forget(ctx context.Context, qn string) (*Forgotten, error) {
	var out Forgotten
	if err := c.call(ctx, "forget", map[string]any{"qn": qn}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
