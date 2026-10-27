package memstore

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	memoryv1 "github.com/triageagent-dev/x-ray/v2/api/v1"
)

// ── default deadlines by kind ─────────────────────────────────────────

func decayStore() *Store {
	return NewStore(WithConfig(Config{
		DefaultTTL: map[string]time.Duration{
			KindEvent:      30 * 24 * time.Hour,
			"note":         90 * 24 * time.Hour,
			KindHypothesis: 24 * time.Hour,
			KindUnknown:    7 * 24 * time.Hour,
		},
	}))
}

func within(t *testing.T, got *metav1.Time, d time.Duration, msg string) {
	t.Helper()
	require.NotNil(t, got, msg)
	assert.WithinDuration(t, time.Now().Add(d), got.Time, 5*time.Second, msg)
}

func TestStore_DefaultTTLStampsANewItemOfADecayingKind(t *testing.T) {
	s := decayStore()
	within(t, upsert(t, s, mk("e", KindEvent, "t", "c")).StaleAfter, 30*24*time.Hour, "event")
	within(t, upsert(t, s, mk("n", "note", "t", "c")).StaleAfter, 90*24*time.Hour, "note")
	within(t, upsert(t, s, mk("h", KindHypothesis, "t", "c")).StaleAfter, 24*time.Hour, "hypothesis")
	assert.Nil(t, upsert(t, s, mk("f", KindStableFact, "t", "c")).StaleAfter, "a kind with no default never expires")

	explicit := mk("x", KindEvent, "t", "c")
	at := metav1.NewTime(time.Now().Add(time.Hour))
	explicit.StaleAfter = &at
	within(t, upsert(t, s, explicit).StaleAfter, time.Hour, "an explicit deadline is kept")
}

func TestStore_DefaultTTLNeverRestampsAnUpdate(t *testing.T) {
	s := decayStore()
	first := upsert(t, s, mk("n", "note", "t", "c"))
	require.NotNil(t, first.StaleAfter)
	soon := metav1.NewTime(time.Now().Add(time.Minute))
	s.Mutate("n", func(it *Item) bool { it.StaleAfter = &soon; return true })

	edited := upsert(t, s, mk("n", "note", "t", "edited"))
	require.NotNil(t, edited.StaleAfter)
	assert.WithinDuration(t, soon.Time, edited.StaleAfter.Time, time.Second, "an update with no deadline keeps the stored one: it never restamps or clears it implicitly")

	renewal := mk("n", "note", "t", "renewed")
	later := metav1.NewTime(time.Now().Add(48 * time.Hour))
	renewal.StaleAfter = &later
	within(t, upsert(t, s, renewal).StaleAfter, 48*time.Hour, "renewal is an explicit deadline from the writer")
}

func TestStore_RenewOnMergeReArmsTheDeadlineOfARestatedDecayingKind(t *testing.T) {
	text := "The cache layer evicts idle entries after ten minutes of silence and logs every eviction at debug level."
	for _, renew := range []bool{false, true} {
		s := NewStore(WithConfig(Config{
			DefaultTTL:   map[string]time.Duration{KindHypothesis: 24 * time.Hour},
			RenewOnMerge: renew,
		}))
		first := mk("keep", KindHypothesis, "Cache layer evicts idle entries", text)
		soon := metav1.NewTime(time.Now().Add(time.Minute))
		first.StaleAfter = &soon
		upsert(t, s, first)

		got, mergedInto, msg := s.UpsertDedup(mk("again", KindHypothesis, "Cache layer evicts idle entries", text+" "))
		require.Empty(t, msg)
		require.Equal(t, "keep", mergedInto)
		require.NotNil(t, got.StaleAfter)
		if renew {
			within(t, got.StaleAfter, 24*time.Hour, "a restatement re-arms the deadline")
		} else {
			assert.WithinDuration(t, soon.Time, got.StaleAfter.Time, time.Second, "without the switch the merged cell keeps its deadline")
		}
	}
}

func TestStore_WithoutDefaultTTLTheHypothesisDefaultsStayAsTheyWere(t *testing.T) {
	s := NewStore()
	within(t, upsert(t, s, mk("h", KindHypothesis, "t", "c")).StaleAfter, 24*time.Hour, "compiled default")
	first := mk("h2", KindHypothesis, "t", "c")
	soon := metav1.NewTime(time.Now().Add(time.Minute))
	first.StaleAfter = &soon
	upsert(t, s, first)
	within(t, upsert(t, s, mk("h2", KindHypothesis, "t", "edited")).StaleAfter, 24*time.Hour, "an update restamps, as the host has always seen")
}

