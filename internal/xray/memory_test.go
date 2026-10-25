package xray

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mcell "github.com/triageagent-dev/x-ray/v2/api/v1"
)

func newTestStore() *MemoryStore {
	return NewMemoryStore()
}

func upsertItem(t *testing.T, s *MemoryStore, id, kind, scope, title, content, subject string, tags []string) string {
	t.Helper()
	res, errMsg := s.Upsert(context.Background(), id, kind, scope, title, content, subject, "", "", nil, tags)
	require.Empty(t, errMsg)
	return res.ID
}

// upsertEventItem is upsertItem plus an eventDate, for tests covering the
// event_time reader annotation (docs/todo/graphiti-comparison-refinement.md
// item #1).
func upsertEventItem(t *testing.T, s *MemoryStore, id, kind, scope, title, content, subject, eventDate string) string {
	t.Helper()
	res, errMsg := s.Upsert(context.Background(), id, kind, scope, title, content, subject, "", eventDate, nil, nil)
	require.Empty(t, errMsg)
	return res.ID
}

// seedLinkedItem writes a MemoryItem directly into the ephemeral store with
// an explicit Status and Links — Upsert has no parameter for either (nothing
// in this codebase currently writes ThoughtLinks through Upsert; see
// cellsync.go Persist's comment on Links having no MemoryItem counterpart),
// so tests covering Search's ThoughtLink expansion
// (docs/todo/graphiti-comparison-refinement.md item #4) bypass Upsert the
// same way seedSystemConfig does for its own unsettable field.
func seedLinkedItem(s *MemoryStore, id, kind, scope, title, content, subject, status string, links []mcell.ThoughtLink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[id] = &MemoryItem{
		ID:      id,
		Kind:    kind,
		Scope:   scope,
		Subject: subject,
		Title:   title,
		Content: content,
		Status:  status,
		Links:   links,
	}
}

// seedSystemConfig writes a system_configuration item directly into s.items,
// bypassing the Upsert guard. Use only in tests that verify filtering behaviour.
func seedSystemConfig(s *MemoryStore, id, subject string, tags []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[id] = &MemoryItem{
		ID:      id,
		Kind:    "system_configuration",
		Scope:   "global",
		Subject: subject,
		Title:   "Config",
		Content: "system prompt",
		Status:  "active",
		Tags:    tags,
	}
}

// ── Upsert ────────────────────────────────────────────────────────────────────

func TestMemoryStore_UpsertCreate(t *testing.T) {
	s := newTestStore()
	res, errMsg := s.Upsert(context.Background(), "", "stable_fact", "public", "Hello", "world", "", "", "", nil, nil)
	assert.Empty(t, errMsg)
	assert.NotEmpty(t, res.ID)
	assert.Equal(t, "created", res.Status)
}

func TestMemoryStore_UpsertUpdate(t *testing.T) {
	s := newTestStore()
	id := upsertItem(t, s, "", "stable_fact", "public", "Title", "v1", "", nil)

	res, errMsg := s.Upsert(context.Background(), id, "stable_fact", "public", "Title", "v2", "", "", "", nil, nil)
	assert.Empty(t, errMsg)
	assert.Equal(t, id, res.ID)
	assert.Equal(t, "updated", res.Status)

	it := s.GetItem(id)
	require.NotNil(t, it)
	assert.Equal(t, "v2", it.Content)
}

