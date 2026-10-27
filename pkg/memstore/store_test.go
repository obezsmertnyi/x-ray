package memstore

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	memoryv1 "github.com/triageagent-dev/x-ray/v2/api/v1"
)

func mk(id, kind, title, content string, tags ...string) Item {
	return Item{MemoryCellSpec: memoryv1.MemoryCellSpec{
		ID: id, Kind: kind, Scope: ScopePublic, Title: title, Content: content, Tags: tags,
		Confidence: "high", Status: "active",
	}}
}

func must(t *testing.T, it Item, msg string) Item {
	t.Helper()
	require.Empty(t, msg)
	return it
}

func upsert(t *testing.T, s *Store, it Item) Item {
	t.Helper()
	got, msg := s.Upsert(it)
	return must(t, got, msg)
}

func ids(items []Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.ID
	}
	return out
}

// ── construction and seams ─────────────────────────────────────────────────

// countingLock records how often the store takes the host's lock.
type countingLock struct {
	sync.RWMutex
	locks, rlocks int
}

func (c *countingLock) Lock()  { c.RWMutex.Lock(); c.locks++ }
func (c *countingLock) RLock() { c.RWMutex.RLock(); c.rlocks++ }

func TestStore_UsesTheHostsLockAndRunsLockedHooksUnderIt(t *testing.T) {
	lock := &countingLock{}
	var beforeSawLocked bool
	s := NewStore(WithLocker(lock), WithConfig(Config{
		Before: func(it *Item) {
			// The write lock is held here: a try-lock from inside must fail.
			beforeSawLocked = !lock.RWMutex.TryRLock()
			if !beforeSawLocked {
				lock.RWMutex.RUnlock()
			}
		},
	}))
	upsert(t, s, mk("a", KindStableFact, "t", "c"))
	assert.True(t, beforeSawLocked, "Before runs with the host lock held")
	assert.Positive(t, lock.locks)

	_, _ = s.Get("a")
	assert.Positive(t, lock.rlocks, "reads take the read lock")
}

func TestStore_DirtySignalAndPublishedFireOnWrites(t *testing.T) {
	var dirty, signal, published int
	var changes []string
	s := NewStore(WithConfig(Config{
		Dirty: func() { dirty++ }, Signal: func() { signal++ }, Published: func() { published++ },
		Quiet: func(id string) bool { return id == "quiet" },
	}))
	s.SetChangeHook(func(op string, it *Item, id, kind string) {
		if it != nil {
			id = it.ID
		}
		changes = append(changes, op+":"+id+":"+kind)
	})
	upsert(t, s, mk("a", KindStableFact, "t", "c"))
	upsert(t, s, mk("quiet", KindStableFact, "t", "c"))
	assert.GreaterOrEqual(t, dirty, 2)
	assert.GreaterOrEqual(t, signal, 2)
	assert.Equal(t, 1, published, "a quiet id is not published")
	assert.Equal(t, []string{"upsert:a:stable_fact"}, changes)

	require.True(t, s.Delete("a"))
	assert.Equal(t, []string{"upsert:a:stable_fact", "delete:a:stable_fact"}, changes, "a delete carries the kind captured before it")
	assert.False(t, s.Delete("a"), "deleting nothing reports false")
}

func TestStore_ReadHookAmendsEveryReturnedItem(t *testing.T) {
	s := NewStore(WithConfig(Config{Read: func(items []Item) {
		for i := range items {
			items[i].Title = "read:" + items[i].Title
		}
	}}))
	upsert(t, s, mk("a", KindStableFact, "t", "c"))
	got, _ := s.Get("a")
	assert.Equal(t, "read:t", got.Title)
	assert.Equal(t, "read:t", s.Items()[0].Title)
	assert.Equal(t, "read:t", s.Filtered("", "", "", "", "", 0)[0].Title)
	stored, _ := s.Get("a")
	assert.Equal(t, "read:t", stored.Title)
	assert.Equal(t, "t", s.ItemsLocked()[0].Title, "the stored item is untouched")
}

// ── validation, vocabulary and coercion ──────────────────────────────────