// ── keyword search with a per-kind quota ─────────────────────────────

func TestKeywordSearchPerKind_CapsEachKindSoOneCannotCrowdTheRest(t *testing.T) {
	s := NewStore()
	for i := 0; i < 8; i++ {
		upsert(t, s, mk(fmt.Sprintf("event-%d", i), KindEvent, "deploy finished", fmt.Sprintf("deploy number %d finished", i)))
	}
	upsert(t, s, mk("fact-1", KindStableFact, "deploy process", "how a deploy is done"))
	upsert(t, s, mk("pattern-1", KindOperationalPattern, "deploy freeze", "deploys freeze on fridays"))

	flat := s.KeywordSearch("deploy", "", "", 5)
	kinds := map[string]int{}
	for _, it := range flat {
		kinds[it.Kind]++
	}
	assert.GreaterOrEqual(t, kinds[KindEvent], 3, "without a quota the dominant kind fills the limit")

	fair := s.KeywordSearchPerKind("deploy", "", "", 10, 2)
	counts := map[string]int{}
	for _, it := range fair {
		counts[it.Kind]++
	}
	assert.Equal(t, 2, counts[KindEvent], "at most two of the dominant kind")
	assert.Equal(t, 1, counts[KindStableFact])
	assert.Equal(t, 1, counts[KindOperationalPattern])
	assert.Len(t, s.KeywordSearchPerKind("deploy", "", "", 3, 2), 3, "the limit is still a ceiling")
	assert.Equal(t, ids(s.KeywordSearch("deploy", "", "", 10)), ids(s.KeywordSearchPerKind("deploy", "", "", 10, 0)), "a zero quota is a plain search")
}

// ── the write boundary vocabulary ─────────────────────────────────────

func TestVocabulary_ValidateWriteAndSearchKind(t *testing.T) {
	v := BaseVocabulary()
	assert.Equal(t, "kind is required", v.ValidateWrite("", ""))
	assert.Contains(t, v.ValidateWrite("episodic", "high"), `kind "episodic" is not one of: `, "a server-written kind is not writable from outside")
	assert.Equal(t, "stable_fact items must have confidence=high", v.ValidateWrite(KindStableFact, "medium"))
	assert.Empty(t, v.ValidateWrite(KindStableFact, "high"))
	assert.Empty(t, v.ValidateWrite("note", ""))

	assert.Empty(t, v.ValidateSearchKind(""), "no filter means every kind")
	assert.Empty(t, v.ValidateSearchKind("episodic"), "but a server-written kind is searchable")
	assert.Empty(t, v.ValidateSearchKind("mental_map"))
	assert.Contains(t, v.ValidateSearchKind("system_configuration"), `kind "system_configuration" is not a searchable kind; use one of: `, "never indexed, so a filter on it can only return nothing")

	open := Vocabulary{}
	assert.Empty(t, open.ValidateWrite("anything", ""), "an open vocabulary accepts any kind")
	assert.Empty(t, open.ValidateSearchKind("anything"))
	assert.Equal(t, "kind is required", open.ValidateWrite("", ""))
}

// ── anchors and spreading activation ───────────────────────────────────

func TestStore_ResolveAnchors(t *testing.T) {
	s := NewStore()
	a := mk("cell-a", "note", "Billing service", "text")
	a.Subject = "billing"
	b := mk("cell-b", "note", "Invoices export", "text")
	b.Subject = "billing"
	c := mk("cell-c", "note", "Payroll batch", "text")
	c.Subject = "payroll-eu"
	priv := mk("cell-p", "note", "billing diary", "text", PrivateOwnerTagPrefix+"u")
	priv.Scope = ScopePrivate
	for _, it := range []Item{a, b, c, priv} {
		upsert(t, s, it)
	}

	assert.Equal(t, []string{"cell-a"}, s.ResolveAnchors("CELL-A"), "an id first, case-insensitively")
	assert.ElementsMatch(t, []string{"cell-a", "cell-b"}, s.ResolveAnchors("billing"), "then every cell whose subject is the anchor")
	assert.ElementsMatch(t, []string{"cell-c"}, s.ResolveAnchors("payroll"), "then a subject or title that contains it")
	assert.ElementsMatch(t, []string{"cell-b"}, s.ResolveAnchors("invoices"))
	assert.Empty(t, s.ResolveAnchors("diary"), "a private cell never resolves")
	assert.Empty(t, s.ResolveAnchors("  "))
}

