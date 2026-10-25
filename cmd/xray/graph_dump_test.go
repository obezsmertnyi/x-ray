package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/engine"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/parser"
)

// writeSnapshotFixture writes a tiny snapshot with three 4-dim vectors and
// returns its path. Keys are chosen so sorted order and grep hits are known.
func writeSnapshotFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fix.graph.gob.gz")
	snap := &engine.Snapshot{
		Header: engine.SnapshotHeader{
			Version:       engine.SnapshotVersion,
			Label:         "fix",
			RepoPath:      "/repo/fix",
			Fingerprint:   engine.EmbedFingerprint{Model: "stub", Dims: 4, MaxInputChars: 512},
			BuiltAt:       time.Date(2026, 9, 16, 8, 0, 0, 0, time.UTC),
			Nodes:         3,
			Edges:         1,
			Summary:       "fix: a fixture map about apples and pears",
			SummaryVec:    []float32{1, 0, 0, 0},
			SummarySource: "explicit",
		},
		Body: engine.SnapshotBody{
			Files: map[string]parser.Result{
				"a.go": {Nodes: []parser.NodeInfo{{Kind: "Func", Name: "Apple", FilePath: "a.go", Language: "go", LineStart: 1, LineEnd: 3}}},
				"b.go": {
					Nodes: []parser.NodeInfo{{Kind: "Func", Name: "Banana", FilePath: "b.go", Language: "go", LineStart: 5, LineEnd: 9}},
					Edges: []parser.EdgeInfo{{Source: "b.go::Banana", Target: "a.go::Apple", Kind: "calls", File: "b.go", Line: 7}},
				},
				"p.py": {Nodes: []parser.NodeInfo{{Kind: "Func", Name: "Pear", FilePath: "p.py", Language: "python", LineStart: 2, LineEnd: 4}}},
			},
			Cache: map[string][]float32{
				"Func Apple in a.go":  {1, 0, 0, 0},
				"Func Banana in b.go": {0, 1, 0, 0},
				"Func Pear in p.go":   {0, 0, 0.6, 0.8},
			},
			HNSW: map[string][]byte{"idx": []byte("0123456789")},
		},
	}
	if err := engine.WriteSnapshot(path, snap); err != nil {
		t.Fatal(err)
	}
	return path
}

// runDumpForTest runs `dump` with the given arguments and returns its streams.
func runDumpForTest(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var out, errb bytes.Buffer
	err := runDumpTo(&out, &errb, args)
	return out.String(), errb.String(), err
}

// runVectorsForTest is `dump -vectors …`: the listing view the vectors
// subcommand used to be, kept as a helper so its assertions read unchanged.
func runVectorsForTest(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	// Go's flag parsing stops at the first positional, so -vectors goes right
	// after a leading target and in front of everything otherwise.
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return runDumpForTest(t, append([]string{args[0], "-vectors"}, args[1:]...)...)
	}
	return runDumpForTest(t, append([]string{"-vectors"}, args...)...)
}

func TestDump_HeaderOnly(t *testing.T) {
	path := writeSnapshotFixture(t)
	out, _, err := runVectorsForTest(t, "-header", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"label:          fix",
		"repo:           /repo/fix",
		"built:          2026-09-16T08:00:00Z",
		"model:          stub",
		"dims:           4",
		"maxInputChars:  512",
		"nodes:          3",
		"edges:          1",
		"vectors:        -", // header-only never reads the body
		"summarySource:  explicit",
		"apples and pears",
		"routingVector:  dims=4 norm=1.0000 head=[1 0 0 0]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("header output lacks %q:\n%s", want, out)
		}
	}
}

func TestDump_DefaultListsAllSorted(t *testing.T) {
	path := writeSnapshotFixture(t)
	out, errb, err := runVectorsForTest(t, path)
	if err != nil {
		t.Fatal(err)
	}
	if errb != "" {
		t.Errorf("no dims mismatch in the fixture, got warning: %s", errb)
	}
	if !strings.HasPrefix(out, "label=fix model=stub dims=4 nodes=3 vectors=3 hnswBytes=10\n") {
		t.Errorf("header line wrong:\n%s", out)
	}
	a, b, p := strings.Index(out, "Apple"), strings.Index(out, "Banana"), strings.Index(out, "Pear")
	if a < 0 || b < 0 || p < 0 || !(a < b && b < p) {
		t.Errorf("vectors must print in sorted key order:\n%s", out)
	}
	if !strings.Contains(out, "norm=1.0000  [0 0 0.6 0.8]") {
		t.Errorf("pear vector line wrong:\n%s", out)
	}
	if !strings.HasSuffix(out, "shown=3 of 3 (matching 3)\n") {
		t.Errorf("trailer wrong:\n%s", out)
	}
}

func TestDump_GrepIsCaseInsensitive(t *testing.T) {
	path := writeSnapshotFixture(t)
	out, _, err := runVectorsForTest(t, "-grep", "PEAR", path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "Apple") || strings.Contains(out, "Banana") || !strings.Contains(out, "Pear") {
		t.Errorf("grep must keep only the pear:\n%s", out)
	}
	if !strings.HasSuffix(out, "shown=1 of 3 (matching 1)\n") {
		t.Errorf("trailer wrong:\n%s", out)
	}
}