func TestStore_ValidateBaseRules(t *testing.T) {
	s := NewStore()
	cases := []struct {
		name string
		it   Item
		want string
	}{
		{"kind", mk("a", "", "t", "c"), "kind is required"},
		{"content", mk("a", "note", "t", " "), "content is required"},
		{"scope", func() Item { i := mk("a", "note", "t", "c"); i.Scope = ""; return i }(), "scope is required"},
		{"origin", func() Item { i := mk("a", "note", "t", "c"); i.Origin = "bogus"; return i }(), "origin must be one of"},
		{"stable fact confidence", func() Item { i := mk("a", KindStableFact, "t", "c"); i.Confidence = "low"; return i }(), "stable_fact items must have confidence=high"},
		{"mindmap needs a deadline", mk("a", "mindmap", "t", "c"), "must have stale_after"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Contains(t, s.ValidateBase(c.it), c.want)
			_, msg := s.Upsert(c.it)
			assert.Contains(t, msg, c.want, "Upsert refuses the same item")
		})
	}
	assert.Empty(t, s.ValidateBase(mk("a", "note", "t", "c")))
}

func TestStore_VocabularyRejectsCoercesAndHidesKinds(t *testing.T) {
	voc := Vocabulary{
		Rejected:      []string{"policy"},
		NonSearchable: []string{"identity"},
		Coerce: func(it *Item) {
			if it.Kind == "reserved" {
				it.Kind = "note"
				it.Tags = append(it.Tags, "was-reserved")
			}
		},
	}
	s := NewStore(WithConfig(Config{Vocabulary: voc}))

	_, msg := s.Upsert(mk("p", "policy", "t", "c"))
	assert.Contains(t, msg, "cannot be written")

	got := upsert(t, s, mk("r", "reserved", "t", "c"))
	assert.Equal(t, "note", got.Kind, "the host rewrites the kind before validation")
	assert.Contains(t, got.Tags, "was-reserved")

	upsert(t, s, mk("i", "identity", "needle", "prompt text"))
	upsert(t, s, mk("n", "note", "needle", "plain text"))
	assert.Equal(t, []string{"n"}, ids(s.KeywordSearch("needle", "", "", 10)), "a non-searchable kind is left out of an unfiltered search")
	assert.Equal(t, []string{"i"}, ids(s.KeywordSearch("needle", "identity", "", 10)), "but an explicit filter finds it")

	assert.True(t, voc.IsRejected("policy"))
	assert.False(t, Vocabulary{}.IsRejected("policy"))
	assert.False(t, Vocabulary{}.IsSearchable(""), "an unset kind is never searchable")
	assert.True(t, Vocabulary{}.IsSearchable("anything"))
	assert.False(t, Vocabulary{Searchable: []string{"a"}}.IsSearchable("b"))
}

func TestStore_HostValidateRunsAfterTheBaseRules(t *testing.T) {
	s := NewStore(WithConfig(Config{Validate: func(it Item) string {
		if strings.Contains(it.Content, "forbidden") {
			return "host says no"
		}
		return ""
	}}))
	_, msg := s.Upsert(mk("a", "note", "t", "forbidden words"))
	assert.Equal(t, "host says no", msg)
	_, msg = s.Upsert(mk("b", "note", "t", " "))
	assert.Equal(t, "content is required", msg, "the base rules answer first")
}

// ── retention ──────────────────────────────────────────────────────────

