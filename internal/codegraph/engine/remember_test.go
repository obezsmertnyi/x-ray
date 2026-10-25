package engine

// Untagged on purpose: the static, snapshot-serving build is the one that must
// be able to remember, so these run under CGO_ENABLED=0 as well.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/index"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/mcpserver"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/parser"
)

func memoryRegistry(t *testing.T, writable bool) (*Registry, string, *stubEmbedder) {
	t.Helper()
	emb := &stubEmbedder{dims: 32}
	fp := EmbedFingerprint{Model: "stub", Dims: emb.dims, MaxInputChars: 8192}
	dir := t.TempDir()
	r := NewRegistry(emb, fp, 1, 0, writable, testLogger())
	r.SetSnapshotDir(dir)
	return r, dir, emb
}

func memoryHits(t *testing.T, r *Registry, label, query string, order index.Order) []index.Hit {
	t.Helper()
	ctx := context.Background()
	resolved, err := r.Resolve(ctx, label, query)
	if err != nil {
		t.Fatalf("resolve %s: %v", label, err)
	}
	if len(resolved) != 1 || resolved[0].Label != label {
		t.Fatalf("resolve %s: got %+v", label, resolved)
	}
	sel := index.Select{Kinds: map[string]bool{"Memory": true}, Order: order}
	hits, err := resolved[0].Index.Search(ctx, query, nil, "", sel, 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	return hits
}

func TestRemember_CreatesTheMapAndWritesItsSnapshot(t *testing.T) {
	r, dir, _ := memoryRegistry(t, true)
	ctx := context.Background()

	got, err := r.Remember(ctx, "session", Note{
		Text:  "A user asked for the most experienced academy student; answered Morgan at 30 years.",
		Title: "Academy: most experienced",
		Attrs: map[string]string{"session": "s1"},
	})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	if got.Map != "session" || got.Notes != 1 || !strings.HasPrefix(got.QN, "session//memory/") || !strings.Contains(got.QN, "-academy-most-experienced") {
		t.Fatalf("unexpected receipt: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "session.graph.gob.gz")); err != nil {
		t.Fatalf("snapshot must be flushed at once, not on shutdown: %v", err)
	}

	hits := memoryHits(t, r, "session", "experienced academy student", index.Order{})
	if len(hits) != 1 || hits[0].QN != got.QN || hits[0].Kind != "Memory" {
		t.Fatalf("the note must be the only Memory hit, got %+v", hits)
	}
	n, ok := mustResolved(t, r, "session").Graph.Get(got.QN)
	if !ok {
		t.Fatalf("node %s missing from graph", got.QN)
	}
	if n.Attrs["at"] == "" || n.Attrs["session"] != "s1" || n.Attrs["title"] != "Academy: most experienced" {
		t.Fatalf("attrs must carry the server's at plus the caller's facets: %v", n.Attrs)
	}
	if _, err := time.Parse(time.RFC3339, n.Attrs["at"]); err != nil {
		t.Fatalf("at must be RFC3339: %v", err)
	}
}

func mustResolved(t *testing.T, r *Registry, label string) mcpserver.Resolved {
	t.Helper()
	res, err := r.ResolveMap(context.Background(), label)
	if err != nil {
		t.Fatalf("resolve map %s: %v", label, err)
	}
	return res
}

func TestRemember_NotesAccumulateAndSurviveARestart(t *testing.T) {
	r, dir, emb := memoryRegistry(t, true)
	ctx := context.Background()

	if _, err := r.Remember(ctx, "session", Note{Text: "first note about alpha routing"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond) // names are millisecond timestamps; keep them distinct and ordered
	second, err := r.Remember(ctx, "session", Note{Text: "second note about beta eviction"})
	if err != nil {
		t.Fatal(err)
	}
	if second.Notes != 2 {
		t.Fatalf("second note must report 2 Memory nodes, got %d", second.Notes)
	}

	// The newest note first: this is how "what did we do last" is read back.
	hits := memoryHits(t, r, "session", "note", index.Order{Key: "at", Desc: true})
	if len(hits) != 2 || hits[0].QN != second.QN {
		t.Fatalf("order=at:desc must put the newer note first, got %+v", hits)
	}

	// A fresh process registers the flushed file and finds both notes without
	// embedding anything: the snapshot carries the vectors.
	fresh := NewRegistry(&stubEmbedder{dims: emb.dims, panicBatch: true}, EmbedFingerprint{Model: "stub", Dims: emb.dims, MaxInputChars: 8192}, 1, 0, true, testLogger())
	if err := fresh.AddSnapshot(filepath.Join(dir, "session.graph.gob.gz")); err != nil {
		t.Fatalf("register flushed memory map: %v", err)
	}
	hits = memoryHits(t, fresh, "session", "note", index.Order{Key: "at", Desc: true})
	if len(hits) != 2 || hits[0].QN != second.QN {
		t.Fatalf("both notes must survive a restart, got %+v", hits)
	}
	if res := mustResolved(t, fresh, "session"); res.SummarySource != "explicit" || !strings.Contains(res.Summary, "memory") {
		t.Fatalf("a memory map must keep its explicit router summary across a restart, got %q/%q", res.SummarySource, res.Summary)
	}
}

func TestRemember_Refusals(t *testing.T) {
	ctx := context.Background()
	ro, _, _ := memoryRegistry(t, false)
	if _, err := ro.Remember(ctx, "session", Note{Text: "x"}); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("a read-only registry must refuse, got %v", err)
	}
	rw, _, _ := memoryRegistry(t, true)
	if _, err := rw.Remember(ctx, "session", Note{Text: "   "}); err == nil {
		t.Fatal("blank text must refuse")
	}
	if _, err := rw.Remember(ctx, "", Note{Text: "x"}); err == nil {
		t.Fatal("empty label must refuse")
	}
	noDir := NewRegistry(&stubEmbedder{dims: 32}, EmbedFingerprint{Model: "stub", Dims: 32, MaxInputChars: 8192}, 1, 0, true, testLogger())
	if _, err := noDir.Remember(ctx, "session", Note{Text: "x"}); err == nil || !strings.Contains(err.Error(), "snapshot dir") {
		t.Fatalf("no snapshot dir and no such map must refuse and say why, got %v", err)
	}
}

// The watcher re-registers a path on every write, including our own flush; a
// loaded entry must keep its worked copy through that.
func TestAddSnapshot_KeepsALoadedWorkedCopyForTheSamePath(t *testing.T) {
	r, dir, _ := memoryRegistry(t, true)
	ctx := context.Background()
	if _, err := r.Remember(ctx, "session", Note{Text: "keep me resident"}); err != nil {
		t.Fatal(err)
	}
	e := r.entry("session")
	before := e.ixr
	if before == nil || !e.loaded {
		t.Fatal("a remembered map must be resident")
	}
	e.mu.Lock()
	e.nodes = 0 // pretend the header count went stale
	e.mu.Unlock()

	path := filepath.Join(dir, "session.graph.gob.gz")
	if err := r.AddSnapshot(path); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	after := r.entry("session")
	if after != e || after.ixr != before {
		t.Fatal("re-registering the same path must keep the entry and its worked copy")
	}
	if after.nodes != 1 {
		t.Fatalf("header counts must refresh from the file, got nodes=%d", after.nodes)
	}

	// Evicted (not loaded) entries are still replaced, so an external rebuild
	// takes effect.
	e.mu.Lock()
	e.ixr, e.loaded = nil, false
	e.mu.Unlock()
	if err := r.AddSnapshot(path); err != nil {
		t.Fatal(err)
	}
	if r.entry("session") == e {
		t.Fatal("an unloaded entry must be replaced on re-registration")
	}
}

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"":                              "",
		"Academy: most experienced":     "academy-most-experienced",
		"  --Weird__punct!!  ":          "weird-punct",
		strings.Repeat("abcdefghij", 6): strings.Repeat("abcdefghij", 4),
	}
	for in, want := range cases {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
}

// A note may land only in a map that holds notes: writing prose into a code
// or catalog map is memory poisoning, and the engine refuses it even though
// the tool surface no longer offers a map to choose.
func TestRemember_RefusesAMapThatIsNotANotesMap(t *testing.T) {
	ctx := context.Background()
	r, dir, emb := memoryRegistry(t, true)

	// A catalog-shaped map: one Student node, written as a snapshot the
	// registry then registers, exactly as a served map arrives.
	files := map[string]parser.Result{"academy.catalog.json": {Nodes: []parser.NodeInfo{{
		Kind: "Student", Name: "janedoe", FilePath: "academy.catalog.json", Content: "Jane Doe. 11 years.",
	}}}}
	ixr, err := NewIndexerFromResults(ctx, "academy", "memory://academy", files, emb, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	fp := EmbedFingerprint{Model: "stub", Dims: emb.dims, MaxInputChars: 8192}
	snap, err := ixr.SnapshotFor(ctx, "academy", fp, emb)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "academy.graph.gob.gz")
	if err := WriteSnapshot(path, snap); err != nil {
		t.Fatal(err)
	}
	if err := r.AddSnapshot(path); err != nil {
		t.Fatal(err)
	}

	_, err = r.Remember(ctx, "academy", Note{Text: "the most experienced student is Morgan"})
	if err == nil {
		t.Fatal("a catalog map must refuse a note")
	}
	for _, want := range []string{`map "academy" is not a notes map`, "Student=1", "agent-notes map"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q should mention %q", err, want)
		}
	}
	// Refused means untouched: no Memory node, no rewrite of the snapshot.
	if hits := memoryHits(t, r, "academy", "student", index.Order{}); len(hits) != 0 {
		t.Fatalf("a refused note must not be written, found %d Memory hits", len(hits))
	}

	// A notes-only map still accepts, twice.
	for i := 0; i < 2; i++ {
		if _, err := r.Remember(ctx, "session", Note{Text: "a note that belongs here"}); err != nil {
			t.Fatalf("notes map must accept: %v", err)
		}
	}
}
