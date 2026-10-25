//go:build cgo

package main

// The repository half of `dump`: parse a tree with the Tree-sitter extractors
// and print what came out. CGO-only because it parses; the snapshot half lives
// untagged in dump.go and is present in the static build.

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/engine"
)

func dumpRepo(w io.Writer, repo string, o dumpOpts) error {
	nodes, edges, files, langs, err := engine.ParseRepo(repo, false)
	if err != nil {
		return fmt.Errorf("walk %s: %w", repo, err)
	}
	if o.asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"nodes": nodes, "edges": edges})
	}
	printSummary(w, nodes, edges, files, langs, modeLimit(o.limit, dumpSampleLimit))
	return nil
}
