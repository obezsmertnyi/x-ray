//go:build cgo

package codegraph

import (
	"context"
	"errors"
	"fmt"

	"github.com/rs/zerolog"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/engine"
)

const parserAvailable = true

// Build parses the repositories into a graph and embeds every node. It reads
// source files, Markdown and YAML under each path, skipping ignored directories
// and symlinked ones. Embedding is the slow part: a large repository makes one
// request per batch of nodes.
func Build(ctx context.Context, emb *Embeddings, repos ...Repo) (*Graph, error) {
	if len(repos) == 0 {
		return nil, errors.New("codegraph: Build needs at least one repository")
	}
	specs := make([]string, 0, len(repos))
	for _, r := range repos {
		if r.Path == "" {
			return nil, errors.New("codegraph: a repository needs a path")
		}
		if r.Label == "" {
			specs = append(specs, r.Path)
		} else {
			specs = append(specs, r.Label+"="+r.Path)
		}
	}
	ixr, err := engine.NewIndexer(ctx, specs, emb.Embedder, zerolog.Nop(), false)
	if err != nil {
		return nil, fmt.Errorf("codegraph: build: %w", err)
	}
	return &Graph{ixr: ixr, emb: emb}, nil
}

// Reindex rebuilds the graph from the files on disk.
func (g *Graph) Reindex(ctx context.Context) error {
	if err := g.ixr.Reindex(ctx); err != nil {
		return fmt.Errorf("codegraph: reindex: %w", err)
	}
	return nil
}