func TestStore_ContextSpreadsActivationOverLinks(t *testing.T) {
	s := NewStore()
	link := func(to string, w float32) []memoryv1.ThoughtLink {
		return []memoryv1.ThoughtLink{{ToID: to, Relation: "explains", Weight: w}}
	}
	a := mk("a", "note", "anchor cell", "c")
	a.Links = link("b", 1)
	b := mk("b", "note", "second", "c")
	b.Links = link("c", 0.5)
	c := mk("c", "note", "third", "c")
	c.Links = link("d", 1)
	s.Put(a)
	s.Put(b)
	s.Put(c)
	s.Put(mk("d", "note", "fourth", "c"))

	anchors, activated := s.Context("anchor cell", 0)
	require.Len(t, anchors, 1)
	assert.Equal(t, "a", anchors[0].ID)
	require.Len(t, activated, 3, "default depth is three hops")
	assert.Equal(t, "b", activated[0].Item.ID)
	assert.InDelta(t, 0.6, activated[0].Score, 1e-6, "one hop multiplies by decay 0.6 and the link weight")
	assert.InDelta(t, 0.18, activated[1].Score, 1e-6)
	assert.InDelta(t, 0.108, activated[2].Score, 1e-6)

	_, one := s.Context("a", 1)
	assert.Len(t, one, 1)
	none, nothing := s.Context("zzz", 3)
	assert.Empty(t, none)
	assert.Empty(t, nothing)
	_, deep := s.Context("a", 99)
	assert.Len(t, deep, 3, "at most eight hops, and the graph is only three long")
}

// ── newest first ──────────────────────────────────────────────────────

func TestStore_RecentIsNewestFirstByCreationAndCanSkip(t *testing.T) {
	s := NewStore()
	base := time.Now().Add(-time.Hour)
	for i, id := range []string{"old", "mid", "new"} {
		it := mk(id, "episodic", id, "c")
		it.CreatedAt = metav1.NewTime(base.Add(time.Duration(i) * time.Minute))
		s.Put(it)
	}
	other := mk("note-1", "note", "n", "c")
	other.CreatedAt = metav1.NewTime(base.Add(time.Hour))
	s.Put(other)

	assert.Equal(t, []string{"new", "mid", "old"}, ids(s.Recent("episodic", "", "", 10, nil)))
	assert.Equal(t, []string{"new", "mid"}, ids(s.Recent("episodic", "", "", 2, nil)))
	assert.Equal(t, []string{"new", "old"}, ids(s.Recent("episodic", "", "", 10, func(it Item) bool { return it.ID == "mid" })), "a caller can drop what it considers expired")
	assert.Empty(t, s.Recent("episodic", "", "", 0, nil))
	assert.Equal(t, "note-1", s.Recent("", "", "", 10, nil)[0].ID, "no kind filter spans every kind")
}

// ── ids ───────────────────────────────────────────────────────────────

func TestStore_NewIDSuppliesTheIDOfAnItemWrittenWithoutOne(t *testing.T) {
	n := 0
	s := NewStore(WithConfig(Config{NewID: func() string { n++; return fmt.Sprintf("id-%d", n) }}))
	assert.Equal(t, "id-1", upsert(t, s, mk("", "note", "t", "c")).ID)
	got, msg := s.Supersede("id-1", mk("", "note", "t", "replacement"))
	require.Empty(t, msg)
	assert.Equal(t, "id-2", got.ID)
	assert.Equal(t, "named", upsert(t, s, mk("named", "note", "t", "c")).ID, "an item that has an id keeps it")
}

// ── counting ─────────────────────────────────────────────────────────

func TestStore_CountActiveCountsEveryActiveItemPrivateOnesIncluded(t *testing.T) {
	s := NewStore()
	upsert(t, s, mk("a", "note", "t", "c"))
	priv := mk("p", "note", "t", "c", PrivateOwnerTagPrefix+"u")
	priv.Scope = ScopePrivate
	upsert(t, s, priv)
	upsert(t, s, mk("gone", "note", "t", "c"))
	s.Expire("gone")
	assert.Equal(t, 2, s.CountActive())
}

// ── private search for the owner ───────────────────────────────────────