func TestStore_RetentionStampsKnowledgeByPartition(t *testing.T) {
	s := NewStore(WithConfig(Config{Retention: func(p string) int {
		switch p {
		case "short":
			return 1
		case "none":
			return 0
		}
		return 30
	}}))
	near := func(got *metav1.Time, d time.Duration) {
		t.Helper()
		require.NotNil(t, got)
		assert.WithinDuration(t, time.Now().Add(d), got.Time, 5*time.Second)
	}
	near(upsert(t, s, mk("a", KindStableFact, "t", "c")).StaleAfter, 30*24*time.Hour)
	near(upsert(t, s, mk("b", KindOperationalPattern, "t", "c", "tenant/short")).StaleAfter, 24*time.Hour)
	assert.Nil(t, upsert(t, s, mk("c", KindStableFact, "t", "c", "tenant/none")).StaleAfter, "0 days means no deadline")
	assert.Nil(t, upsert(t, s, mk("d", "note", "t", "c")).StaleAfter, "other kinds are untouched")

	hyp := mk("h", KindHypothesis, "t", "c")
	near(upsert(t, s, hyp).StaleAfter, 24*time.Hour)
	unk := mk("u", KindUnknown, "t", "c")
	near(upsert(t, s, unk).StaleAfter, 7*24*time.Hour)

	explicit := mk("e", KindStableFact, "t", "c")
	at := metav1.NewTime(time.Now().Add(time.Hour))
	explicit.StaleAfter = &at
	near(upsert(t, s, explicit).StaleAfter, time.Hour)
}

func TestStore_StaleOnReadMarksExpiredItems(t *testing.T) {
	s := NewStore()
	past := metav1.NewTime(time.Now().Add(-time.Minute))
	it := mk("a", "note", "t", "c")
	it.StaleAfter = &past
	upsert(t, s, it)
	upsert(t, s, mk("b", "note", "t", "c"))

	assert.Equal(t, []string{"b"}, ids(s.Filtered("active", "", "", "", "", 0)), "the expired item is no longer active")
	got, _ := s.Get("a")
	assert.Equal(t, "stale", got.Status)
	assert.Equal(t, []string{"a"}, ids(s.Filtered("stale", "", "", "", "", 0)))
}

// ── write-time merge ──────────────────────────────────────────────────

func TestStore_UpsertDedupMergesARestatedFact(t *testing.T) {
	var before []string
	s := NewStore(WithConfig(Config{Before: func(it *Item) { before = append(before, it.ID) }}))
	first := mk("keep", KindStableFact, "Cache layer evicts idle entries",
		"The cache evicts entries after ten minutes of silence and logs each eviction at debug level.", "service/cache")
	first.Links = []memoryv1.ThoughtLink{{ToID: "other", Relation: "explains", Weight: 0.5}}
	stored := upsert(t, s, first)

	restated := mk("new", KindStableFact, "Cache layer evicts idle entries",
		"The cache evicts entries after ten minutes of silence and logs every eviction at debug level.", "service/cache", "extra")
	got, mergedInto, msg := s.UpsertDedup(restated)
	require.Empty(t, msg)
	assert.Equal(t, "keep", mergedInto)
	assert.Equal(t, "keep", got.ID)
	assert.Equal(t, stored.CreatedAt, got.CreatedAt, "the creation time is kept")
	assert.Equal(t, first.Links, got.Links, "the links are kept")
	assert.Equal(t, []string{"service/cache", "extra"}, got.Tags, "tags are unioned, the existing ones first")
	assert.Len(t, s.Items(), 1)
	assert.Equal(t, []string{"keep", "keep"}, before, "Before runs on the merged item")

	other := mk("distinct", KindStableFact, "Billing export schedule", "The export job runs at four in the morning every weekday.", "service/cache")
	_, mergedInto, msg = s.UpsertDedup(other)
	require.Empty(t, msg)
	assert.Empty(t, mergedInto, "a different fact lands as its own item")
	assert.Len(t, s.Items(), 2)
}

func TestStore_UpsertDedupNeverMergesIntoSharedOrAuthoredCells(t *testing.T) {
	s := NewStore()
	body := "The queue drains every minute and the oldest message is never older than two minutes in normal operation."
	shared := mk("shared", KindStableFact, "Queue drain cadence", body, SharedTag)
	upsert(t, s, shared)
	got, mergedInto, msg := s.UpsertDedup(mk("again", KindStableFact, "Queue drain cadence", body))
	require.Empty(t, msg)
	assert.Empty(t, mergedInto)
	assert.Equal(t, "again", got.ID)

	authored := mk("authored", KindStableFact, "Gateway timeout budget", "The gateway gives each upstream call five seconds and retries once on a connection reset.", "author/alice")
	upsert(t, s, authored)
	_, mergedInto, msg = s.UpsertDedup(mk("nope", KindStableFact, "Gateway timeout budget", "The gateway gives each upstream call five seconds and retries once on a connection reset."))
	require.Empty(t, msg)
	assert.Empty(t, mergedInto, "an unauthored restatement never folds into an authored cell")
}