// TestMemoryStore_UpsertUpdatesCRBackedItem guards the restart case: an item may
// exist only as a CR (seeded by an earlier pod) and never have been written to
// this pod's ephemeral s.items. Upsert-by-id must then report "updated" — not
// "created" — and preserve the original CreatedAt. Regression for the bug where
// status was decided purely from s.items, so any post-restart edit lied "created".
func TestMemoryStore_UpsertUpdatesCRBackedItem(t *testing.T) {
	created := time.Date(2026, 6, 2, 10, 19, 50, 0, time.UTC)
	cell := &mcell.MemoryCell{
		ObjectMeta: metav1.ObjectMeta{Name: "cell-prior", Namespace: "avatar"},
		Spec: mcell.MemoryCellSpec{
			ID: "prior-pod-id", Kind: "system_identity", Scope: "global",
			Subject: "Illia", Title: "Illia", Content: "v1",
			Status: "active", CreatedAt: metav1.NewTime(created),
		},
	}

	scheme := runtime.NewScheme()
	require.NoError(t, mcell.AddToScheme(scheme))
	reader := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cell).
		WithIndex(&mcell.MemoryCell{}, "spec.id", func(o client.Object) []string {
			return []string{o.(*mcell.MemoryCell).Spec.ID}
		}).
		Build()

	s := newTestStore()
	s.SetCacheReader(reader, "avatar")

	res, errMsg := s.Upsert(context.Background(), "prior-pod-id",
		"system_identity", "global", "Illia", "v2", "Illia", "", "", nil, nil)
	require.Empty(t, errMsg)
	assert.Equal(t, "prior-pod-id", res.ID)
	assert.Equal(t, "updated", res.Status, "CR-backed item must report updated, not created")
	require.NotNil(t, res.Item)
	assert.Equal(t, "v2", res.Item.Content)
	assert.True(t, res.Item.CreatedAt.Equal(created),
		"original CreatedAt must be preserved, got %s", res.Item.CreatedAt)
}

// TestMemoryStore_PrivateNoteOwnerScoped guards the SSE privacy model: a private
// note (scope=private) must never reach the shared broadcast that every client
// subscribes to, but it MUST reach its owner's own channel so the note appears
// live in their UI.
func TestMemoryStore_PrivateNoteOwnerScoped(t *testing.T) {
	s := newTestStore()
	shared := s.hub.subscribe()             // anonymous / other clients (empty hash)
	owner := s.hub.subscribeAs("user-hash") // the note's owner
	defer s.hub.unsubscribe(shared)
	defer s.hub.unsubscribe(owner)

	// Control: a normal kind broadcasts an upsert event to all clients.
	upsertItem(t, s, "", "stable_fact", "public", "Public", "shared body", "", nil)
	select {
	case <-shared:
	case <-time.After(time.Second):
		t.Fatal("expected an upsert broadcast for a non-personal item")
	}
	<-owner // owner is also a client, so it receives the shared event too

	// A private note must NOT hit the shared/anonymous channel...
	upsertItem(t, s, "", "note", "private", "Secret", "private meeting plan", "user-hash", nil)
	select {
	case raw := <-shared:
		t.Fatalf("private note must not be broadcast to other clients, got: %s", raw)
	case <-time.After(100 * time.Millisecond):
		// no event — correct.
	}

	// ...but it MUST reach the owner's channel.
	select {
	case raw := <-owner:
		var got MemoryChangeEvent
		require.NoError(t, json.Unmarshal(raw, &got))
		assert.Equal(t, "upsert", got.Op)
		assert.Equal(t, "note", got.Item.Kind)
		assert.Equal(t, "private meeting plan", got.Item.Content)
	case <-time.After(time.Second):
		t.Fatal("owner must receive their own private note over SSE")
	}
}

func TestMemoryStore_UpsertValidation(t *testing.T) {
	s := newTestStore()
	tests := []struct {
		name, kind, scope, content string
	}{
		{"missing kind", "", "public", "body"},
		{"missing scope", "stable_fact", "", "body"},
		{"missing content", "stable_fact", "public", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, errMsg := s.Upsert(context.Background(), "", tt.kind, tt.scope, "t", tt.content, "", "", "", nil, nil)
			assert.NotEmpty(t, errMsg)
		})
	}
}

