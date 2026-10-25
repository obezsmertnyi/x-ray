package mcpserver

// The recall tool: an agent reads its own notes back.
//
// Notes were readable before this through search_graph map=<notes> kind=Memory
// order=at:desc — a recipe, spelled out in remember's reply and description.
// The voice agent still did not use it: asked "do you remember the Octocat
// notes", it searched the academy catalog and reported nothing, because a
// recipe buried in another tool's argument list is not a tool it knows it
// has. recall is that recipe as a first-class tool with the description doing
// the routing: any question about a previous conversation starts here.
//
// It reads only Deps.MemoryMap and needs no Rememberer: a read-only snapshot
// server that ships a notes map must still be able to recall from it, so the
// tool registers whenever a notes map is named, on both surfaces.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/graph"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/index"
)

// recallDesc is the lever: a model that reads it should reach for recall
// before any code or catalog map when the question is about what was said.
const recallDesc = "Your own notes from earlier conversations with this user: what they told you, asked, decided, and where each call ended. Call this FIRST for any question about a previous conversation — 'what did we talk about', 'do you remember', 'last time', 'what did I tell you about X' — before searching any code or catalog map; those maps hold code and records, not what was said. For a question about a named person, service or thing, do the reverse: search its map first, then recall with the name to add what was said since — notes written with about=<name> come back first, and a note supplements the record, it never replaces it. With no query it returns the newest notes; with a query it ranks notes by it. Empty means nothing was remembered, not that it does not exist elsewhere."

// recallDefaultLimit and recallMaxLimit bound a reply: a note is short, but
// twenty of them read aloud is a wall, and five is a spoken answer.
const (
	recallDefaultLimit = 5
	recallMaxLimit     = 20
)

// recallAttrs are the attributes recall lifts into their own fields; the
// rest are the caller's facets and come back as attrs.
var recallAttrs = map[string]bool{"at": true, "title": true, "expires": true}

// aboutAttr is the facet that names the map record a note is ABOUT — a
// student's handle, a service name. It stays a plain attr (the notes map's
// layout does not change), but recall reads it: see aboutFirst.
const aboutAttr = "about"

func registerRecall(s *server.MCPServer, d Deps) {
	if d.MemoryMap == "" || (d.Resolver == nil && (d.Graph == nil || d.Index == nil)) {
		return
	}
	s.AddTool(mcp.NewTool("recall",
		readOnlyTool(),
		mcp.WithDescription(recallDesc),
		mcp.WithString("query", mcp.Description("Optional: what to look for among the notes, in plain words. Notes are ranked by it. Omit for the newest notes first.")),
		mcp.WithNumber("limit", mcp.Description(fmt.Sprintf("Max notes to return (default %d, max %d)", recallDefaultLimit, recallMaxLimit))),
	), logged(d, "recall", func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		limit := 0
		if l, ok := args["limit"].(float64); ok {
			limit = int(l)
		}
		out, err := recall(ctx, d, argStr(args, "query"), limit)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return jsonResult(out)
	}))
}

// recall is the shared body of the MCP tool and RecallDirect.
func recall(ctx context.Context, d Deps, query string, limit int) (map[string]any, error) {
	if d.MemoryMap == "" {
		return nil, fmt.Errorf("memory is not enabled on this server")
	}
	if limit <= 0 {
		limit = recallDefaultLimit
	}
	if limit > recallMaxLimit {
		limit = recallMaxLimit
	}
	g, ix, repo, err := notesSource(ctx, d)
	if err != nil {
		// A notes map that does not exist yet is "nothing remembered", not a
		// failure: it is created by the first remember.
		return recallResult(d.MemoryMap, query, nil, fmt.Sprintf("no notes map %q on this server yet (%v); the first remember creates it", d.MemoryMap, err)), nil
	}
	now := time.Now()
	var notes []*graph.Node
	if query != "" {
		// Ranked: the selection path scores every Memory node by the query
		// (cosine within the named set, never the similarity channels that
		// would drop a member the query did not happen to retrieve).
		sel := index.Select{Kinds: map[string]bool{memoryKind: true}}
		hits, err := ix.Search(ctx, query, nil, repo, sel, limit+recallMaxLimit) // over-fetch: expired hits are dropped below
		if err != nil {
			return nil, err
		}
		for _, h := range hits {
			if n, ok := g.Get(h.QN); ok && !noteExpired(n, now) {
				notes = append(notes, n)
			}
		}
		notes = aboutFirst(g, repo, query, notes, now)
	} else {
		// Newest first, from the graph directly: no query means no embedding
		// call, and "at" is fixed-width so a string sort is a time sort.
		for _, n := range g.Nodes() {
			if n.Kind == memoryKind && (repo == "" || n.Repo == repo) && !noteExpired(n, now) {
				notes = append(notes, n)
			}
		}
		sort.SliceStable(notes, func(i, j int) bool { return notes[i].Attrs["at"] > notes[j].Attrs["at"] })
	}
	if len(notes) > limit {
		notes = notes[:limit]
	}
	return recallResult(d.MemoryMap, query, notes, ""), nil
}