func TestStore_UpsertDedupSkipsNonKnowledgeKindsAndNamedIDs(t *testing.T) {
	s := NewStore()
	upsert(t, s, mk("a", "note", "Same title here", "same body text for both of them in this note"))
	_, mergedInto, _ := s.UpsertDedup(mk("b", "note", "Same title here", "same body text for both of them in this note"))
	assert.Empty(t, mergedInto, "notes are not deduplicated")

	fact := "Pool size is fixed at twenty connections for every worker process on the host machine."
	upsert(t, s, mk("f1", KindStableFact, "Pool size", fact))
	got, mergedInto, _ := s.UpsertDedup(mk("f1", KindStableFact, "Pool size", fact+" Updated."))
	assert.Empty(t, mergedInto, "naming an existing id is an update, not a merge")
	assert.Equal(t, "f1", got.ID)
}

// ── origin ─────────────────────────────────────────────────────────────

func TestStore_ADerivedWriteNeverReplacesAHumanCell(t *testing.T) {
	s := NewStore()
	h := mk("a", "note", "t", "written by a person")
	h.Origin = OriginHuman
	upsert(t, s, h)

	d := mk("a", "note", "t", "a model rewrites it")
	d.Origin = OriginDerived
	_, msg := s.Upsert(d)
	assert.Equal(t, errDerivedReplacesHuman, msg)
	got, _ := s.Get("a")
	assert.Equal(t, "written by a person", got.Content, "the refused write changed nothing")
}

func TestStore_OriginOfAnEditKeepsTheStoredOne(t *testing.T) {
	s := NewStore()
	h := mk("a", "note", "t", "written by a person")
	h.Origin = OriginHuman
	upsert(t, s, h)
	got := upsert(t, s, mk("a", "note", "t", "edited"))
	assert.Equal(t, OriginHuman, got.Origin, "an empty incoming origin keeps the stored one")

	d := mk("a", "note", "t", "edited by a model")
	d.Origin = OriginDerived
	_, msg := s.Upsert(d)
	assert.Equal(t, errDerivedReplacesHuman, msg)
}

func TestStore_MirrorKeepsOriginAndSkipsNoOps(t *testing.T) {
	var changes int
	s := NewStore()
	s.SetChangeHook(func(string, *Item, string, string) { changes++ })

	cell := mk("a", "note", "t", "from a cell")
	cell.Tags = []string{"author/alice"}
	cell.UpdatedAt = metav1.NewTime(time.Now().Add(-time.Hour))
	assert.True(t, s.Mirror(cell))
	got, _ := s.Get("a")
	assert.Equal(t, OriginHuman, got.Origin, "an author tag restores a human origin")
	assert.Equal(t, 1, changes)

	assert.False(t, s.Mirror(cell), "an identical cell is not a change")
	assert.Equal(t, 1, changes)

	older := cell
	older.Content = "an older edit"
	older.UpdatedAt = metav1.NewTime(time.Now().Add(-2 * time.Hour))
	assert.False(t, s.Mirror(older), "a cell older than the in-memory item never clobbers it")

	newer := cell
	newer.Content = "a real external edit"
	newer.UpdatedAt = metav1.NewTime(time.Now())
	assert.True(t, s.Mirror(newer))
	assert.Equal(t, 2, changes)

	assert.False(t, s.Mirror(Item{}), "an item without an id is ignored")
}

// ── private scope ───────────────────────────────────────────────────────

