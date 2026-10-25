package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/engine"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/graph"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/mcpserver"
)

// envSnapshotDir names the snapshot directory for the standalone readers, so a
// shell sets it once instead of on every call.
const envSnapshotDir = "XRAY_SNAPSHOT_DIR"

func runImpact(argv []string) error {
	return runImpactTo(os.Stdout, os.Stderr, argv)
}

// runImpactTo is get_graph_impact without a server: it reads the snapshot the
// first seed's label// prefix names, rebuilds that map's graph, and prints the
// tool's own reply (mcpserver.ImpactDirectGraph). Blast radius is a walk over
// the graph alone, so no embedder, no index and no HNSW decode are involved —
// it runs from the static binary against the same files serve-snapshots reads.
func runImpactTo(stdout, stderr io.Writer, argv []string) error {
	fs := flag.NewFlagSet("impact", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("snapshot-dir", defaultSnapshotDir(), "directory of <label>.graph.gob.gz snapshots; the first seed's label picks the file. Default $"+envSnapshotDir+", else ~/.xray-maps")
	depth := fs.Int("depth", 0, "max traversal depth (0 = the tool's default, 2)")
	maxNodes := fs.Int("max-nodes", 0, "cap on impacted nodes (0 = the tool's default, 200)")
	keyPath := snapshotKeyFlag(fs)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: xray graph impact [-snapshot-dir dir] [-depth n] [-max-nodes n] [-snapshot-key file] <qn>...")
		fs.PrintDefaults()
	}
	if err := fs.Parse(argv); err != nil {
		return err
	}
	seeds := fs.Args()
	if len(seeds) == 0 {
		fs.Usage()
		return fmt.Errorf("give at least one qn (label//path::Symbol, as search prints it)")
	}
	g, err := loadSnapshotGraph(*dir, seeds[0], *keyPath)
	if err != nil {
		return err
	}
	// Seeds from another map come back under "unknown", as they do from the
	// tool: one call walks one map, and ranking never crosses maps.
	out, err := mcpserver.ImpactDirectGraph(g, seeds, *depth, *maxNodes)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, out)
	return err
}

// defaultSnapshotDir is $XRAY_SNAPSHOT_DIR, else ~/.xray-maps.
func defaultSnapshotDir() string {
	if d := os.Getenv(envSnapshotDir); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".xray-maps"
	}
	return filepath.Join(home, ".xray-maps")
}

// loadSnapshotGraph reads the snapshot for qn's label and links its graph the
// way a loaded map is linked (graph.Build over every file's parsed results).
func loadSnapshotGraph(dir, qn, keyPath string) (*graph.Graph, error) {
	label, _, ok := strings.Cut(qn, "//")
	if !ok || label == "" {
		return nil, fmt.Errorf("qn %q has no label// prefix, so it names no map", qn)
	}
	path, err := snapshotPath(dir, label)
	if err != nil {
		return nil, fmt.Errorf("qn %q: %w", qn, err)
	}
	if err := sniffSnapshotFile(path); err != nil {
		return nil, err
	}
	keys, err := loadSnapshotKeys(keyPath, "")
	if err != nil {
		return nil, err
	}
	snap, err := engine.ReadSnapshotWith(path, keys)
	if err != nil {
		return nil, fmt.Errorf("read snapshot %s: %w", path, err)
	}
	nodes, edges, _ := snapshotGraph(snap)
	g := graph.New()
	g.Build(graph.RepoGraph{Repo: snap.Header.Label, Nodes: nodes, Edges: edges})
	return g, nil
}