func TestDump_LimitCapsOutput(t *testing.T) {
	path := writeSnapshotFixture(t)
	out, _, err := runVectorsForTest(t, "-limit", "2", path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "--- ") != 2 {
		t.Errorf("limit=2 must print two vectors:\n%s", out)
	}
	if !strings.HasSuffix(out, "shown=2 of 3 (matching 3)\n") {
		t.Errorf("trailer wrong:\n%s", out)
	}
	// 0 = all
	out, _, err = runVectorsForTest(t, "-limit", "0", path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "--- ") != 3 {
		t.Errorf("limit=0 must print all:\n%s", out)
	}
}

func TestDump_NTruncatesComponents(t *testing.T) {
	path := writeSnapshotFixture(t)
	out, _, err := runVectorsForTest(t, "-n", "2", "-grep", "pear", path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "norm=1.0000  [0 0]\n") {
		t.Errorf("-n 2 must print two components (norm stays over the whole vector):\n%s", out)
	}
	out, _, err = runVectorsForTest(t, "-n", "0", "-grep", "pear", path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[0 0 0.6 0.8]") {
		t.Errorf("-n 0 must print the whole vector:\n%s", out)
	}
}

func TestDump_JSONLines(t *testing.T) {
	path := writeSnapshotFixture(t)
	out, _, err := runVectorsForTest(t, "-json", "-n", "2", path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("-json prints one object per vector and nothing else, got %d lines:\n%s", len(lines), out)
	}
	var first vectorLine
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("line 0 is not JSON: %v\n%s", err, lines[0])
	}
	if first.Text != "Func Apple in a.go" || first.Dims != 4 || first.Norm != 1 || len(first.Vector) != 2 {
		t.Errorf("unexpected first object: %+v", first)
	}
}

func TestDump_RoutingVector(t *testing.T) {
	path := writeSnapshotFixture(t)
	out, _, err := runVectorsForTest(t, "-routing", path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "routing vector: dims=4 norm=1.0000\n[1 0 0 0]\n") {
		t.Errorf("routing output wrong:\n%s", out)
	}
}

func TestDump_DimsMismatchWarns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.graph.gob.gz")
	snap := &engine.Snapshot{
		Header: engine.SnapshotHeader{Version: engine.SnapshotVersion, Label: "bad", Fingerprint: engine.EmbedFingerprint{Dims: 4}},
		Body:   engine.SnapshotBody{Cache: map[string][]float32{"short": {1, 0}}},
	}
	if err := engine.WriteSnapshot(path, snap); err != nil {
		t.Fatal(err)
	}
	_, errb, err := runVectorsForTest(t, path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errb, "warning: vector has 2 dims, header says 4") {
		t.Errorf("expected a dims warning on stderr, got: %q", errb)
	}
}

// The documented form puts the target first; flags must still apply after it.
func TestDump_PathFirstThenFlags(t *testing.T) {
	path := writeSnapshotFixture(t)
	out, _, err := runVectorsForTest(t, path, "-grep", "pear", "-limit", "1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "Apple") || !strings.Contains(out, "Pear") || !strings.HasSuffix(out, "shown=1 of 3 (matching 1)\n") {
		t.Errorf("flags after the path must apply:\n%s", out)
	}
	if _, _, err := runVectorsForTest(t, path, path); err == nil {
		t.Error("two paths must be an error")
	}
}

func TestDump_Errors(t *testing.T) {
	if _, _, err := runVectorsForTest(t, filepath.Join(t.TempDir(), "missing.graph.gob.gz")); err == nil {
		t.Error("a missing file must be an error")
	}
	if _, _, err := runVectorsForTest(t, "-header", filepath.Join(t.TempDir(), "missing.graph.gob.gz")); err == nil {
		t.Error("a missing file must be an error in -header mode too")
	}
	// A bare `dump -vectors` targets "." — a directory — and the snapshot-only
	// flags must refuse it rather than parse the tree.
	if _, _, err := runVectorsForTest(t); err == nil {
		t.Error("-vectors on a directory target must be an error")
	}
	if _, _, err := runDumpForTest(t, "-header", t.TempDir()); err == nil {
		t.Error("-header on a directory target must be an error")
	}
}