func TestStore_PrivateItemsReachOnlyTheirOwner(t *testing.T) {
	s := NewStore()
	upsert(t, s, mk("pub", "note", "t", "shared"))
	priv := mk("mine", "note", "t", "secret", PrivateOwnerTagPrefix+"alice")
	priv.Scope = ScopePrivate
	upsert(t, s, priv)

	assert.Equal(t, []string{"pub"}, ids(s.Items()))
	assert.Equal(t, []string{"pub"}, ids(s.Filtered("", "", "", "", "", 0)))
	assert.ElementsMatch(t, []string{"pub", "mine"}, ids(s.ForUser("alice", "", "", "", "", "", 0)))
	assert.Equal(t, []string{"pub"}, ids(s.ForUser("bob", "", "", "", "", "", 0)))
	assert.Equal(t, []string{"pub"}, ids(s.ForUser("", "", "", "", "", "", 0)))
	assert.ElementsMatch(t, []string{"pub", "mine"}, ids(s.AllItems()), "AllItems is the internal exception")
	assert.False(t, s.Indexed("mine"), "a private item is never in the shared lexical index")
	assert.Empty(t, s.KeywordSearch("secret", "", "", 10))
}

// ── links ────────────────────────────────────────────────────────────

func TestThoughtLinkRejection(t *testing.T) {
	assert.Contains(t, ThoughtLinkRejection("a", memoryv1.ThoughtLink{ToID: "", Relation: "explains"}), "empty toId")
	msg := ThoughtLinkRejection("a", memoryv1.ThoughtLink{ToID: "b", Relation: "likes"})
	assert.Contains(t, msg, `relation "likes" is not valid`)
	assert.Contains(t, msg, "triggers, caused_by, correlates_with, precedes, explains, contradicts")
	assert.Contains(t, ThoughtLinkRejection("a", memoryv1.ThoughtLink{ToID: "a", Relation: "explains"}), "self-link")
	assert.Empty(t, ThoughtLinkRejection("a", memoryv1.ThoughtLink{ToID: "b", Relation: "explains"}))
}

func TestStore_AddLinkRulesCapAndEviction(t *testing.T) {
	s := NewStore()
	upsert(t, s, mk("a", "note", "t", "c"))

	assert.False(t, s.AddLink("missing", memoryv1.ThoughtLink{ToID: "b", Relation: "explains", Weight: 1}), "a missing source is refused")
	assert.False(t, s.AddLink("a", memoryv1.ThoughtLink{ToID: "a", Relation: "explains", Weight: 1}), "a self-link is refused")
	assert.False(t, s.AddLink("a", memoryv1.ThoughtLink{ToID: "b", Relation: "bogus", Weight: 1}), "an unknown relation is refused")

	for i := 0; i < MaxModelLinks; i++ {
		require.True(t, s.AddLink("a", memoryv1.ThoughtLink{ToID: fmt.Sprintf("t%02d", i), Relation: "explains", Weight: float32(i+1) / 100}))
	}
	it, _ := s.Get("a")
	require.Len(t, it.Links, MaxModelLinks)

	assert.False(t, s.AddLink("a", memoryv1.ThoughtLink{ToID: "weak", Relation: "explains", Weight: 0.001}), "past the cap a link must out-weigh the lowest")
	assert.True(t, s.AddLink("a", memoryv1.ThoughtLink{ToID: "strong", Relation: "explains", Weight: 0.9}))
	it, _ = s.Get("a")
	require.Len(t, it.Links, MaxModelLinks)
	var targets []string
	for _, l := range it.Links {
		targets = append(targets, l.ToID)
	}
	assert.Contains(t, targets, "strong")
	assert.NotContains(t, targets, "t00", "the lowest-weight link was evicted")

	assert.True(t, s.AddLink("a", memoryv1.ThoughtLink{ToID: "strong", Relation: "explains", Weight: 0.95}), "the same target and relation replaces, exempt from the cap")
	it, _ = s.Get("a")
	assert.Len(t, it.Links, MaxModelLinks)
}

