package engine

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/graph"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/index"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/parser"
)

// repoState holds one repository's absolute path and its parsed-per-file results,
// so an incremental update can replace a single file's nodes/edges and rebuild.
type repoState struct {
	label string
	path  string                   // absolute repo root
	files map[string]parser.Result // rel path -> parsed result
}

// Indexer owns the live graph + index and the per-repo parsed state needed to
// update incrementally. The graph/index are swapped atomically on each rebuild
// so MCP readers always see a consistent snapshot.
//
// Two construction paths exist: NewIndexer (live parse, CGO — see indexer_cgo.go)
// and NewIndexerFromSnapshot (parse-free load from a pre-built snapshot, pure Go).
// The CGO-free static build uses only the snapshot path.
type Indexer struct {
	mu     sync.Mutex
	repos  []*repoState
	g      *graph.Graph
	ix     *index.Index
	logger zerolog.Logger

	// followSymlinks is the walk option NewIndexer was built with — carried on
	// the Indexer (not just a NewIndexer-local var) so a later Reindex walks
	// with the same setting. Live-parse only; the snapshot-load path has no
	// walk at all.
	followSymlinks bool

	// meta is the opaque per-node attribute bag (QN → string k/v) carried verbatim
	// through the snapshot. recall never interprets it — a builder sets it, a reader
	// consumes it. Round-trips: SnapshotFor writes it into Body.Meta,
	// NewIndexerFromSnapshot reads it back.
	meta map[string]map[string]string

	// explicitSummary is an operator-written router description (the snapshot
	// subcommand's -summary flag) that SnapshotFor prefers outright over the
	// README/stats fallback — see summaryFor. Empty by default (no override).
	// Set via SetExplicitSummary before calling SnapshotFor, same idiom as meta.
	explicitSummary string

	lastBuilt time.Time
}

// Graph and Index expose the live (swappable) instances for the MCP server.
func (ixr *Indexer) Graph() *graph.Graph { return ixr.g }
func (ixr *Indexer) Index() *index.Index { return ixr.ix }

// Labels returns the served repo labels. Used by serve mode to snapshot each
// repo's graph to disk (periodic + on shutdown).
func (ixr *Indexer) Labels() []string {
	ixr.mu.Lock()
	defer ixr.mu.Unlock()
	out := make([]string, 0, len(ixr.repos))
	for _, rs := range ixr.repos {
		out = append(out, rs.label)
	}
	return out
}

// NewIndexerFromSnapshot reconstructs an indexer from a pre-built snapshot
// WITHOUT calling the embedder for any node: the snapshot's text→vec
// cache is loaded first, so Build is all cache-hits and only (re)builds the HNSW
// index. The embedder is retained solely for embedding queries at search time.
//
// A snapshot built with `snapshot -no-embed` (snap.Header.NoEmbed) carries an
// empty cache on purpose: this marks the loaded Index no-embed too
// (index.Index.SetNoEmbed), so Build leaves every node unembedded instead of
// treating the empty cache as ordinary cache misses and embedding the whole
// repo on load — the read-side half of never re-embedding a no-embed map.
// Search over the result degrades to its lexical channel; other tools (graph
// walks, node lookup) are unaffected.
//
// This is the load path of the read-only `serve-snapshots` server (Mode B); the
// resulting indexer is treated as read-only (Update/Reindex are never called).
// It performs NO parsing, so it compiles and runs without CGO.
func NewIndexerFromSnapshot(ctx context.Context, snap *Snapshot, embedder index.Embedder, logger zerolog.Logger) (*Indexer, error) {
	ixr := &Indexer{logger: logger, meta: snap.Body.Meta}
	rs := &repoState{
		label: snap.Header.Label,
		path:  snap.Header.RepoPath,
		files: snap.Body.Files,
	}
	if rs.files == nil {
		rs.files = map[string]parser.Result{}
	}
	ixr.repos = []*repoState{rs}

	ixr.g = graph.New()
	ixr.buildGraphLocked()

	ixr.ix = index.New(ixr.g, embedder, logger)
	if snap.Header.NoEmbed {
		ixr.ix.SetNoEmbed(true)
	}
	ixr.ix.LoadCache(snap.Body.Cache)     // every node text is now a cache hit (empty for a no-embed snapshot)
	ixr.ix.SetPendingHNSW(snap.Body.HNSW) // deserialize the ANN instead of rebuilding (if present)
	if err := ixr.ix.Build(ctx); err != nil {
		return nil, fmt.Errorf("build index from snapshot: %w", err)
	}
	ixr.lastBuilt = time.Now()
	return ixr, nil
}

