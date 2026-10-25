package xray

import (
	"context"
	"encoding/json"
)

// Query is the resolved form of a list of QueryOption, for an implementation of
// Memory that answers in process.
type Query struct {
	Subject string
	Kind    string
	Limit   int
}

// ResolveQuery applies opts and returns what they asked for.
func ResolveQuery(opts ...QueryOption) Query {
	q := buildQuery(opts)
	return Query{Subject: q.subject, Kind: q.kind, Limit: q.limit}
}

// GraphQuery is the resolved form of a list of GraphOption.
type GraphQuery struct {
	Map    string
	Kind   string
	Limit  int
	Attr   string
	Order  string
	Cutoff bool
}

// ResolveGraph applies opts and returns what they asked for.
func ResolveGraph(opts ...GraphOption) GraphQuery {
	g := buildGraph(opts)
	return GraphQuery{Map: g.mapName, Kind: g.kind, Limit: g.limit, Attr: g.attr, Order: g.order, Cutoff: g.cutoff}
}

// Memory is the authored-memory API. *Client implements it against a running
// X-Ray, and memory.Store implements it in the caller's own process, so an
// agent written against Memory runs on either.
type Memory interface {
	Search(ctx context.Context, query string, opts ...QueryOption) ([]Item, error)
	List(ctx context.Context, opts ...QueryOption) ([]Item, error)
	Fetch(ctx context.Context, ids []string, opts ...QueryOption) ([]Item, error)
	Get(ctx context.Context, id string, opts ...QueryOption) (*Item, error)
	Summary(ctx context.Context) (*Summary, error)
	Recent(ctx context.Context, opts ...QueryOption) ([]Episode, error)
	Upsert(ctx context.Context, w Write) (*WriteResult, error)
	Delete(ctx context.Context, id string, opts ...QueryOption) error
	Refute(ctx context.Context, id, reason string, evidence []string, opts ...QueryOption) (*RefuteResult, error)
	Expire(ctx context.Context, id string, opts ...QueryOption) error
	Supersede(ctx context.Context, oldID string, w Write) (*SupersedeResult, error)
	Consolidate(ctx context.Context, apply bool, opts ...QueryOption) (*ConsolidationReport, error)
	Context(ctx context.Context, anchor string, maxDepth int) ([]ContextItem, error)
}

// CodeGraph is the code-graph API. *Client implements it against a running
// graph server, and codegraph.Graph implements it in the caller's own process.
type CodeGraph interface {
	SearchGraph(ctx context.Context, query string, opts ...GraphOption) (*SearchResult, error)
	SearchGraphDocs(ctx context.Context, query string, opts ...GraphOption) (*SearchResult, error)
	Node(ctx context.Context, qn string) (*Node, error)
	Impact(ctx context.Context, seeds []string, depth, maxNodes int) (*Impact, error)
	Risk(ctx context.Context, qn string, depth, maxNodes int) (*Risk, error)
	GraphStats(ctx context.Context, opts ...GraphOption) (json.RawMessage, error)
	UpdateGraph(ctx context.Context, paths ...string) (*UpdateResult, error)
}

var (
	_ Memory    = (*Client)(nil)
	_ CodeGraph = (*Client)(nil)
)