func TestStore_UpsertKeepsLinksUnlessLinksSet(t *testing.T) {
	s := NewStore()
	upsert(t, s, mk("a", "note", "t", "c"))
	require.True(t, s.AddLink("a", memoryv1.ThoughtLink{ToID: "b", Relation: "explains", Weight: 1}))

	got := upsert(t, s, mk("a", "note", "t", "edited"))
	assert.Len(t, got.Links, 1, "a content edit never wipes a link")

	set := mk("a", "note", "t", "edited again")
	set.LinksSet = true
	got = upsert(t, s, set)
	assert.Empty(t, got.Links, "an authoritative empty set clears them")
}

func TestStore_PruneInvalidLinksDropsOnlyBrokenOnes(t *testing.T) {
	s := NewStore()
	broken := mk("a", "note", "t", "c")
	broken.Links = []memoryv1.ThoughtLink{
		{ToID: "", Relation: "explains"}, {ToID: "a", Relation: "explains"}, {ToID: "elsewhere", Relation: "explains", Weight: 1},
	}
	s.Put(broken)
	var signals int
	s.cfg.Signal = func() { signals++ }

	assert.Equal(t, 2, s.PruneInvalidLinks())
	got, _ := s.Get("a")
	require.Len(t, got.Links, 1)
	assert.Equal(t, "elsewhere", got.Links[0].ToID, "a target the store does not hold is not a broken link")
	assert.Equal(t, 1, signals)
	assert.Zero(t, s.PruneInvalidLinks())
	assert.Equal(t, 1, signals, "nothing removed, nothing signalled")
}

// ── lifecycle ──────────────────────────────────────────────────────────

func TestStore_ExpireAndDeleteLeaveTheLexicalIndex(t *testing.T) {
	s := NewStore()
	upsert(t, s, mk("a", "note", "needle", "c"))
	upsert(t, s, mk("b", "note", "needle", "c"))
	require.True(t, s.Indexed("a"))

	assert.True(t, s.Expire("a"))
	assert.False(t, s.Indexed("a"))
	assert.True(t, s.Delete("b"))
	assert.False(t, s.Indexed("b"))
	assert.Empty(t, s.KeywordSearch("needle", "", "", 10))
	assert.False(t, s.Expire("missing"))

	var ops []string
	s.SetChangeHook(func(op string, it *Item, id, kind string) { ops = append(ops, op) })
	upsert(t, s, mk("c", "note", "x", "c"))
	require.True(t, s.Expire("c"))
	assert.Equal(t, []string{"upsert", "delete"}, ops, "an expiry is published as a delete")
}

func TestStore_SupersedeRetiresTheOldAndLinksTheNew(t *testing.T) {
	s := NewStore()
	upsert(t, s, mk("old", KindStableFact, "t", "old text"))
	got, msg := s.Supersede("old", mk("new", KindStableFact, "t", "new text"))
	require.Empty(t, msg)
	assert.Equal(t, []string{"old"}, got.Supersedes)
	old, _ := s.Get("old")
	assert.Equal(t, "superseded", old.Status)
	assert.False(t, s.Indexed("old"))
	assert.True(t, s.Indexed("new"))

	_, msg = s.Supersede("nothing", mk("x", "note", "t", "c"))
	assert.Equal(t, "not_found", msg)

	h := mk("human", "note", "t", "a person's note")
	h.Origin = OriginHuman
	upsert(t, s, h)
	d := mk("model", "note", "t", "a model's replacement")
	d.Origin = OriginDerived
	_, msg = s.Supersede("human", d)
	assert.Equal(t, errDerivedReplacesHuman, msg)
}