func TestKeywordSearchForUser_IncludesTheOwnersPrivateItemsOnly(t *testing.T) {
	s := NewStore()
	upsert(t, s, mk("shared", "note", "weekly roadmap review", "public text"))
	mine := mk("mine", "note", "roadmap diary", "my private thoughts about the roadmap", PrivateOwnerTagPrefix+"alice")
	mine.Scope = ScopePrivate
	upsert(t, s, mine)
	theirs := mk("theirs", "note", "roadmap diary", "someone else's thoughts about the roadmap", PrivateOwnerTagPrefix+"bob")
	theirs.Scope = ScopePrivate
	upsert(t, s, theirs)

	assert.Equal(t, []string{"shared"}, ids(s.KeywordSearch("roadmap", "", "", 10)), "the shared search never sees a private item")
	got := ids(s.KeywordSearchForUser("alice", "roadmap", "", "", 10))
	assert.ElementsMatch(t, []string{"shared", "mine"}, got)
	assert.Equal(t, []string{"mine"}, ids(s.KeywordSearchForUser("alice", "diary", "note", "", 10)))
	assert.Equal(t, []string{"shared"}, ids(s.KeywordSearchForUser("", "roadmap", "", "", 10)), "no user, no private items")
	assert.Equal(t, []string{"shared"}, ids(s.KeywordSearchForUser("carol", "roadmap", "", "", 10)))
}

// ── reconfiguring a searcher ───────────────────────────────────────────

func TestSearcher_WithReturnsACopyWithOtherSwitchesSharingTheStats(t *testing.T) {
	f := newFixture(t, SearchConfig{})
	f.add(t, mk("a", "note", "cache eviction policy", "idle entries are evicted"), vec4(1, 0, 0, 0))
	f.vecs["cache eviction policy"] = vec4(1, 0, 0, 0)

	fused := f.search.With(func(c *SearchConfig) { c.AlwaysFuse = true })
	fused.HybridSearch(context.Background(), "cache eviction policy", "", "", "", 5)
	assert.EqualValues(t, 1, f.embeds.Load(), "the copy always fuses")
	f.search.HybridSearch(context.Background(), "cache eviction policy", "", "", "", 5)
	assert.EqualValues(t, 1, f.embeds.Load(), "the original still short-circuits")
	assert.EqualValues(t, 2, f.search.Stats().Snapshot().Total, "both count into the same statistics")
}

// ── the stable-fact confidence rule ───────────────────────────────────

func TestVocabulary_FactConfidenceOptionalLeavesTheRuleToTheHostsBoundary(t *testing.T) {
	loose := mk("a", KindStableFact, "t", "c")
	loose.Confidence = ""

	strict := NewStore()
	_, msg := strict.Upsert(loose)
	assert.Equal(t, "stable_fact items must have confidence=high", msg, "the store enforces it by default")

	open := NewStore(WithConfig(Config{Vocabulary: Vocabulary{FactConfidenceOptional: true}}))
	_, msg = open.Upsert(loose)
	assert.Empty(t, msg, "a host that checks at its own boundary can turn the store's check off")

	assert.Equal(t, "stable_fact items must have confidence=high", Vocabulary{FactConfidenceOptional: true}.ValidateWrite(KindStableFact, "low"), "the boundary check is a separate function and stays on")
}

// ── ids as search text ────────────────────────────────────────────────

func TestStore_SkipIDTextKeepsAMintedIDOutOfTheSearchText(t *testing.T) {
	minted := "0123456789abcdef"
	skip := func(id string) bool { return len(id) == 16 && !strings.ContainsAny(id, "ghijklmnopqrstuvwxyz-") }
	for _, withSkip := range []bool{false, true} {
		cfg := Config{}
		if withSkip {
			cfg.SkipIDText = skip
		}
		s := NewStore(WithConfig(cfg))
		upsert(t, s, mk(minted, "note", "first note", "body of the first"))
		upsert(t, s, mk("billing-retry-policy", "note", "second note", "body of the second"))
		assert.Equal(t, []string{"billing-retry-policy"}, ids(s.KeywordSearch("billing retry policy", "", "", 5)), "a chosen id is always searchable")
		found := s.KeywordSearch(minted, "", "", 5)
		if withSkip {
			assert.Empty(t, found, "a minted id says nothing and is left out")
			assert.False(t, s.Indexed(minted) && len(found) > 0)
		} else {
			assert.Equal(t, []string{minted}, ids(found))
		}
	}
}