func TestMemoryStore_UpsertPreservesImageOnEmptyUpdate(t *testing.T) {
	s := newTestStore()
	id := upsertItem(t, s, "", "stable_fact", "public", "T", "body", "", nil)
	// First upsert with image
	imgData := map[string]mcell.DataEntry{"image": {Bytes: "imgdata", Mime: "image/jpeg"}}
	s.Upsert(context.Background(), id, "stable_fact", "public", "T", "body", "", "", "", imgData, nil) //nolint:errcheck

	// Second upsert with nil data — should not wipe existing image
	s.Upsert(context.Background(), id, "stable_fact", "public", "T", "body2", "", "", "", nil, nil) //nolint:errcheck

	it := s.GetItem(id)
	require.NotNil(t, it)
	img, ok := it.Data["image"]
	assert.True(t, ok, "image entry should be preserved when not re-supplied")
	assert.Equal(t, "imgdata", img.Bytes)
}

// ── Get / GetItem ─────────────────────────────────────────────────────────────

func TestMemoryStore_GetItem(t *testing.T) {
	s := newTestStore()
	id := upsertItem(t, s, "", "stable_fact", "public", "T", "body", "", nil)
	it := s.GetItem(id)
	require.NotNil(t, it)
	assert.Equal(t, id, it.ID)
}

func TestMemoryStore_GetMissing(t *testing.T) {
	s := newTestStore()
	assert.Nil(t, s.Get("nonexistent"))
}

// ── Delete ────────────────────────────────────────────────────────────────────

func TestMemoryStore_Delete(t *testing.T) {
	s := newTestStore()
	id := upsertItem(t, s, "", "stable_fact", "public", "T", "body", "", nil)
	assert.True(t, s.Delete(id))
	assert.False(t, s.Delete(id), "second delete should return false")
	assert.Nil(t, s.Get(id))
}

// ── List ──────────────────────────────────────────────────────────────────────

func TestMemoryStore_List(t *testing.T) {
	s := newTestStore()
	upsertItem(t, s, "", "stable_fact", "public", "A", "a", "alice", nil)
	upsertItem(t, s, "", "recent_change", "public", "B", "b", "bob", nil)
	upsertItem(t, s, "", "stable_fact", "public", "C", "c", "alice", nil)

	all := s.List("", "", "", 100)
	assert.Len(t, all, 3)

	byKind := s.List("stable_fact", "", "", 100)
	assert.Len(t, byKind, 2)

	bySubject := s.List("", "", "alice", 100)
	assert.Len(t, bySubject, 2)

	limited := s.List("", "", "", 1)
	assert.Len(t, limited, 1)
}

func TestMemoryStore_List_HidesSystemConfiguration(t *testing.T) {
	s := newTestStore()
	seedSystemConfig(s, "cfg-1", "docs", nil)
	upsertItem(t, s, "", "stable_fact", "public", "Fact", "val", "", nil)

	all := s.List("", "", "", 100)
	assert.Len(t, all, 1, "system_configuration should be hidden from List with no kind filter")

	// system_configuration is reserved internal config — never shared via the
	// List API regardless of scope, so an explicit kind filter must NOT expose it
	// (isPrivateItem treats it as private). It is loaded only via ConfigWatcher.
	cfg := s.List("system_configuration", "", "", 100)
	assert.Empty(t, cfg, "system_configuration must stay hidden even with an explicit kind filter")
}

func TestMemoryStore_List_HidesAgentNotes(t *testing.T) {
	s := newTestStore()
	upsertItem(t, s, "", "note", "private", "Note", "ephemeral", "", nil)
	upsertItem(t, s, "", "stable_fact", "public", "Fact", "durable", "", nil)

	all := s.List("", "", "", 100)
	assert.Len(t, all, 1)
}

// ── Search (keyword mode) ─────────────────────────────────────────────────────

