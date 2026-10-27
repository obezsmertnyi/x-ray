package memstore

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	memoryv1 "github.com/triageagent-dev/x-ray/v2/api/v1"
)

// ── the lexical short-circuit ────────────────────────────────────────

func TestHybridSearch_AlwaysFuseConsultsTheIndexEvenForAConfidentLexicalAnswer(t *testing.T) {
	f := newFixture(t, SearchConfig{AlwaysFuse: true})
	f.add(t, mk("a", "note", "cache eviction policy", "idle entries are evicted after ten minutes"), vec4(1, 0, 0, 0))
	f.vecs["cache eviction policy"] = vec4(1, 0, 0, 0)

	got := f.search.HybridSearch(context.Background(), "cache eviction policy", "", "", "", 5)
	assert.Equal(t, "a", got[0].Item.ID)
	assert.EqualValues(t, 1, f.embeds.Load(), "a long question over long cells shares tokens with almost anything, so the lexical answer is not trusted to stand alone")
	assert.EqualValues(t, 0, f.search.Stats().Snapshot().KWVoiceHits)
}

// ── recall shaping ──────────────────────────────────────────────────

func TestHybridSearch_NoShapingReturnsTheFusedOrderUnchanged(t *testing.T) {
	body := "The scheduler assigns pending jobs to the least loaded worker and retries a job twice before it gives up."
	shaped := newFixture(t, SearchConfig{})
	raw := newFixture(t, SearchConfig{NoShaping: true})
	for _, f := range []*fixture{shaped, raw} {
		f.add(t, mk("k1", KindStableFact, "Scheduler assigns pending jobs", body), nil)
		f.add(t, mk("k2", KindStableFact, "Scheduler assigns pending jobs", body+" "), nil)
	}
	assert.Len(t, shaped.search.HybridSearch(context.Background(), "scheduler assigns pending jobs", "", "", "", 5), 1, "near-duplicates collapse")
	got := raw.search.HybridSearch(context.Background(), "scheduler assigns pending jobs", "", "", "", 5)
	assert.Len(t, got, 2, "with shaping off both restatements come back")
	for _, r := range got {
		assert.Zero(t, r.CollapsedCount)
	}
}

// ── source collapse ─────────────────────────────────────────────────

func TestHybridSearch_SourceCollapseReplacesAFactByItsSourceEpisode(t *testing.T) {
	f := newFixture(t, SearchConfig{SourceRelation: "derived_from"})
	ep := mk("episode-1", "episodic", "Tuesday call", "a long transcript of the call about the launch date")
	f.add(t, ep, nil)
	other := mk("episode-2", "episodic", "Thursday call", "a different call about hiring")
	f.add(t, other, nil)
	link := func(to string) []memoryv1.ThoughtLink {
		return []memoryv1.ThoughtLink{{ToID: to, Relation: "derived_from", Weight: 1}}
	}
	f1 := mk("fact-1", KindStableFact, "launch date fixed", "the launch is on the ninth of may")
	f1.Links = link("episode-1")
	f2 := mk("fact-2", KindStableFact, "launch owner named", "the launch owner is the platform team")
	f2.Links = link("episode-1")
	f3 := mk("fact-3", KindStableFact, "hiring plan launch", "two engineers join before the launch")
	f3.Links = link("episode-2")
	orphan := mk("fact-4", KindStableFact, "launch checklist", "the launch checklist lives in the wiki")
	orphan.Links = link("gone-episode")
	for _, it := range []Item{f1, f2, f3, orphan} {
		f.add(t, it, nil)
	}

	got := f.search.HybridSearch(context.Background(), "launch", "", "", "", 10)
	byID := map[string]SearchResult{}
	for _, r := range got {
		byID[r.Item.ID] = r
	}
	require.Contains(t, byID, "episode-1", "two facts of one episode surface the episode once")
	assert.NotContains(t, byID, "fact-1")
	assert.NotContains(t, byID, "fact-2")
	assert.Contains(t, []string{"fact-1", "fact-2"}, byID["episode-1"].Via, "the episode reports the fact that reached it")
	assert.Contains(t, byID, "episode-2")
	assert.Equal(t, "fact-3", byID["episode-2"].Via)
	assert.Contains(t, byID, "fact-4", "a dangling source keeps the fact instead of losing the hit")
	assert.Empty(t, byID["fact-4"].Via)

	off := newFixture(t, SearchConfig{})
	for _, it := range []Item{ep, other, f1, f2, f3, orphan} {
		off.add(t, it, nil)
	}
	for _, r := range off.search.HybridSearch(context.Background(), "launch", "", "", "", 10) {
		assert.Empty(t, r.Via, "without the switch nothing is replaced")
	}
}