func TestStore_RefuteCascadesOneHopToDerivedCitations(t *testing.T) {
	s := NewStore()
	upsert(t, s, mk("root", "note", "t", "claim"))
	cites := mk("cites", "note", "t", "drawn from root")
	cites.EvidenceRefs = []string{"cell:root"}
	upsert(t, s, cites)
	linked := mk("linked", "note", "t", "links to root")
	linked.Links = []memoryv1.ThoughtLink{{ToID: "root", Relation: "explains", Weight: 1}}
	upsert(t, s, linked)
	observed := mk("observed", "note", "t", "measured")
	observed.EvidenceRefs = []string{"root"}
	observed.Origin = OriginObserved
	upsert(t, s, observed)
	second := mk("second-hop", "note", "t", "cites the citation")
	second.EvidenceRefs = []string{"cites"}
	upsert(t, s, second)

	ok, cascaded := s.Refute("root", "wrong", []string{"log line"}, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	require.True(t, ok)
	assert.Equal(t, []string{"cites", "linked"}, cascaded)
	for _, id := range []string{"root", "cites", "linked"} {
		it, _ := s.Get(id)
		assert.Equal(t, "stale", it.Status, id)
	}
	for _, id := range []string{"observed", "second-hop"} {
		it, _ := s.Get(id)
		assert.Equal(t, "active", it.Status, id+" is not touched")
	}
	root, _ := s.Get("root")
	assert.Contains(t, root.Data, RefutedDataKey)

	ok, _ = s.Refute("missing", "x", nil, time.Now())
	assert.False(t, ok)
	ok, _ = s.Refute(" ", "x", nil, time.Now())
	assert.False(t, ok)
}

func TestStore_TraverseSpreadsActivationAndSkipsPrivateAndAnchors(t *testing.T) {
	s := NewStore()
	a := mk("a", "note", "t", "c")
	a.Links = []memoryv1.ThoughtLink{{ToID: "b", Relation: "explains", Weight: 1}, {ToID: "priv", Relation: "explains", Weight: 1}}
	b := mk("b", "note", "t", "c")
	b.Links = []memoryv1.ThoughtLink{{ToID: "c", Relation: "explains", Weight: 0.5}, {ToID: "a", Relation: "explains", Weight: 1}}
	priv := mk("priv", "note", "t", "c")
	priv.Scope = ScopePrivate
	s.Put(a)
	s.Put(b)
	s.Put(mk("c", "note", "t", "c"))
	s.Put(priv)

	got := s.Traverse([]string{"a"}, 0, 0.6)
	assert.Equal(t, []string{"b", "c"}, ids(got), "ordered by activation; anchor and private item excluded")
	assert.Equal(t, []string{"b"}, ids(s.Traverse([]string{"a"}, 1, 0.6)), "depth bounds the walk")
}

func TestStore_PruneExceptRemovesRestoredAndRetiredItemsNotInKeep(t *testing.T) {
	s := NewStore()
	restored := mk("restored", "note", "t", "c")
	restored.CellOriginated = true
	s.Put(restored)
	kept := mk("kept", "note", "t", "c")
	kept.CellOriginated = true
	s.Put(kept)
	upsert(t, s, mk("authored", "note", "t", "c"))
	retired := mk("retired", "note", "t", "c")
	retired.Status = "stale"
	s.Put(retired)

	assert.Equal(t, 2, s.PruneExcept(map[string]struct{}{"kept": {}}))
	_, ok := s.Get("authored")
	assert.True(t, ok, "an active item authored in memory is never pruned")
	_, ok = s.Get("kept")
	assert.True(t, ok)
	_, ok = s.Get("restored")
	assert.False(t, ok)
	assert.False(t, s.Indexed("restored"))
}

func TestStore_ForgetLockedRemovesWithIndexAndPublishesOneDeletePerItem(t *testing.T) {
	s := NewStore()
	upsert(t, s, mk("a", "note", "needle", "c", "tenant/x"))
	upsert(t, s, mk("b", "note", "needle", "c", "tenant/y"))
	var ops []string
	s.SetChangeHook(func(op string, it *Item, id, kind string) { ops = append(ops, op+":"+id) })

	s.mu.Lock()
	gone := s.ForgetLocked(func(it Item) bool { return PartitionFromTags(it.Tags) == "x" })
	s.mu.Unlock()
	s.PublishRemoved(gone)

	assert.Equal(t, []Removed{{ID: "a", Kind: "note"}}, gone)
	assert.Equal(t, []string{"delete:a"}, ops)
	assert.False(t, s.Indexed("a"))
	assert.True(t, s.Indexed("b"))
}

func TestStore_RestorePutMutateAndHits(t *testing.T) {
	s := NewStore()
	upsert(t, s, mk("old", "note", "t", "c"))
	s.Restore([]Item{mk("r1", "note", "restored one", "c"), mk("r2", "note", "restored two", "c")})
	assert.ElementsMatch(t, []string{"r1", "r2"}, ids(s.AllItems()), "Restore replaces everything")
	r1, _ := s.Get("r1")
	assert.Equal(t, OriginDerived, r1.Origin, "a restored item reads as derived")
	assert.True(t, s.Indexed("r1"))
	assert.Equal(t, 2, s.LenLocked())

	assert.False(t, s.Mutate("missing", func(*Item) bool { return true }))
	assert.True(t, s.Mutate("r1", func(it *Item) bool { it.Tags = append(it.Tags, "x"); return true }))
	r1, _ = s.Get("r1")
	assert.Equal(t, []string{"x"}, r1.Tags)
	before := r1.UpdatedAt
	assert.True(t, s.Mutate("r1", func(*Item) bool { return false }))
	r1, _ = s.Get("r1")
	assert.Equal(t, before, r1.UpdatedAt, "no change, no stamp")

	var gotIDs, gotKinds []string
	s.SetHitHook(func(ids []string, query, kind string, kinds []string) { gotIDs, gotKinds = ids, kinds })
	s.NotifyHit(nil, "q", "", nil)
	assert.Nil(t, gotIDs, "an empty hit is not an event")
	s.NotifyHit([]string{"a", "b"}, "q", "", []string{"note"})
	assert.Equal(t, []string{"a", "b"}, gotIDs)
	assert.Nil(t, gotKinds, "kinds of the wrong length are dropped")
	s.NotifyHit([]string{"a"}, "q", "", []string{"note"})
	assert.Equal(t, []string{"note"}, gotKinds)
}

func TestStore_CloneIntoCopiesItemsOnly(t *testing.T) {
	s := NewStore()
	upsert(t, s, mk("a", "note", "t", "c"))
	lock := &sync.RWMutex{}
	c := s.CloneInto(lock, Config{Subject: "copy"})
	assert.Equal(t, "copy", c.Subject())
	got, ok := c.Get("a")
	require.True(t, ok)
	got.Title = "changed"
	orig, _ := s.Get("a")
	assert.Equal(t, "t", orig.Title)
	assert.False(t, c.Indexed("a"), "the indexes of the copy start empty")
}

func TestStore_SubjectFilterMatchesServiceTags(t *testing.T) {
	s := NewStore(WithConfig(Config{Subject: "instance-1"}))
	a := mk("a", "note", "t", "c")
	a.Subject = "instance-1"
	a.Tags = []string{ServiceTagPrefix + "auth"}
	upsert(t, s, a)
	b := mk("b", "note", "t", "c")
	b.Subject = "auth"
	upsert(t, s, b)
	assert.Equal(t, "instance-1", s.Subject())
	assert.ElementsMatch(t, []string{"a", "b"}, ids(s.Filtered("", "", "", "auth", "", 0)), "a service tag or the literal subject")
}

func TestStore_GeneratedIDsUseThePrefix(t *testing.T) {
	s := NewStore(WithConfig(Config{IDPrefix: "mem-"}))
	got := upsert(t, s, mk("", "note", "t", "c"))
	assert.True(t, strings.HasPrefix(got.ID, "mem-"), got.ID)
	assert.True(t, strings.HasPrefix(upsert(t, NewStore(), mk("", "note", "t", "c")).ID, "memory-"))
}

// newTestStore is a store with no host seams and its own lock.
func newTestStore() *Store { return NewStore() }

func TestValidateBase_NeedsOnlyAVocabulary(t *testing.T) {
	v := Vocabulary{Rejected: []string{"policy"}}
	assert.Contains(t, ValidateBase(v, mk("a", "policy", "t", "c")), "cannot be written")
	assert.Empty(t, ValidateBase(v, mk("a", "note", "t", "c")))
	assert.Equal(t, NewStore(WithConfig(Config{Vocabulary: v})).ValidateBase(mk("a", "policy", "t", "c")), ValidateBase(v, mk("a", "policy", "t", "c")))
}
