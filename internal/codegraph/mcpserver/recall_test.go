package mcpserver

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/server"
	"github.com/rs/zerolog"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/graph"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/index"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/parser"
)

// textEmbedder derives a deterministic vector from text, with a strong
// component for the token "alpha", so a query holding it ranks alpha notes
// first without a network.
type textEmbedder struct{}

func (textEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = textVec(t)
	}
	return out, nil
}

func (textEmbedder) EmbedQuery(_ context.Context, text string) ([]float32, error) {
	return textVec(text), nil
}

func textVec(text string) []float32 {
	v := make([]float32, 8)
	h := fnv.New32a()
	_, _ = h.Write([]byte(text))
	seed := h.Sum32()
	for i := range v {
		seed = seed*1664525 + 1013904223
		v[i] = float32(int32(seed)) / float32(1<<31) / 10
	}
	if strings.Contains(text, "alpha") {
		v[0] += 5
	}
	return v
}

// notesGraph is a notes map with three notes written a minute apart, the
// middle one expired, plus a fixed live-mode variant with a foreign repo.
func notesGraph(t *testing.T, repo string) (*graph.Graph, *index.Index) {
	t.Helper()
	at := func(min int) string {
		return time.Date(2026, 9, 15, 19, min, 0, 0, time.UTC).Format("2006-01-02T15:04:05.000Z07:00")
	}
	nodes := []parser.NodeInfo{
		{Kind: "Memory", Name: "n1", FilePath: "memory/n1.md", Content: "first: alpha routing decided", Body: "first: alpha routing decided",
			Attrs: map[string]string{"at": at(1), "title": "alpha", "topic": "routing", "expires": "2999-01-01T00:00:00.000Z"}},
		{Kind: "Memory", Name: "n2", FilePath: "memory/n2.md", Content: "second: beta eviction, expired", Body: "second: beta eviction, expired",
			Attrs: map[string]string{"at": at(2), "expires": "2020-01-01T00:00:00.000Z"}},
		{Kind: "Memory", Name: "n3", FilePath: "memory/n3.md", Content: "third: gamma snapshot, no expiry", Body: "third: gamma snapshot, no expiry",
			Attrs: map[string]string{"at": at(3)}},
	}
	g := graph.New()
	g.Build(graph.RepoGraph{Repo: repo, Nodes: nodes}, graph.RepoGraph{Repo: "code", Nodes: []parser.NodeInfo{
		{Kind: "Func", Name: "Alpha", FilePath: "a.go", Content: "func Alpha() alpha alpha"},
	}})
	ix := index.New(g, textEmbedder{}, zerolog.Nop())
	if err := ix.Build(context.Background()); err != nil {
		t.Fatal(err)
	}
	return g, ix
}

// fakeResolver serves one labelled map and, like the registry, falls back to
// it for any other name — which recall must refuse to accept as its own.
type fakeResolver struct {
	label string
	g     *graph.Graph
	ix    *index.Index
}

func (f *fakeResolver) Resolve(_ context.Context, _ string, _ string) ([]Resolved, error) {
	return []Resolved{{Label: f.label, Graph: f.g, Index: f.ix}}, nil
}
func (f *fakeResolver) ResolveByQN(_ context.Context, _ string) (Resolved, error) {
	return Resolved{Label: f.label, Graph: f.g, Index: f.ix}, nil
}
func (f *fakeResolver) UpdateByPaths(context.Context, []string) (int, int, int, []string, error) {
	return 0, 0, 0, nil, nil
}
func (f *fakeResolver) RecordSearch(string, int) {}
func (f *fakeResolver) Metrics() map[string]any  { return nil }