func TestMemoryStore_KeywordSearch(t *testing.T) {
	s := newTestStore()
	upsertItem(t, s, "", "stable_fact", "public", "Go concurrency", "channels and goroutines", "", nil)
	upsertItem(t, s, "", "stable_fact", "public", "Python async", "asyncio and coroutines", "", nil)

	results := s.Search(context.Background(), "goroutine", "", "", 10)
	require.Len(t, results, 1)
	assert.Equal(t, "Go concurrency", results[0].Title)
}

func TestMemoryStore_KeywordSearch_SubjectFilter(t *testing.T) {
	s := newTestStore()
	upsertItem(t, s, "", "stable_fact", "public", "Go", "content", "alice", nil)
	upsertItem(t, s, "", "stable_fact", "public", "Go", "content", "bob", nil)

	results := s.Search(context.Background(), "Go", "", "alice", 10)
	require.Len(t, results, 1)
	assert.Equal(t, "alice", results[0].Subject)
}

func TestMemoryStore_KeywordSearch_EmptyQuery(t *testing.T) {
	s := newTestStore()
	upsertItem(t, s, "", "stable_fact", "public", "T", "body", "", nil)
	assert.Nil(t, s.Search(context.Background(), "", "", "", 10))
}

func TestMemoryStore_KeywordSearch_HidesSystemConfiguration(t *testing.T) {
	s := newTestStore()
	seedSystemConfig(s, "cfg-2", "docs", nil)
	upsertItem(t, s, "", "stable_fact", "public", "Public fact", "open", "", nil)

	results := s.Search(context.Background(), "secret", "", "", 10)
	assert.Len(t, results, 0, "system_configuration must not appear in general search")
}

func TestMemoryStore_KeywordSearch_KindFilter(t *testing.T) {
	s := newTestStore()
	upsertItem(t, s, "", "stable_fact", "public", "Fact one", "alpha", "", nil)
	upsertItem(t, s, "", "recent_change", "public", "Change one", "alpha", "", nil)

	results := s.Search(context.Background(), "alpha", "stable_fact", "", 10)
	require.Len(t, results, 1)
	assert.Equal(t, "stable_fact", results[0].Kind)
}

// ── KeywordSearch (direct) ────────────────────────────────────────────────────

func TestMemoryStore_KeywordSearch_Direct(t *testing.T) {
	s := newTestStore()
	upsertItem(t, s, "", "stable_fact", "public", "Go concurrency", "channels and goroutines", "", nil)
	upsertItem(t, s, "", "stable_fact", "public", "Python async", "asyncio and coroutines", "", nil)

	results := s.KeywordSearch("goroutine", "", "", 10)
	require.Len(t, results, 1)
	assert.Equal(t, "Go concurrency", results[0].Title)
}

// TestMemoryStore_KeywordSearchPerKind_Fairness guards the fix for the graph
// search starving small categories: over a skewed store where one kind vastly
// outnumbers another and BOTH match the query, a flat limit fills with the
// dominant kind and the small kind never surfaces. Per-kind fairness caps each
// kind so the small category still appears. Mirrors the live "persona" case
// (97 events vs 36 concerns all matching, but the graph showed no concerns).
func TestMemoryStore_KeywordSearchPerKind_Fairness(t *testing.T) {
	s := newTestStore()
	// A dominant kind (60 events) and a small kind (10 concerns) all match
	// "persona". Events additionally carry the query word in the TITLE, so they
	// outrank the concerns (query in content only) — reproducing the live skew
	// where the dominant kind fills the top of the ranking.
	for i := 0; i < 60; i++ {
		upsertItem(t, s, "", "event", "public", "persona persona schedule", "persona persona detail", "", nil)
	}
	for i := 0; i < 10; i++ {
		upsertItem(t, s, "", "active_concern", "public", "incident report", "a persona-related concern", "", nil)
	}

	// Flat limit of 20 (no fairness): the 60 higher-ranked events fill all 20
	// slots, so not one concern surfaces.
	flat := s.KeywordSearch("persona", "", "", 20)
	flatConcerns := 0
	for _, it := range flat {
		if it.Kind == "active_concern" {
			flatConcerns++
		}
	}
	assert.Equal(t, 0, flatConcerns, "flat limit must starve concerns (events outrank and fill the limit)")

	// Per-kind fairness: at most 5 per kind → concerns are guaranteed to appear.
	fair := s.KeywordSearchPerKind("persona", "", "", 100, 5)
	fairEvents, fairConcerns := 0, 0
	for _, it := range fair {
		switch it.Kind {
		case "event":
			fairEvents++
		case "active_concern":
			fairConcerns++
		}
	}
	assert.LessOrEqual(t, fairEvents, 5, "events must be capped at perKind")
	assert.Equal(t, 5, fairConcerns, "per-kind fairness must surface concerns the flat limit starved")
}

