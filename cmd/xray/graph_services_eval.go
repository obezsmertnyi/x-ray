package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// serviceTruth is a hand-kept reference topology (eval/servicemap/*.truth.yaml):
// the edges a project's own architecture documents, which a derivation from the
// code is scored against. Kept outside the code on purpose — a truth read from
// the same graph would only measure the graph against itself.
type serviceTruth struct {
	Source string `yaml:"source"`
	Edges  []struct {
		From string `yaml:"from"`
		To   string `yaml:"to"`
		Via  string `yaml:"via"`
	} `yaml:"edges"`
}

// scoreServices compares a derived catalog's calls with a truth file: directed
// edge precision/recall/F1, service recall, and every hit, miss and extra edge,
// so a change to the derivation can be judged by what it gained and broke.
func scoreServices(w io.Writer, doc triageCatalog, truthPath string) error {
	raw, err := os.ReadFile(truthPath)
	if err != nil {
		return err
	}
	var truth serviceTruth
	if err := yaml.Unmarshal(raw, &truth); err != nil {
		return fmt.Errorf("parse %s: %w", truthPath, err)
	}
	want := map[string]string{}
	wantSvc := map[string]bool{}
	for _, e := range truth.Edges {
		want[e.From+" -> "+e.To] = e.Via
		wantSvc[e.From], wantSvc[e.To] = true, true
	}
	got := map[string]bool{}
	gotSvc := map[string]bool{}
	for _, s := range doc.Services {
		gotSvc[s.Name] = true
		for _, c := range s.Calls {
			got[s.Name+" -> "+c] = true
		}
	}
	var hit, miss, extra []string
	for e, via := range want {
		if got[e] {
			hit = append(hit, e+" ("+via+")")
		} else {
			miss = append(miss, e+" ("+via+")")
		}
	}
	for e := range got {
		if _, ok := want[e]; !ok {
			extra = append(extra, e)
		}
	}
	found := 0
	for s := range wantSvc {
		if gotSvc[s] {
			found++
		}
	}
	sort.Strings(hit)
	sort.Strings(miss)
	sort.Strings(extra)
	p, r := ratio(len(hit), len(got)), ratio(len(hit), len(want))
	f1 := 0.0
	if p+r > 0 {
		f1 = 2 * p * r / (p + r)
	}
	fmt.Fprintf(w, "truth %s (%s)\n", truthPath, truth.Source)
	fmt.Fprintf(w, "edges: precision=%.2f recall=%.2f f1=%.2f  (hit=%d extra=%d miss=%d)  services found=%d/%d\n",
		p, r, f1, len(hit), len(extra), len(miss), found, len(wantSvc))
	for _, l := range []struct {
		name  string
		edges []string
	}{{"hit", hit}, {"extra", extra}, {"miss", miss}} {
		if len(l.edges) > 0 {
			fmt.Fprintf(w, "  %-5s %s\n", l.name, strings.Join(l.edges, "\n        "))
		}
	}
	return nil
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}