func TestRecall_NewestFirstWithoutAQuery_ExcludingExpired(t *testing.T) {
	g, ix := notesGraph(t, "session")
	d := Deps{Resolver: &fakeResolver{label: "session", g: g, ix: ix}, MemoryMap: "session"}
	out, err := recall(context.Background(), d, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	notes, _ := out["notes"].([]map[string]any)
	if out["count"] != 2 || len(notes) != 2 {
		t.Fatalf("two live notes expected (the expired one dropped), got %v", out)
	}
	if notes[0]["qn"] != "session//memory/n3.md::n3" || notes[1]["qn"] != "session//memory/n1.md::n1" {
		t.Fatalf("newest first: got %v then %v", notes[0]["qn"], notes[1]["qn"])
	}
	// Full text, lifted fields, and the caller's facets alone under attrs.
	if notes[1]["text"] != "first: alpha routing decided" || notes[1]["title"] != "alpha" || notes[1]["expires"] != "2999-01-01T00:00:00.000Z" {
		t.Fatalf("note fields: %v", notes[1])
	}
	if attrs, _ := notes[1]["attrs"].(map[string]string); len(attrs) != 1 || attrs["topic"] != "routing" {
		t.Fatalf("attrs must hold the caller's facets only, got %v", notes[1]["attrs"])
	}
	if _, has := notes[0]["attrs"]; has {
		t.Fatalf("a note with no facets carries no attrs, got %v", notes[0])
	}
	if _, has := notes[0]["expires"]; has {
		t.Fatalf("a never-expiring note carries no expires, got %v", notes[0])
	}
}

func TestRecall_AQueryRanksAndLimitCaps(t *testing.T) {
	g, ix := notesGraph(t, "session")
	d := Deps{Resolver: &fakeResolver{label: "session", g: g, ix: ix}, MemoryMap: "session"}
	out, err := recall(context.Background(), d, "alpha", 0)
	if err != nil {
		t.Fatal(err)
	}
	notes, _ := out["notes"].([]map[string]any)
	if len(notes) != 2 || notes[0]["qn"] != "session//memory/n1.md::n1" {
		t.Fatalf("the alpha note must rank first for an alpha query, got %v", notes)
	}
	if out["query"] != "alpha" {
		t.Fatalf("the reply echoes the query, got %v", out["query"])
	}
	for _, n := range notes {
		if n["qn"] == "session//memory/n2.md::n2" {
			t.Fatal("an expired note must not be recalled even when the query matches it")
		}
	}
	out, err = recall(context.Background(), d, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if out["count"] != 1 {
		t.Fatalf("limit=1 must cap the reply, got %v", out["count"])
	}
	if out, _ := recall(context.Background(), d, "", 1000); out["count"] != 2 {
		t.Fatalf("an oversized limit is clamped, not refused: %v", out)
	}
}

// Live mode: the fixed graph holds several repos; only the notes repo's Memory
// nodes come back, and the code repo's Alpha never does.
func TestRecall_LiveModeFiltersToTheNotesRepo(t *testing.T) {
	g, ix := notesGraph(t, "session")
	d := Deps{Graph: g, Index: ix, MemoryMap: "session"}
	for _, q := range []string{"", "alpha"} {
		out, err := recall(context.Background(), d, q, 0)
		if err != nil {
			t.Fatal(err)
		}
		notes, _ := out["notes"].([]map[string]any)
		if len(notes) != 2 {
			t.Fatalf("query %q: two live notes expected, got %v", q, out)
		}
		for _, n := range notes {
			if !strings.HasPrefix(n["qn"].(string), "session//memory/") {
				t.Fatalf("query %q: a non-note leaked into recall: %v", q, n)
			}
		}
	}
}

func TestRecall_UnknownNotesMapIsEmptyNotAnError(t *testing.T) {
	g, ix := notesGraph(t, "session")
	d := Deps{Resolver: &fakeResolver{label: "team", g: g, ix: ix}, MemoryMap: "session"}
	out, err := recall(context.Background(), d, "", 0)
	if err != nil {
		t.Fatalf("a missing notes map is 'nothing remembered', not a failure: %v", err)
	}
	if out["count"] != 0 {
		t.Fatalf("the resolver's fallback map must not be read as notes, got %v", out)
	}
	if n, _ := out["note"].(string); !strings.Contains(n, `"session"`) {
		t.Fatalf("the note must name the missing map, got %q", n)
	}
	if _, err := recall(context.Background(), Deps{}, "", 0); err == nil {
		t.Fatal("no notes map at all must be an error")
	}
}

// recall reads; it registers wherever a notes map is named, including on a
// read-only server that has no Rememberer, and on the live surface.
func TestRegisterRecall_NeedsOnlyANotesMap(t *testing.T) {
	g, ix := notesGraph(t, "session")
	ro := server.NewMCPServer("t", "0")
	registerRecall(ro, Deps{Resolver: &fakeResolver{label: "session", g: g, ix: ix}, MemoryMap: "session"})
	tool, ok := ro.ListTools()["recall"]
	if !ok {
		t.Fatal("recall missing on a read-only server with a notes map")
	}
	for _, want := range []string{"query", "limit"} {
		if _, has := tool.Tool.InputSchema.Properties[want]; !has {
			t.Errorf("recall must take %q", want)
		}
	}
	if len(tool.Tool.InputSchema.Properties) != 2 {
		t.Fatalf("recall takes query and limit only, got %v", tool.Tool.InputSchema.Properties)
	}
	if !strings.Contains(tool.Tool.Description, "FIRST") || !strings.Contains(tool.Tool.Description, "previous conversation") {
		t.Fatalf("the description is the routing lever, got %q", tool.Tool.Description)
	}
	live := server.NewMCPServer("t", "0")
	registerRecall(live, Deps{Graph: g, Index: ix, MemoryMap: "session"})
	if _, ok := live.ListTools()["recall"]; !ok {
		t.Fatal("recall missing on the live surface")
	}
	none := server.NewMCPServer("t", "0")
	registerRecall(none, Deps{Graph: g, Index: ix})
	if _, ok := none.ListTools()["recall"]; ok {
		t.Fatal("recall registered without a notes map")
	}
	s, err := RecallDirect(context.Background(), Deps{Graph: g, Index: ix, MemoryMap: "session"}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(s), &got); err != nil {
		t.Fatal(err)
	}
	if got["count"] != float64(2) {
		t.Fatalf("direct twin must carry the same fields: %v", got)
	}
}

// The framing sentence is on EVERY recall reply. A voice agent answered "what
// are the FULL expectations" for a student from one remember note and never
// searched the team map again; the reply has to say that notes are what
// was said, not the record. The status sentence that explained an empty reply
// stays verbatim beside it.
func TestRecall_FramingNoteIsAlwaysPresent(t *testing.T) {
	g, ix := notesGraph(t, "session")
	d := Deps{Resolver: &fakeResolver{label: "session", g: g, ix: ix}, MemoryMap: "session"}

	out, err := recall(context.Background(), d, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	note, _ := out["note"].(string)
	if out["count"] != 2 || !strings.Contains(note, recallFraming) {
		t.Fatalf("a non-empty reply must carry the framing, got %q", note)
	}
	if !strings.Contains(note, "newest first unless a query ranked them") {
		t.Fatalf("the ordering sentence must stay beside the framing, got %q", note)
	}
	for _, want := range []string{"not the record", "search that map"} {
		if !strings.Contains(recallFraming, want) {
			t.Errorf("the framing must say notes are %q and to %q, got %q", want, want, recallFraming)
		}
	}

	// A ranked reply is empty only when the map holds no live note (the
	// selection path ranks every member), so the shaping is checked directly.
	note, _ = recallResult("session", "nothing-like-this-zzz", nil, "")["note"].(string)
	if !strings.Contains(note, `no note in "session" matches "nothing-like-this-zzz"; recall with no query lists the newest notes`) || !strings.Contains(note, recallFraming) {
		t.Fatalf("an empty ranked reply must keep its explanation verbatim AND the framing, got %q", note)
	}
	note, _ = recallResult("session", "", nil, "")["note"].(string)
	if !strings.Contains(note, `nothing has been remembered in "session" yet`) || !strings.Contains(note, recallFraming) {
		t.Fatalf("an empty listing must keep its explanation verbatim AND the framing, got %q", note)
	}

	missing := Deps{Resolver: &fakeResolver{label: "team", g: g, ix: ix}, MemoryMap: "session"}
	out, _ = recall(context.Background(), missing, "", 0)
	note, _ = out["note"].(string)
	if !strings.Contains(note, `no notes map "session" on this server yet`) || !strings.Contains(note, recallFraming) {
		t.Fatalf("a missing notes map must keep its explanation verbatim AND the framing, got %q", note)
	}
}

// The descriptions carry the about= contract: remember tells the writer to
// name the record a note is about, recall tells the reader to search the
// record's map first and add the notes on top. Cheap string checks, so a
// rewording cannot silently drop either half.
func TestRecall_DescriptionsNameTheAboutContract(t *testing.T) {
	rd := rememberDesc(Deps{MemoryMap: "session"})
	for _, want := range []string{"about=<the record's title or handle>", "about=octocat42", "never replaces it"} {
		if !strings.Contains(rd, want) {
			t.Errorf("remember description must say %q, got %q", want, rd)
		}
	}
	for _, want := range []string{"search its map first", "about=<name>", "never replaces it"} {
		if !strings.Contains(recallDesc, want) {
			t.Errorf("recall description must say %q, got %q", want, recallDesc)
		}
	}
	s := server.NewMCPServer("t", "0")
	registerRemember(s, Deps{Memory: &fakeMemory{}, MemoryMap: "session"})
	attrs, _ := s.ListTools()["remember"].Tool.InputSchema.Properties["attrs"].(map[string]any)
	if desc, _ := attrs["description"].(string); !strings.Contains(desc, "about=") {
		t.Fatalf("the attrs argument must explain about=, got %v", attrs)
	}
}

// aboutGraph holds a note whose TEXT is all about alpha and one whose text
// never says alpha but whose about attr names it, plus an expired about match
// and one in a foreign repo — neither may surface.
func aboutGraph(t *testing.T) (*graph.Graph, *index.Index) {
	t.Helper()
	at := func(min int) string {
		return time.Date(2026, 9, 16, 10, min, 0, 0, time.UTC).Format("2006-01-02T15:04:05.000Z07:00")
	}
	g := graph.New()
	g.Build(graph.RepoGraph{Repo: "session", Nodes: []parser.NodeInfo{
		{Kind: "Memory", Name: "text", FilePath: "memory/text.md", Content: "alpha alpha routing decided", Body: "alpha alpha routing decided",
			Attrs: map[string]string{"at": at(1)}},
		{Kind: "Memory", Name: "about", FilePath: "memory/about.md", Content: "add GitOps to the course expectations", Body: "add GitOps to the course expectations",
			Attrs: map[string]string{"at": at(2), "about": "@Alpha"}},
		{Kind: "Memory", Name: "expired", FilePath: "memory/expired.md", Content: "an older wish, expired", Body: "an older wish, expired",
			Attrs: map[string]string{"at": at(3), "about": "alpha", "expires": "2020-01-01T00:00:00.000Z"}},
	}}, graph.RepoGraph{Repo: "other", Nodes: []parser.NodeInfo{
		{Kind: "Memory", Name: "foreign", FilePath: "memory/foreign.md", Content: "another map's note", Body: "another map's note",
			Attrs: map[string]string{"at": at(4), "about": "alpha"}},
	}})
	ix := index.New(g, textEmbedder{}, zerolog.Nop())
	if err := ix.Build(context.Background()); err != nil {
		t.Fatal(err)
	}
	return g, ix
}

// A single-token query that IS a note's about handle returns that note first,
// case-insensitively and with or without '@' — even though cosine on the text
// ranks the note that merely says the word above it. A multi-word query is a
// description and keeps rank order.
func TestRecall_AboutMatchRanksFirstForASingleTokenQuery(t *testing.T) {
	g, ix := aboutGraph(t)
	d := Deps{Graph: g, Index: ix, MemoryMap: "session"} // live mode: the foreign repo's about match must stay out

	// Establish that the hoist, not cosine, does the work: raw ranking puts
	// the alpha-text note first.
	hits, err := ix.Search(context.Background(), "alpha", nil, "session", index.Select{Kinds: map[string]bool{"Memory": true}}, 10)
	if err != nil || len(hits) == 0 || hits[0].QN != "session//memory/text.md::text" {
		t.Fatalf("cosine alone must rank the alpha-text note first for this test to prove anything, got %v (%v)", hits, err)
	}

	for _, q := range []string{"alpha", "@alpha", "ALPHA", " Alpha "} {
		out, err := recall(context.Background(), d, q, 0)
		if err != nil {
			t.Fatal(err)
		}
		notes, _ := out["notes"].([]map[string]any)
		if len(notes) != 2 {
			t.Fatalf("query %q: the two live session notes expected (expired and foreign dropped), got %v", q, out)
		}
		if notes[0]["qn"] != "session//memory/about.md::about" || notes[1]["qn"] != "session//memory/text.md::text" {
			t.Fatalf("query %q: the about=%q note must come first, got %v then %v", q, "@Alpha", notes[0]["qn"], notes[1]["qn"])
		}
		if !strings.Contains(out["note"].(string), recallFraming) {
			t.Fatalf("query %q: framing missing", q)
		}
	}

	out, err := recall(context.Background(), d, "alpha routing", 0)
	if err != nil {
		t.Fatal(err)
	}
	notes, _ := out["notes"].([]map[string]any)
	if len(notes) != 2 || notes[0]["qn"] != "session//memory/text.md::text" {
		t.Fatalf("a multi-word query is not a handle and keeps rank order, got %v", notes)
	}

	// Snapshot mode reads the same way through the resolver.
	rd := Deps{Resolver: &fakeResolver{label: "session", g: g, ix: ix}, MemoryMap: "session"}
	out, _ = recall(context.Background(), rd, "alpha", 1)
	notes, _ = out["notes"].([]map[string]any)
	if len(notes) != 1 || notes[0]["qn"] != "session//memory/about.md::about" {
		t.Fatalf("limit=1 must keep the about match, got %v", notes)
	}
}

// The hoist finds an about match the over-fetch never returned: with a limit
// of 1 and the about note ranked below the cut, it is still first.
func TestAboutFirst_GathersAMatchTheHitsMissed(t *testing.T) {
	g, _ := aboutGraph(t)
	text, _ := g.Get("session//memory/text.md::text")
	about, _ := g.Get("session//memory/about.md::about")
	got := aboutFirst(g, "session", "alpha", []*graph.Node{text}, time.Now())
	if len(got) != 2 || got[0] != about || got[1] != text {
		t.Fatalf("the about note must be gathered from the graph and hoisted, got %v", got)
	}
	if got := aboutFirst(g, "session", "alpha routing", []*graph.Node{text}, time.Now()); len(got) != 1 || got[0] != text {
		t.Fatalf("a multi-word query must not hoist, got %v", got)
	}
	if got := aboutFirst(g, "session", "", []*graph.Node{text}, time.Now()); len(got) != 1 {
		t.Fatalf("an empty query must not hoist, got %v", got)
	}
}