// NewIndexerFromResults builds an indexer from pre-constructed per-"file"
// parser.Results (nodes + edges) rather than by parsing a repo on disk, then
// runs the embed pass so every node gets a vector and the HNSW is built. This is
// the entry point for NON-code mental maps — e.g. a service dependency graph
// (services as nodes, calls as edges) built from a JSON file — which reuse the
// exact same graph/index/snapshot pipeline as code maps without any parsing (so
// it compiles and runs CGO-free). label is the map name; repoPath is a
// descriptive origin recorded in the snapshot header (e.g. the source JSON path).
func NewIndexerFromResults(ctx context.Context, label, repoPath string, files map[string]parser.Result, embedder index.Embedder, logger zerolog.Logger) (*Indexer, error) {
	ixr := &Indexer{logger: logger}
	if files == nil {
		files = map[string]parser.Result{}
	}
	ixr.repos = []*repoState{{label: label, path: repoPath, files: files}}

	ixr.g = graph.New()
	ixr.buildGraphLocked()

	ixr.ix = index.New(ixr.g, embedder, logger)
	if err := ixr.ix.Build(ctx); err != nil { // embed pass: embeds every node, builds HNSW
		return nil, fmt.Errorf("build index from results: %w", err)
	}
	ixr.lastBuilt = time.Now()
	return ixr, nil
}

// SetMeta attaches the opaque per-node attribute bag (QN → string k/v) that
// SnapshotFor will write verbatim into Body.Meta. A builder calls this before
// snapshotting; recall assigns the map no meaning. Nil clears it.
func (ixr *Indexer) SetMeta(meta map[string]map[string]string) {
	ixr.mu.Lock()
	defer ixr.mu.Unlock()
	ixr.meta = meta
}

// Meta returns the opaque per-node attribute bag carried by the snapshot (QN →
// string k/v), or nil if none. recall never interprets it — a consumer (e.g.
// triage) reads its own keys. The returned map must not be mutated.
func (ixr *Indexer) Meta() map[string]map[string]string {
	ixr.mu.Lock()
	defer ixr.mu.Unlock()
	return ixr.meta
}

// SetExplicitSummary attaches an operator-written router description that
// SnapshotFor will use verbatim (see summaryFor) instead of deriving one from
// the README or graph stats. A builder calls this before snapshotting, e.g.
// from the snapshot subcommand's -summary flag. Empty clears any override.
func (ixr *Indexer) SetExplicitSummary(s string) {
	ixr.mu.Lock()
	defer ixr.mu.Unlock()
	ixr.explicitSummary = s
}

// SnapshotFor captures the current single-repo graph as a portable Snapshot:
// the parsed per-file Results, the embedding cache (verbatim), and a synthesized
// router summary embedded via the embedder. The indexer must hold exactly one
// repo (the snapshot subcommand builds one project at a time).
func (ixr *Indexer) SnapshotFor(ctx context.Context, label string, fp EmbedFingerprint, embedder index.Embedder) (*Snapshot, error) {
	ixr.mu.Lock()
	defer ixr.mu.Unlock()
	if len(ixr.repos) != 1 {
		return nil, fmt.Errorf("SnapshotFor expects exactly one repo, have %d", len(ixr.repos))
	}
	rs := ixr.repos[0]

	files := make(map[string]parser.Result, len(rs.files))
	maps.Copy(files, rs.files)

	summary, summarySource := summaryFor(ixr.g, label, ixr.explicitSummary)
	noEmbed := ixr.ix.NoEmbed()
	// The routing vector is itself an embed call — a -no-embed build must
	// never make it either, or it would be the one embedder call an otherwise
	// embed-free build still made (and the one that fails when the embedder
	// is down, which -no-embed exists to tolerate). SummaryVec stays empty;
	// Registry.rankBySummary already skips entries with no summary vector, so
	// the map is only reachable by its exact label until it is rebuilt without
	// the flag.
	var summaryVec []float32
	if !noEmbed {
		var err error
		summaryVec, err = embedder.EmbedQuery(ctx, summary)
		if err != nil {
			return nil, fmt.Errorf("embed summary: %w", err)
		}
	}

	st := ixr.g.Stats()
	return &Snapshot{
		Header: SnapshotHeader{
			Version:       snapshotVersion,
			Label:         label,
			RepoPath:      rs.path,
			Fingerprint:   fp,
			BuiltAt:       time.Now(),
			Nodes:         st.Nodes,
			Edges:         st.Edges,
			Summary:       summary,
			SummaryVec:    summaryVec,
			SummarySource: summarySource,
			Facts:         parser.FactsVersion,
			NoEmbed:       noEmbed,
		},
		Body: SnapshotBody{
			Files: files,
			Cache: ixr.ix.CacheSnapshot(),
			HNSW:  ixr.ix.DumpHNSW(), // serialized ANN; load skips the rebuild
			Meta:  ixr.meta,          // opaque attribute bag, round-tripped verbatim
		},
	}, nil
}

