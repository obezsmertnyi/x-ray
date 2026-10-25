package engine

// Put is the write path for nodes that never had a file. Update re-parses a
// path on disk and then replaces that file's Result, rebuilds and re-embeds;
// Put is that tail without the parse step, for a Result the caller already
// holds — a memory note an agent asked the server to keep, a catalog entry
// added at runtime. It is untagged: nothing here needs Tree-sitter, and the
// static build serving snapshots is exactly the one that must be able to
// remember.

import (
	"context"
	"fmt"
	"time"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/hub"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/parser"
)

// Put replaces (or, when res carries no nodes, removes) the per-file Result
// stored under rel in the indexer's single repo, rebuilds the graph and embeds
// only the nodes that lost their vector on the swap. rel is a repo-relative
// path in name only: no file is read, and none needs to exist.
func (ixr *Indexer) Put(ctx context.Context, rel string, res parser.Result) error {
	ixr.mu.Lock()
	defer ixr.mu.Unlock()
	if len(ixr.repos) != 1 {
		return fmt.Errorf("put expects exactly one repo, have %d", len(ixr.repos))
	}
	rs := ixr.repos[0]
	if len(res.Nodes) == 0 {
		if _, had := rs.files[rel]; !had {
			return nil
		}
		delete(rs.files, rel)
	} else {
		rs.files[rel] = res
	}

	ixr.buildGraphLocked()
	// Unchanged nodes keep their cached embedding across the rebuild; only the
	// new text reaches the embedder.
	if err := ixr.ix.EmbedMissing(ctx); err != nil {
		return fmt.Errorf("re-embed: %w", err)
	}
	ixr.lastBuilt = time.Now()
	hub.PublishChange(hub.ChangeEvent{Op: "update", Paths: []string{repoRelPath(rs.label, rel)}})
	return nil
}

// Remove drops every per-file Result stored under rels (paths that are not
// held are ignored) and rebuilds once, so sweeping a batch of expired notes
// costs one graph build rather than one per note. Nothing is embedded: a
// removal only loses vectors. Returns how many of rels were actually held.
func (ixr *Indexer) Remove(ctx context.Context, rels []string) (int, error) {
	ixr.mu.Lock()
	defer ixr.mu.Unlock()
	if len(ixr.repos) != 1 {
		return 0, fmt.Errorf("remove expects exactly one repo, have %d", len(ixr.repos))
	}
	rs := ixr.repos[0]
	removed := 0
	for _, rel := range rels {
		if _, had := rs.files[rel]; had {
			delete(rs.files, rel)
			removed++
		}
	}
	if removed == 0 {
		return 0, nil
	}
	ixr.buildGraphLocked()
	if err := ixr.ix.EmbedMissing(ctx); err != nil { // no-op in practice; keeps the index consistent with the graph
		return removed, fmt.Errorf("re-embed: %w", err)
	}
	ixr.lastBuilt = time.Now()
	paths := make([]string, len(rels))
	for i, rel := range rels {
		paths[i] = repoRelPath(rs.label, rel)
	}
	hub.PublishChange(hub.ChangeEvent{Op: "update", Paths: paths})
	return removed, nil
}
