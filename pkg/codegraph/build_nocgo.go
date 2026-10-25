//go:build !cgo

package codegraph

import "context"

const parserAvailable = false

// Build needs the Tree-sitter parser, which this build does not have.
func Build(context.Context, *Embeddings, ...Repo) (*Graph, error) { return nil, ErrNoParser }

// Reindex needs the Tree-sitter parser, which this build does not have.
func (g *Graph) Reindex(context.Context) error { return ErrNoParser }