func TestMemoryStore_KeywordSearch_NoEmbedderNeeded(t *testing.T) {
	// KeywordSearch on a keyword-only store (no embedder) must return results
	// without panicking — confirms it never touches the embedder path.
	s := newTestStore()
	upsertItem(t, s, "", "stable_fact", "public", "Auth middleware", "JWT validation logic", "", nil)
	upsertItem(t, s, "", "stable_fact", "public", "Billing service", "invoice generation", "", nil)

	results := s.KeywordSearch("JWT", "", "", 10)
	require.Len(t, results, 1)
	assert.Equal(t, "Auth middleware", results[0].Title)
}

func TestFuseRRF_OrdersByFusedRank(t *testing.T) {
	a := &MemoryItem{ID: "a", Title: "A"}
	b := &MemoryItem{ID: "b", Title: "B"}
	c := &MemoryItem{ID: "c", Title: "C"}

	// lexical: [a, b]; semantic: [b, c]. b appears in both at good ranks,
	// so RRF must float b above a (rank-0 lex, absent from sem) and c.
	lex := []*MemoryItem{a, b}
	sem := []*MemoryItem{b, c}
	got := fuseRRF(lex, sem, 10)

	require.Len(t, got, 3)
	assert.Equal(t, "b", got[0].ID, "item present in both lists must rank first")
	ids := []string{got[0].ID, got[1].ID, got[2].ID}
	assert.ElementsMatch(t, []string{"a", "b", "c"}, ids, "all items from both lists are fused, deduped")
}

func TestFuseRRF_RespectsLimit(t *testing.T) {
	mk := func(id string) *MemoryItem { return &MemoryItem{ID: id, Title: id} }
	lex := []*MemoryItem{mk("a"), mk("b"), mk("c")}
	sem := []*MemoryItem{mk("d"), mk("e")}
	got := fuseRRF(lex, sem, 2)
	assert.Len(t, got, 2, "fused output must be truncated to limit")
}

func TestEditDistance_Runes(t *testing.T) {
	// editDistance must measure in runes, not bytes. A byte-indexed DP returned a
	// spurious 0 distance for multibyte words like "déjà", causing false matches.
	assert.Equal(t, 0, editDistance("déjà", "déjà"))
	assert.Equal(t, 1, editDistance("déjà", "dejà"), "one accented char differs")
	assert.Greater(t, editDistance("lunchpad", "déjà"), 1, "must not spuriously match a multibyte word")
	// ASCII sanity unchanged.
	assert.Equal(t, 1, editDistance("rac", "rag"))
	assert.Equal(t, 1, editDistance("lunchpad", "launchpad"))
}

func TestFuzzyWordMatch_CamelCaseJoin(t *testing.T) {
	// normalizeQuery splits CamelCase ("LaunchPad" → "launch pad"); a mishear
	// token must still match by rejoining adjacent words.
	assert.True(t, fuzzyWordMatch("launch pad", "lunchpad"), "rejoined CamelCase split must match the mishear")
	assert.False(t, fuzzyWordMatch("blog when déjà vu strategy", "lunchpad"), "unrelated multibyte words must not match")
}