// buildGraphLocked rebuilds ixr.g from all repos' current parsed files. Caller
// holds ixr.mu. Cross-file call resolution runs per repo, so re-parsing one file
// correctly re-links dependents that call into it.
func (ixr *Indexer) buildGraphLocked() graph.LinkStats {
	rgs := make([]graph.RepoGraph, 0, len(ixr.repos))
	for _, rs := range ixr.repos {
		var nodes []parser.NodeInfo
		var edges []parser.EdgeInfo
		for _, res := range rs.files {
			nodes = append(nodes, res.Nodes...)
			edges = append(edges, res.Edges...)
		}
		rgs = append(rgs, graph.RepoGraph{Repo: rs.label, Nodes: nodes, Edges: edges})
	}
	return ixr.g.Build(rgs...)
}

// Status reports current graph stats for the status CLI / endpoint.
func (ixr *Indexer) Status() map[string]any {
	ixr.mu.Lock()
	defer ixr.mu.Unlock()
	st := ixr.g.Stats()
	repos := make([]string, 0, len(ixr.repos))
	for _, rs := range ixr.repos {
		repos = append(repos, rs.label)
	}
	sort.Strings(repos)
	return map[string]any{
		"nodes":     st.Nodes,
		"edges":     st.Edges,
		"embedded":  st.Embedded,
		"byKind":    st.ByKind,
		"byEdge":    st.ByEdge,
		"repos":     repos,
		"lastBuilt": ixr.lastBuilt.Format(time.RFC3339),
	}
}

// locate maps an input path (absolute or relative) to its owning repo + the
// repo-relative path. Returns (nil, "") if it isn't under any served repo.
// An ABSOLUTE path is matched by containment. A RELATIVE path is resolved
// against each served repo root — deliberately NOT against the process working
// directory, which is where this used to go wrong: a server started by launchd
// has CWD "/", so every relative path resolved under no repo and was dropped
// while the caller was told "changed: 0" — indistinguishable from "nothing to
// do". A relative path must name a file that exists under exactly one served
// repo; matching none or several returns no repo, so the caller can be told
// rather than have a guess made on its behalf. Recording a DELETION therefore
// needs an absolute path: the file is gone, so nothing resolves it by name.
//
// A path that leaves the repo — lexically ("../x", "a/../../x") or through a
// symlinked directory the walk would not enter — owns no repo either. The
// relative branch once returned filepath.Clean(raw) with its ".." intact, so
// an unauthenticated update_code_graph or POST /update of "../outside/x.md"
// indexed a file outside every served tree and get_graph_node read it back.
func (ixr *Indexer) locate(raw string) (*repoState, string) {
	if filepath.IsAbs(raw) {
		for _, rs := range ixr.repos {
			if rel, err := filepath.Rel(rs.path, raw); err == nil && filepath.IsLocal(rel) {
				if !ixr.withinRoot(rs, rel) {
					return nil, ""
				}
				return rs, rel
			}
		}
		return nil, ""
	}
	if !filepath.IsLocal(raw) {
		return nil, ""
	}
	var match *repoState
	for _, rs := range ixr.repos {
		if !ixr.withinRoot(rs, raw) {
			continue
		}
		if _, err := os.Stat(filepath.Join(rs.path, raw)); err == nil {
			if match != nil {
				return nil, "" // same relative path under several repos: refuse to guess
			}
			match = rs
		}
	}
	if match == nil {
		return nil, ""
	}
	return match, filepath.Clean(raw)
}

// withinRoot reports whether rel's directory, with symlinks resolved, is still
// under the repo root. It mirrors FilepathWalk: a symlinked directory is only
// descended into when the Indexer was built with followSymlinks, so without it
// an update must not read through one to a tree the full build never indexed.
// A symlink to a plain file is read either way, as the walk reads it. A parent
// that no longer resolves is allowed: nothing can be read through it, and the
// path is a deletion that must still drop the file's nodes.
func (ixr *Indexer) withinRoot(rs *repoState, rel string) bool {
	if ixr.followSymlinks {
		return true
	}
	root, err := filepath.EvalSymlinks(rs.path)
	if err != nil {
		return true // no root on disk (a snapshot built elsewhere): nothing to read
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(filepath.Join(rs.path, rel)))
	if err != nil {
		return true
	}
	r, err := filepath.Rel(root, dir)
	return err == nil && filepath.IsLocal(r)
}