// The default view on a snapshot is the header line plus the same summary the
// repository path prints, built from the snapshot's parsed results.
func TestDump_SnapshotDefaultSummary(t *testing.T) {
	path := writeSnapshotFixture(t)
	out, _, err := runDumpForTest(t, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"label=fix model=stub dims=4 nodes=3 vectors=3 hnswBytes=10\n",
		"Code Graph Summary",
		"Files parsed: 3",
		"Nodes:        3",
		"Edges:        1",
		"Languages:    go=2 python=1",
		"Nodes by kind: Func=3",
		"Edges by kind: calls=1",
		"[Func] a.go::Apple  (L1-3)",
		"b.go::Banana --calls--> a.go::Apple  (b.go:7)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
	// Files walk in sorted order, so the samples are stable run to run.
	if a, p := strings.Index(out, "Apple"), strings.Index(out, "Pear"); !(a >= 0 && p > a) {
		t.Errorf("sample nodes must be in file order:\n%s", out)
	}
	// -limit bounds the sample rows in this mode too.
	out, _, err = runDumpForTest(t, path, "-limit", "1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Sample nodes (first 1):") || strings.Contains(out, "[Func] b.go::Banana") {
		t.Errorf("-limit 1 must print one sample node:\n%s", out)
	}
}

// -json on a snapshot is one object: header + nodes + edges.
func TestDump_SnapshotJSON(t *testing.T) {
	path := writeSnapshotFixture(t)
	out, _, err := runDumpForTest(t, path, "-json")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Header headerJSON        `json:"header"`
		Nodes  []parser.NodeInfo `json:"nodes"`
		Edges  []parser.EdgeInfo `json:"edges"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if got.Header.Label != "fix" || got.Header.Dims != 4 || got.Header.Vectors != 3 || got.Header.HNSWBytes != 10 || len(got.Nodes) != 3 || len(got.Edges) != 1 {
		t.Errorf("unexpected object: header=%+v nodes=%d edges=%d", got.Header, len(got.Nodes), len(got.Edges))
	}
}

// -header -json is the header object alone; vectors/hnswBytes read -1 because
// the body was never decoded.
func TestDump_HeaderJSON(t *testing.T) {
	path := writeSnapshotFixture(t)
	out, _, err := runDumpForTest(t, "-header", "-json", "-n", "2", path)
	if err != nil {
		t.Fatal(err)
	}
	var h headerJSON
	if err := json.Unmarshal([]byte(out), &h); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if h.Label != "fix" || h.Vectors != -1 || h.HNSWBytes != -1 || h.SummarySource != "explicit" || h.RoutingDims != 4 || len(h.RoutingHead) != 2 {
		t.Errorf("unexpected header object: %+v", h)
	}
}

func TestDump_RoutingJSON(t *testing.T) {
	path := writeSnapshotFixture(t)
	out, _, err := runDumpForTest(t, "-routing", "-json", path)
	if err != nil {
		t.Fatal(err)
	}
	var v vectorLine
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if v.Text != "" || v.Dims != 4 || v.Norm != 1 || len(v.Vector) != 4 {
		t.Errorf("unexpected routing object: %+v", v)
	}
}

// A regular file that is neither gzip nor age is named as such, instead of
// surfacing a codec error from inside the decoder.
func TestDump_NotASnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.graph.gob.gz")
	if err := os.WriteFile(path, []byte("hello, not a snapshot"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := runDumpForTest(t, path)
	if err == nil || !strings.Contains(err.Error(), "not a snapshot") {
		t.Fatalf("want a 'not a snapshot' error, got %v", err)
	}
	if _, _, err := runDumpForTest(t, "-header", path); err == nil || !strings.Contains(err.Error(), "not a snapshot") {
		t.Fatalf("-header: want a 'not a snapshot' error, got %v", err)
	}
}

// -repo and a positional target are the same thing; naming both differently
// is an error, and naming neither means the current directory.
func TestDump_RepoFlagAndTarget(t *testing.T) {
	path := writeSnapshotFixture(t)
	if _, _, err := runDumpForTest(t, path, "-repo", t.TempDir()); err == nil || !strings.Contains(err.Error(), "use one") {
		t.Fatalf("target and a different -repo must be refused, got %v", err)
	}
	if _, _, err := runDumpForTest(t, "-repo", path, "-header"); err != nil {
		t.Fatalf("-repo pointing at a snapshot must still work: %v", err)
	}
}

// impact reads the map the qn's label names from -snapshot-dir and walks it
// with no server: Banana calls Apple, so seeding Apple reaches Banana.
func TestImpact_ReadsSnapshotByLabel(t *testing.T) {
	dir := filepath.Dir(writeSnapshotFixture(t))
	var out, errb bytes.Buffer
	if err := runImpactTo(&out, &errb, []string{"-snapshot-dir", dir, "-depth", "1", "fix//a.go::Apple"}); err != nil {
		t.Fatalf("%v\n%s", err, errb.String())
	}
	var got struct {
		Count    int `json:"count"`
		Depth    int `json:"depth"`
		Impacted []struct{ QN string }
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if got.Count != 2 || got.Depth != 1 || got.Impacted[1].QN != "fix//b.go::Banana" {
		t.Fatalf("got %+v", got)
	}
}

func TestImpact_Errors(t *testing.T) {
	dir := filepath.Dir(writeSnapshotFixture(t))
	for name, args := range map[string][]string{
		"no seeds":     {"-snapshot-dir", dir},
		"no label":     {"-snapshot-dir", dir, "a.go::Apple"},
		"missing map":  {"-snapshot-dir", dir, "nosuch//a.go::Apple"},
		"escaping dir": {"-snapshot-dir", dir, "..//a.go::Apple"},
	} {
		var out, errb bytes.Buffer
		if err := runImpactTo(&out, &errb, args); err == nil {
			t.Errorf("%s: want an error, got %s", name, out.String())
		}
	}
}