func TestMemoryStore_KeywordSearch_UnderscoreInContent(t *testing.T) {
	s := newTestStore()
	upsertItem(t, s, "", "system_identity", "global", "Auth Service", "handles JWT token validation", "docs", nil)

	assert.Len(t, s.KeywordSearch("JWT_token", "", "", 10), 1, "underscore in content query must match")
}

func TestMemoryStore_Search_KeywordFirstFallsBackToSemantic(t *testing.T) {
	// When keyword search returns < 2 results and no embedder is configured,
	// Search falls back to keyword (same result) — no panic, no nil deref.
	s := newTestStore()
	upsertItem(t, s, "", "stable_fact", "public", "Unique item", "only one match here", "", nil)
	upsertItem(t, s, "", "stable_fact", "public", "Unrelated item", "something else entirely", "", nil)

	// Keyword finds exactly 1 result → triggers semantic fallback path → but no
	// embedder configured → falls back to keyword again → same 1 result returned.
	results := s.Search(context.Background(), "Unique", "", "", 10)
	require.Len(t, results, 1)
	assert.Equal(t, "Unique item", results[0].Title)
}

func TestMemoryStore_Search_SkipsSemanticWhenKeywordSufficient(t *testing.T) {
	// When keyword search returns >= 2 results, Search must return them directly
	// without calling the embedder (which would panic on a keyword-only store
	// if s.cells were non-nil — here we verify the count gate works in keyword mode).
	s := newTestStore()
	upsertItem(t, s, "", "stable_fact", "public", "Auth service", "authentication logic", "", nil)
	upsertItem(t, s, "", "stable_fact", "public", "Auth middleware", "authentication middleware", "", nil)
	upsertItem(t, s, "", "stable_fact", "public", "Billing service", "invoice generation", "", nil)

	results := s.Search(context.Background(), "Auth", "", "", 10)
	assert.GreaterOrEqual(t, len(results), 2, "keyword returned >=2, semantic path must not be needed")
}

// TestShouldShortCircuit_TruthTable is a unit test of the decision itself
// (shouldShortCircuit) rather than of Search end-to-end: the test suite has
// no fake/stub embedder that lets a MemoryStore exercise the fused path with
// s.cells != nil — a keyword-only store panics if s.cells were non-nil (see
// TestMemoryStore_Search_SkipsSemanticWhenKeywordSufficient's comment), and
// vectorstore's own tests seed vectors directly rather than faking the
// embedder's HTTP round-trip — so the short-circuit decision is exercised
// directly against its full truth table, plus a check that
// SetLexicalShortCircuit(on) drives the store's alwaysFuse field with the
// expected (inverted) polarity.
func TestShouldShortCircuit_TruthTable(t *testing.T) {
	cases := []struct {
		name                               string
		hasCells, lexConfident, alwaysFuse bool
		want                               bool
	}{
		{"no cells, not confident, short-circuit on", false, false, false, true},
		{"no cells, confident, short-circuit on", false, true, false, true},
		{"no cells, not confident, short-circuit off", false, false, true, true},
		{"no cells, confident, short-circuit off", false, true, true, true},
		{"cells, not confident, short-circuit on: fuse", true, false, false, false},
		{"cells, confident, short-circuit on: lexical wins", true, true, false, true},
		{"cells, not confident, short-circuit off: fuse", true, false, true, false},
		{"cells, confident, short-circuit off: fuse anyway", true, true, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, shouldShortCircuit(c.hasCells, c.lexConfident, c.alwaysFuse))
		})
	}

	s := newTestStore()
	assert.False(t, s.alwaysFuse.Load(), "short-circuit is on (alwaysFuse=false) by default")
	s.SetLexicalShortCircuit(false)
	assert.True(t, s.alwaysFuse.Load(), "SetLexicalShortCircuit(false) forces always-fuse")
	s.SetLexicalShortCircuit(true)
	assert.False(t, s.alwaysFuse.Load(), "SetLexicalShortCircuit(true) restores the short-circuit")
}