// aboutFirst puts the notes whose about attr names the query at the head of a
// ranked reply, when the query is a single token — a handle or a name, the way
// an agent asks after "search the map for octocat42". The selection path
// ranks notes by cosine to the note TEXT, and a note saying "add GitOps to the
// course expectations" carries no trace of the handle it is about, so the one
// note written for that student could land anywhere in the list or past the
// over-fetch entirely. Every live match is therefore gathered from the graph
// (not only from the hits) and hoisted; cosine order is kept inside each half.
// Matching is case-insensitive and ignores a leading '@' on either side, since
// a handle is typed both ways. A multi-word query is a description, not a
// name, and is left in rank order.
func aboutFirst(g *graph.Graph, repo, query string, ranked []*graph.Node, now time.Time) []*graph.Node {
	want := aboutKey(query)
	if want == "" || len(strings.Fields(query)) != 1 {
		return ranked
	}
	seen := make(map[string]bool, len(ranked))
	var about, rest []*graph.Node
	for _, n := range ranked {
		seen[n.QN] = true
		if aboutKey(n.Attrs[aboutAttr]) == want {
			about = append(about, n)
		} else {
			rest = append(rest, n)
		}
	}
	// Matches the over-fetch missed, in a stable order so the reply does not
	// reshuffle between calls.
	var missed []*graph.Node
	for _, n := range g.Nodes() {
		if seen[n.QN] || n.Kind != memoryKind || (repo != "" && n.Repo != repo) || noteExpired(n, now) {
			continue
		}
		if aboutKey(n.Attrs[aboutAttr]) == want {
			missed = append(missed, n)
		}
	}
	sort.SliceStable(missed, func(i, j int) bool { return missed[i].Attrs["at"] > missed[j].Attrs["at"] })
	about = append(about, missed...)
	if len(about) == 0 {
		return ranked
	}
	return append(about, rest...)
}

// aboutKey normalizes a handle for comparison: trimmed, lower-cased, without
// a leading '@'.
func aboutKey(s string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), "@"))
}

// notesSource finds the notes map's graph and index: the resolver's map by
// exact label in snapshot mode (never a semantic fallback to another map —
// that would answer "what did we discuss" out of a code graph), or the fixed
// live graph filtered to the repo label the notes carry.
func notesSource(ctx context.Context, d Deps) (*graph.Graph, *index.Index, string, error) {
	if d.Resolver != nil {
		resolved, err := d.Resolver.Resolve(ctx, d.MemoryMap, "")
		if err != nil {
			return nil, nil, "", err
		}
		if len(resolved) == 0 || resolved[0].Label != d.MemoryMap {
			return nil, nil, "", fmt.Errorf("map %q is not registered", d.MemoryMap)
		}
		return resolved[0].Graph, resolved[0].Index, "", nil
	}
	if d.Graph == nil || d.Index == nil {
		return nil, nil, "", fmt.Errorf("no graph")
	}
	return d.Graph, d.Index, d.MemoryMap, nil
}

// noteExpired mirrors the engine's rule so a note past its expiry that the
// sweep has not yet removed (a read-only server never sweeps) is not read
// back. Missing or unreadable means never.
func noteExpired(n *graph.Node, now time.Time) bool {
	v := n.Attrs["expires"]
	if v == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return false
	}
	return !t.After(now)
}
