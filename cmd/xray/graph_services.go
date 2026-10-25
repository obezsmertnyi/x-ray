package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/engine"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/graph"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/parser"
)

// services derives a service map from a code map that is already built: it
// links the snapshot's parsed nodes and edges into a graph and reads the
// topology off it (deriveTopology, services_graph.go), then writes either a
// catalog that `servicemap` embeds into a map of its own, or a Triage
// services.yaml. Nothing is re-parsed and no file is read: the topology rests
// on the same graph the code map serves, so every edge can be checked with
// get_graph_node — and a snapshot built before an extractor learned a fact
// (Config nodes, Image nodes, Java annotation edges) has to be rebuilt to show it.
func runServices(argv []string) error {
	return runServicesTo(os.Stdout, os.Stderr, argv)
}

func runServicesTo(stdout, stderr io.Writer, argv []string) error {
	fs := flag.NewFlagSet("services", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("o", "", "write the result here instead of stdout")
	format := fs.String("format", "catalog", `"catalog": entity catalog JSON for servicemap; "triage": a Triage services.yaml`)
	tier := fs.Int("tier", 2, "triage: tier written for services found in the repo (1-4; code does not say, review it)")
	extTier := fs.Int("external-tier", 3, "triage: tier written for services the repo only calls")
	team := fs.String("team", "", "triage: team written on every service (default: its domain — code names no owners, and Triage requires one)")
	truth := fs.String("truth", "", "score the derived calls against a reference topology (eval/servicemap/*.truth.yaml) and print the result to stderr")
	keyPath := snapshotKeyFlag(fs)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: xray graph services [-format catalog|triage] [-truth file] [-o file] [-snapshot-key file] <label>.graph.gob.gz")
		fs.PrintDefaults()
	}
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return fmt.Errorf("give one code-map snapshot")
	}
	if *format != "catalog" && *format != "triage" {
		return fmt.Errorf("-format %q: want catalog or triage", *format)
	}
	p := fs.Arg(0)
	if err := sniffSnapshotFile(p); err != nil {
		return err
	}
	keys, err := loadSnapshotKeys(*keyPath, "")
	if err != nil {
		return err
	}
	snap, err := engine.ReadSnapshotWith(p, keys)
	if err != nil {
		return fmt.Errorf("read snapshot %s: %w", p, err)
	}
	if snap.Header.StaleFacts() {
		fmt.Fprintf(stderr, "warning: %s was extracted under facts version %d, this binary extracts %d; services it would find from the newer facts are missing — rebuild the snapshot\n",
			p, snap.Header.Facts, parser.FactsVersion)
	}
	nodes, edges, _ := snapshotGraph(snap)
	g := graph.New()
	g.Build(graph.RepoGraph{Repo: snap.Header.Label, Nodes: nodes, Edges: edges})
	t := deriveTopology(g)
	doc := t.triage(*tier, *extTier, *team)
	if *truth != "" {
		// Scored before the empty check: a derivation that finds nothing is a
		// result to record, not an error.
		if err := scoreServices(stderr, doc, *truth); err != nil {
			return err
		}
	}
	fmt.Fprintln(stderr, t.stats())
	if len(doc.Services) == 0 {
		return fmt.Errorf("%s: no services found", p)
	}
	if *format == "triage" {
		return writeYAML(stdout, *out, doc)
	}
	w := stdout
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]any{"entries": t.catalog()})
}

// triageService is one entry of a Triage services.yaml (servicegraph's
// catalogEntry): calls and called_by must name entries in the same file and
// mirror each other, and tier must be 1-4, or the catalog fails lint.
type triageService struct {
	Name        string   `yaml:"name"`
	Aliases     []string `yaml:"aliases,omitempty"`
	Domain      string   `yaml:"domain"`
	Tier        int      `yaml:"tier"`
	Team        string   `yaml:"team,omitempty"`
	Description string   `yaml:"description"`
	Symptoms    string   `yaml:"symptoms"`
	Calls       []string `yaml:"calls"`
	CalledBy    []string `yaml:"called_by"`
}

type triageCatalog struct {
	Services []triageService `yaml:"services"`
}

func teamOr(team, fallback string) string {
	if team != "" {
		return team
	}
	return fallback
}

// domainOf is the module name's leading segment.
func domainOf(module string) string {
	if d, _, ok := strings.Cut(module, "-"); ok {
		return d
	}
	return module
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func uniqSorted(xs []string) []string {
	set := map[string]bool{}
	for _, x := range xs {
		set[x] = true
	}
	return sortedKeys(set)
}

func writeYAML(stdout io.Writer, out string, doc any) error {
	w := stdout
	if out != "" {
		f, err := os.Create(out)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	return enc.Encode(doc)
}