// ── AllItems / KnowledgeIndex ─────────────────────────────────────────────────

func TestMemoryStore_AllItems(t *testing.T) {
	s := newTestStore()
	upsertItem(t, s, "", "stable_fact", "public", "T1", "b", "alice", nil)
	upsertItem(t, s, "", "stable_fact", "public", "T2", "b", "bob", nil)
	seedSystemConfig(s, "cfg-3", "docs", nil)

	all := s.AllItems("alice")
	assert.Len(t, all, 1)
	assert.Empty(t, s.AllItems("nobody"))
}

func TestMemoryStore_KnowledgeIndex_DeduplicatesTitles(t *testing.T) {
	s := newTestStore()
	upsertItem(t, s, "", "stable_fact", "public", "Shared title", "v1", "", nil)
	upsertItem(t, s, "", "stable_fact", "public", "Shared title", "v2", "", nil)
	upsertItem(t, s, "", "stable_fact", "public", "Unique title", "v3", "", nil)

	idx := s.KnowledgeIndex("")
	assert.Len(t, idx, 2, "duplicate titles should be deduplicated")
}

// ── Concurrency ───────────────────────────────────────────────────────────────

func TestMemoryStore_ConcurrentUpsert(t *testing.T) {
	s := newTestStore()
	s.SetWriteDedup(false) // fifty identical writes must stay fifty cells here
	var wg sync.WaitGroup
	const n = 50
	ids := make([]string, n)
	var mu sync.Mutex

	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, _ := s.Upsert(context.Background(), "", "stable_fact", "public", "T", "body", "", "", "", nil, nil)
			mu.Lock()
			ids[i] = res.ID
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	seen := make(map[string]struct{})
	for _, id := range ids {
		assert.NotEmpty(t, id)
		seen[id] = struct{}{}
	}
	assert.Len(t, seen, n, "each concurrent upsert should produce a unique ID")
}

// ── private note tenant isolation ─────────────────────────────────────────────

// TestSearchMemoryNoAgentNoteLeakAcrossUsers verifies that a private note
// belonging to user A is never returned when searching as user B (kind=""
// general search).
func TestSearchMemoryNoAgentNoteLeakAcrossUsers(t *testing.T) {
	s := newTestStore()
	userA := stableUserID("azure:user-a-oid")
	userB := stableUserID("azure:user-b-oid")

	// User A has a personal note (scope=private).
	upsertItem(t, s, "", "note", "private", "Meeting note", "discussed project timelines", userA, nil)
	// Some shared knowledge both users can see.
	upsertItem(t, s, "", "stable_fact", "public", "Shared fact", "discussed project timelines", "", nil)

	// subject=userB search — A's note must not leak; userB has no own items so result is empty.
	results := s.Search(context.Background(), "discussed", "", userB, 10)
	for _, r := range results {
		assert.NotEqual(t, "private", r.Scope, "private notes must never leak through general search")
		assert.NotEqual(t, userA, r.Subject, "userA's items must not appear in userB search")
	}

	// subject="" (no filter) — shared fact IS reachable, the private note must still be hidden.
	all := s.Search(context.Background(), "discussed", "", "", 10)
	allTitles := make([]string, len(all))
	for i, r := range all {
		allTitles[i] = r.Title
		assert.NotEqual(t, "private", r.Scope, "private notes must not appear in kind-agnostic search")
	}
	assert.Contains(t, allTitles, "Shared fact", "shared stable_fact must appear in subject-agnostic search")

	// Explicit kind=note + correct subject — user A can find own note.
	own := s.Search(context.Background(), "discussed", "note", userA, 10)
	require.Len(t, own, 1)
	assert.Equal(t, userA, own[0].Subject)

	// Storage-layer privacy: a private cell is hidden whenever subject is empty,
	// regardless of the requested kind. The tenant boundary is enforced by the
	// scope=private guard, so callers cannot reach another user's note by passing
	// kind=note with an empty subject.
	unscoped := s.Search(context.Background(), "discussed", "note", "", 10)
	require.Empty(t, unscoped, "private note must not be returned when subject is empty")
}