func TestHybridSearch_SourceCollapseRespectsVisibilityOfTheSource(t *testing.T) {
	f := newFixture(t, SearchConfig{
		SourceRelation: "derived_from",
		Visible:        func(it Item, partition string) bool { return PartitionFromTags(it.Tags) != "b" },
	})
	f.add(t, mk("secret-episode", "episodic", "t", "c", "tenant/b"), nil)
	fact := mk("fact", KindStableFact, "visible fact words", "the fact itself is visible to everyone")
	fact.Links = []memoryv1.ThoughtLink{{ToID: "secret-episode", Relation: "derived_from", Weight: 1}}
	f.add(t, fact, nil)
	got := resultIDs(f.search.HybridSearch(context.Background(), "visible fact words", "", "", "a", 10))
	assert.Equal(t, []string{"fact"}, got, "a hidden source is never substituted")
}

func TestPoolSize_FanoutWidensThePoolForSourceCollapse(t *testing.T) {
	assert.Equal(t, 40, poolSize(SearchConfig{SourceRelation: "r", NoShaping: true, Fanout: 4}, 10), "an explicit fanout is taken as given")
	assert.Equal(t, 30, poolSize(SearchConfig{SourceRelation: "r", NoShaping: true}, 10), "the default fanout is three")
	assert.Equal(t, 40, poolSize(SearchConfig{}, 10), "no collapse: the rerank over-fetch, limit times four")
	assert.Equal(t, 60, poolSize(SearchConfig{}, 30), "capped at the rerank maximum")
	assert.Equal(t, 10, poolSize(SearchConfig{NoShaping: true}, 10), "no shaping, no collapse: exactly the limit")
	assert.Equal(t, 40, poolSize(SearchConfig{SourceRelation: "r"}, 10), "with both, the larger pool")
	assert.Equal(t, 10, poolSize(SearchConfig{SourceRelation: "r", NoShaping: true, Fanout: -3}, 10), "a fanout below one means one: never fewer than the limit")
}

// ── per-subject routing of the semantic channel ─────────────────────

func TestHybridSearch_SubjectRoutingKeepsTheSemanticChannelInsideTheSubject(t *testing.T) {
	for _, routed := range []bool{false, true} {
		f := newFixture(t, SearchConfig{SubjectRouting: routed})
		inA := mk("in-a", "note", "aaa", "alpha text")
		inA.Subject = "topic-a"
		inB := mk("in-b", "note", "bbb", "beta text")
		inB.Subject = "topic-b"
		global := mk("global", "note", "ggg", "gamma text")
		f.add(t, inA, vec4(1, 0, 0, 0))
		f.add(t, inB, vec4(0.99, 0.1, 0, 0))
		f.add(t, global, vec4(0.98, 0.2, 0, 0))
		f.vecs["no lexical overlap"] = vec4(1, 0, 0, 0)

		got := resultIDs(f.search.HybridSearch(context.Background(), "no lexical overlap", "", "topic-a", "", 10))
		assert.Contains(t, got, "in-a")
		if routed {
			assert.NotContains(t, got, "in-b", "the other subject's index is never searched")
			assert.Contains(t, got, "global", "the global index is merged in")
		}
	}
}
