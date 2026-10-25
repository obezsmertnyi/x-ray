package engine

// Untagged on purpose, like remember_test.go: expiry and forget must hold in
// the static build that serves snapshots.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/index"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/mcpserver"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/parser"
)

func noteAttrs(t *testing.T, r *Registry, label, qn string) map[string]string {
	t.Helper()
	n, ok := mustResolved(t, r, label).Graph.Get(qn)
	if !ok {
		t.Fatalf("node %s missing from graph", qn)
	}
	return n.Attrs
}

func TestRemember_ExpiryFollowsTTLThenTheDefault(t *testing.T) {
	r, _, _ := memoryRegistry(t, true)
	ctx := context.Background()

	// No ttl: the registry default, a week.
	got, err := r.Remember(ctx, "session", Note{Text: "default lifetime"})
	if err != nil {
		t.Fatal(err)
	}
	if want := got.At.Add(mcpserver.DefaultMemoryTTL); !got.Expires.Equal(want) {
		t.Fatalf("expires must be at+7d, got %v want %v", got.Expires, want)
	}
	attrs := noteAttrs(t, r, "session", got.QN)
	if attrs["expires"] != got.Expires.UTC().Format(atFormat) || len(attrs["expires"]) != len(attrs["at"]) {
		t.Fatalf("expires must be stored in at's fixed-width layout: at=%q expires=%q", attrs["at"], attrs["expires"])
	}

	// Own ttl wins over the default.
	time.Sleep(2 * time.Millisecond)
	got, err = r.Remember(ctx, "session", Note{Text: "an hour", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if want := got.At.Add(time.Hour); !got.Expires.Equal(want) {
		t.Fatalf("expires must be at+ttl, got %v want %v", got.Expires, want)
	}

	// never: no expiry attribute at all.
	time.Sleep(2 * time.Millisecond)
	got, err = r.Remember(ctx, "session", Note{Text: "forever", TTL: mcpserver.TTLNever})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Expires.IsZero() {
		t.Fatalf("a never note reports no expiry, got %v", got.Expires)
	}
	if _, has := noteAttrs(t, r, "session", got.QN)["expires"]; has {
		t.Fatal("a never note must carry no expires attribute")
	}

	// A server default of never, and a server default of a day.
	r.SetMemoryTTL(mcpserver.TTLNever)
	time.Sleep(2 * time.Millisecond)
	if got, err = r.Remember(ctx, "session", Note{Text: "server says never"}); err != nil || !got.Expires.IsZero() {
		t.Fatalf("server default never: %v %v", got.Expires, err)
	}
	r.SetMemoryTTL(24 * time.Hour)
	time.Sleep(2 * time.Millisecond)
	if got, err = r.Remember(ctx, "session", Note{Text: "server says a day"}); err != nil || !got.Expires.Equal(got.At.Add(24*time.Hour)) {
		t.Fatalf("server default 24h: %v %v", got.Expires, err)
	}
	r.SetMemoryTTL(0)
	time.Sleep(2 * time.Millisecond)
	if got, err = r.Remember(ctx, "session", Note{Text: "back to the default"}); err != nil || !got.Expires.Equal(got.At.Add(mcpserver.DefaultMemoryTTL)) {
		t.Fatalf("SetMemoryTTL(0) restores the built-in default: %v %v", got.Expires, err)
	}
}

// An expired note is gone from every reader — search, get by qn — and from
// the snapshot; a live one and a never one stay; and a fresh process that
// registers the flushed file sees the same.
func TestSweep_RemovesExpiredNotesAndFlushes(t *testing.T) {
	r, dir, emb := memoryRegistry(t, true)
	ctx := context.Background()

	dying, err := r.Remember(ctx, "session", Note{Text: "alpha note that dies in a blink", TTL: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	living, err := r.Remember(ctx, "session", Note{Text: "alpha note that lives an hour", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	forever, err := r.Remember(ctx, "session", Note{Text: "alpha note kept forever", TTL: mcpserver.TTLNever})
	if err != nil {
		t.Fatal(err)
	}
	// The second Remember loaded the map and swept the first note on its way in.
	if living.Notes != 1 || forever.Notes != 2 {
		t.Fatalf("remember sweeps before it writes: counts %d then %d, want 1 then 2", living.Notes, forever.Notes)
	}
	hits := memoryHits(t, r, "session", "alpha note", index.Order{Key: "at", Desc: true})
	if len(hits) != 2 || hits[0].QN != forever.QN || hits[1].QN != living.QN {
		t.Fatalf("search must see the two live notes only, got %+v", hits)
	}
	if _, ok := mustResolved(t, r, "session").Graph.Get(dying.QN); ok {
		t.Fatal("the expired note is still in the graph")
	}

	// An explicit sweep with nothing dead is a no-op; one in the future takes
	// the hour note and leaves the never note.
	if n, err := r.SweepMemory(ctx, "session", time.Now()); err != nil || n != 0 {
		t.Fatalf("nothing to sweep: n=%d err=%v", n, err)
	}
	if n, err := r.SweepMemory(ctx, "session", time.Now().Add(2*time.Hour)); err != nil || n != 1 {
		t.Fatalf("the hour note must go: n=%d err=%v", n, err)
	}
	if _, err := r.SweepMemory(ctx, "nope", time.Now()); err == nil {
		t.Fatal("sweeping an unknown map must fail, not silently do nothing")
	}

	// The flush carried the removal: a fresh process sees one note, no embed.
	fresh := NewRegistry(&stubEmbedder{dims: emb.dims, panicBatch: true}, EmbedFingerprint{Model: "stub", Dims: emb.dims, MaxInputChars: 8192}, 1, 0, true, testLogger())
	if err := fresh.AddSnapshot(filepath.Join(dir, "session.graph.gob.gz")); err != nil {
		t.Fatal(err)
	}
	hits = memoryHits(t, fresh, "session", "alpha note", index.Order{})
	if len(hits) != 1 || hits[0].QN != forever.QN {
		t.Fatalf("only the never note survives on disk, got %+v", hits)
	}
}

// A note written before expiry existed carries no expires and is never swept:
// the migration must not delete what an agent was told was remembered.
func TestSweep_LeavesNotesWithoutAnExpiry(t *testing.T) {
	r, dir, emb := memoryRegistry(t, true)
	ctx := context.Background()
	files := map[string]parser.Result{"memory/old.md": {Nodes: []parser.NodeInfo{{
		Kind: memoryKind, Name: "old", FilePath: "memory/old.md", Language: "memory",
		Content: "an old note", Body: "an old note", Attrs: map[string]string{"at": "2020-01-01T00:00:00.000Z"},
	}}}}
	ixr, err := NewIndexerFromResults(ctx, "session", "memory://session", files, emb, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := ixr.SnapshotFor(ctx, "session", EmbedFingerprint{Model: "stub", Dims: emb.dims, MaxInputChars: 8192}, emb)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteSnapshot(filepath.Join(dir, "session.graph.gob.gz"), snap); err != nil {
		t.Fatal(err)
	}
	if err := r.AddSnapshot(filepath.Join(dir, "session.graph.gob.gz")); err != nil {
		t.Fatal(err)
	}
	if n, err := r.SweepMemory(ctx, "session", time.Now().Add(100*365*24*time.Hour)); err != nil || n != 0 {
		t.Fatalf("a note with no expires is never swept: n=%d err=%v", n, err)
	}
	if hits := memoryHits(t, r, "session", "old note", index.Order{}); len(hits) != 1 {
		t.Fatalf("the old note must still be served, got %+v", hits)
	}
}

// The timed sweeper reaches a resident notes map nobody is reading.
func TestStartMemorySweeper_SweepsOnATimer(t *testing.T) {
	r, _, _ := memoryRegistry(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := r.Remember(ctx, "session", Note{Text: "blink", TTL: time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	wait := r.StartMemorySweeper(ctx, 5*time.Millisecond)
	// Stop the sweeper and let its in-flight sweep finish before the test
	// returns: the note leaves the graph before the flush writes the snapshot,
	// and t.TempDir's RemoveAll runs after this defer. Without the wait, the
	// flush recreates the file mid-removal and cleanup fails with the directory
	// not empty — which is how CI failed while a fast local disk never did.
	defer func() { cancel(); wait() }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if e := r.entry("session"); e != nil {
			e.mu.RLock()
			n := e.ixr.Graph().Stats().ByKind[memoryKind]
			e.mu.RUnlock()
			if n == 0 {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the sweeper never removed the expired note")
}

func TestForget_RemovesExactlyOneNote(t *testing.T) {
	r, dir, emb := memoryRegistry(t, true)
	ctx := context.Background()
	first, err := r.Remember(ctx, "session", Note{Text: "alpha first", TTL: mcpserver.TTLNever})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	second, err := r.Remember(ctx, "session", Note{Text: "alpha second", TTL: mcpserver.TTLNever})
	if err != nil {
		t.Fatal(err)
	}

	got, err := r.Forget(ctx, "session", first.QN)
	if err != nil {
		t.Fatalf("forget: %v", err)
	}
	if got.Map != "session" || got.QN != first.QN || got.Notes != 1 {
		t.Fatalf("receipt: %+v", got)
	}
	hits := memoryHits(t, r, "session", "alpha", index.Order{})
	if len(hits) != 1 || hits[0].QN != second.QN {
		t.Fatalf("only the second note remains, got %+v", hits)
	}
	// Forgotten twice: the second call says so rather than pretending.
	if _, err := r.Forget(ctx, "session", first.QN); err == nil || !strings.Contains(err.Error(), "no note") {
		t.Fatalf("a forgotten note is reported missing, got %v", err)
	}
	// And the removal is on disk.
	fresh := NewRegistry(&stubEmbedder{dims: emb.dims, panicBatch: true}, EmbedFingerprint{Model: "stub", Dims: emb.dims, MaxInputChars: 8192}, 1, 0, true, testLogger())
	if err := fresh.AddSnapshot(filepath.Join(dir, "session.graph.gob.gz")); err != nil {
		t.Fatal(err)
	}
	if hits := memoryHits(t, fresh, "session", "alpha", index.Order{}); len(hits) != 1 || hits[0].QN != second.QN {
		t.Fatalf("forget must be flushed at once, got %+v", hits)
	}
}

func TestForget_Refusals(t *testing.T) {
	ctx := context.Background()
	ro, _, _ := memoryRegistry(t, false)
	if _, err := ro.Forget(ctx, "session", "session//memory/x.md::x"); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("a read-only registry must refuse, got %v", err)
	}

	r, dir, emb := memoryRegistry(t, true)
	// A catalog map registered beside the notes map.
	files := map[string]parser.Result{"team.catalog.json": {Nodes: []parser.NodeInfo{{
		Kind: "Student", Name: "janedoe", FilePath: "team.catalog.json", Content: "Jane Doe. 11 years.",
	}}}}
	ixr, err := NewIndexerFromResults(ctx, "team", "memory://team", files, emb, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	fp := EmbedFingerprint{Model: "stub", Dims: emb.dims, MaxInputChars: 8192}
	snap, err := ixr.SnapshotFor(ctx, "team", fp, emb)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteSnapshot(filepath.Join(dir, "team.graph.gob.gz"), snap); err != nil {
		t.Fatal(err)
	}
	if err := r.AddSnapshot(filepath.Join(dir, "team.graph.gob.gz")); err != nil {
		t.Fatal(err)
	}
	note, err := r.Remember(ctx, "session", Note{Text: "a note"})
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string]struct{ label, qn, want string }{
		"qn in another map":        {"session", "team//team.catalog.json::janedoe", `is in map "team"`},
		"catalog map as the label": {"team", "team//team.catalog.json::janedoe", "not a notes map"},
		"blank qn":                 {"session", " ", "qn is required"},
		"blank label":              {"", note.QN, "label is required"},
		"unknown map":              {"nope", "nope//memory/x.md::x", "no map named"},
		"unknown note":             {"session", "session//memory/x.md::x", "no note"},
	}
	for name, c := range cases {
		_, err := r.Forget(ctx, c.label, c.qn)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want an error mentioning %q, got %v", name, c.want, err)
		}
	}
	// Nothing was touched by any refusal.
	if hits := memoryHits(t, r, "team", "student", index.Order{}); len(hits) != 0 {
		t.Fatalf("the catalog map must be untouched, found Memory hits %+v", hits)
	}
	if _, ok := mustResolved(t, r, "team").Graph.Get("team//team.catalog.json::janedoe"); !ok {
		t.Fatal("the Student node must still be there")
	}
	if _, ok := mustResolved(t, r, "session").Graph.Get(note.QN); !ok {
		t.Fatal("the note must still be there")
	}
}

// The explicit router summary of a notes map must survive reload → flush.
// Before: Registry.load built the Indexer from the snapshot without carrying
// the explicit summary, and the first Remember after a restart flushed a
// stats-derived "session — code graph (Memory=4). Symbols:" over it.
func TestMemoryMap_ExplicitSummarySurvivesReloadAndFlush(t *testing.T) {
	r, dir, emb := memoryRegistry(t, true)
	ctx := context.Background()
	if _, err := r.Remember(ctx, "session", Note{Text: "first"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "session.graph.gob.gz")

	// A fresh process: register from the file, then write again — the write
	// reloads the map and flushes it.
	fresh := NewRegistry(&stubEmbedder{dims: emb.dims}, EmbedFingerprint{Model: "stub", Dims: emb.dims, MaxInputChars: 8192}, 1, 0, true, testLogger())
	fresh.SetSnapshotDir(dir)
	if err := fresh.AddSnapshot(path); err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.Remember(ctx, "session", Note{Text: "second"}); err != nil {
		t.Fatal(err)
	}
	h, err := ReadSnapshotHeader(path)
	if err != nil {
		t.Fatal(err)
	}
	if h.SummarySource != "explicit" || !strings.Contains(h.Summary, "memory: notes") {
		t.Fatalf("the flushed header must keep the explicit summary, got %q / %q", h.SummarySource, h.Summary)
	}
	// The resident entry routes as what is on disk, without the watcher.
	if res := mustResolved(t, fresh, "session"); res.SummarySource != "explicit" || res.Summary != h.Summary {
		t.Fatalf("resident summary must match the flushed header, got %q / %q", res.SummarySource, res.Summary)
	}

	// A file already degraded to a stats summary heals on the next write.
	degraded := NewRegistry(&stubEmbedder{dims: emb.dims}, EmbedFingerprint{Model: "stub", Dims: emb.dims, MaxInputChars: 8192}, 1, 0, true, testLogger())
	degraded.SetSnapshotDir(dir)
	snap, err := ReadSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	snap.Header.Summary, snap.Header.SummarySource = "session — code graph (Memory=2). Symbols: ", "stats"
	if err := WriteSnapshot(path, snap); err != nil {
		t.Fatal(err)
	}
	if err := degraded.AddSnapshot(path); err != nil {
		t.Fatal(err)
	}
	if _, err := degraded.Remember(ctx, "session", Note{Text: "third"}); err != nil {
		t.Fatal(err)
	}
	if h, err = ReadSnapshotHeader(path); err != nil || h.SummarySource != "explicit" || !strings.Contains(h.Summary, "memory: notes") {
		t.Fatalf("a degraded header must heal on the next remember, got %q / %q (%v)", h.SummarySource, h.Summary, err)
	}
}