// TestKeywordSearchMapSubjectEquality verifies that subject filtering uses exact
// equality, not substring match (e.g. "abc" must not match "abcd").
func TestKeywordSearchMapSubjectEquality(t *testing.T) {
	s := newTestStore()
	short := stableUserID("azure:short")
	// Craft a subject that is a prefix of `short` to simulate the old Contains bug.
	prefix := short[:8]

	upsertItem(t, s, "", "note", "private", "Private note", "secret content", short, nil)

	// Search with the prefix subject — must return nothing (exact match required).
	results := s.KeywordSearch("secret", "note", prefix, 10)
	assert.Empty(t, results, "partial subject match must not return items")

	// Exact subject — must return the note.
	exact := s.KeywordSearch("secret", "note", short, 10)
	assert.Len(t, exact, 1)
}

// ── debounced stats flusher (ADR-047) ────────────────────────────────────────

// TestStatsFlusher_SearchMarksDirtyNotFlushSync proves the hot path only flips a
// flag — it does NOT write synchronously. N searches collapse to one pending
// flush, not N etcd writes.
func TestStatsFlusher_SearchMarksDirtyNotFlushSync(t *testing.T) {
	s := newTestStore()
	assert.False(t, s.statsDirty.Load(), "fresh store starts clean")

	for range 10 {
		s.Search(context.Background(), "anything", "", "", 5)
	}
	// All ten searches set the same flag — coalesced, not ten writes.
	assert.True(t, s.statsDirty.Load(), "search marks stats dirty")
}

// TestStatsFlusher_IdleStaysClean: a store with no activity never becomes dirty,
// so the flusher makes zero CR writes when idle.
func TestStatsFlusher_IdleStaysClean(t *testing.T) {
	s := newTestStore()
	assert.False(t, s.statsDirty.Load(), "no activity → nothing to flush")
}

// TestStatsFlusher_VoiceAndFlushStatsMarkDirty: voice tracking and the exported
// FlushStats (wired to login/logout) both go through the dirty flag.
func TestStatsFlusher_VoiceAndFlushStatsMarkDirty(t *testing.T) {
	s := newTestStore()
	s.TrackVoiceCall()
	assert.True(t, s.statsDirty.Load(), "voice call marks dirty")

	s.statsDirty.Store(false)
	s.FlushStats() // exported entry point used by the session store
	assert.True(t, s.statsDirty.Load(), "FlushStats marks dirty (no synchronous write)")
}

// TestStatsFlusher_TickConsumesDirty: the background loop clears the flag when it
// flushes. With no writer wired, flushStats is a no-op, but the Swap still runs —
// so after a tick the store is clean again and an idle next tick does nothing.
func TestStatsFlusher_TickConsumesDirty(t *testing.T) {
	s := newTestStore()
	ctx, cancel := context.WithCancel(context.Background())
	s.markStatsDirty()
	require.True(t, s.statsDirty.Load())

	// Fast tick so the test is quick; the loop Swaps the flag to false on flush.
	s.StartStatsFlusher(ctx, 10*time.Millisecond)
	assert.Eventually(t, func() bool { return !s.statsDirty.Load() }, time.Second, 5*time.Millisecond,
		"flusher tick consumes the dirty flag")
	cancel()
}

// TestStatsFlusher_DisabledOnZeroInterval: a non-positive interval starts no loop.
func TestStatsFlusher_DisabledOnZeroInterval(t *testing.T) {
	s := newTestStore()
	s.markStatsDirty()
	s.StartStatsFlusher(context.Background(), 0) // disabled
	// Nothing consumes the flag — it stays set (loop never started).
	assert.True(t, s.statsDirty.Load(), "zero interval disables the flusher")
}
