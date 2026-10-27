package memstore

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func rerankItem(id, kind, origin, confidence string, updated time.Time) Item {
	it := mk(id, kind, id, id)
	it.Origin, it.Confidence = origin, confidence
	it.UpdatedAt = metav1.NewTime(updated)
	return it
}

// Provenance and confidence are on every item and decide which of two
// restatements to keep when consolidating; the same two facts should also decide
// between a person's verified answer and a model's guess at the same rank.
func TestRerank_APersonsHighConfidenceFactOutranksAFreshLowConfidenceGuess(t *testing.T) {
	now := time.Now()
	s := NewSearcher(SearchConfig{})
	guess := rerankItem("guess", "hypothesis", OriginDerived, "low", now)
	fact := rerankItem("fact", KindStableFact, OriginHuman, "high", now.Add(-5*24*time.Hour))
	out := s.Rerank([]Item{guess, fact}, 10)
	assert.Equal(t, "fact", out[0].Item.ID, "the human, high-confidence fact one rank lower still comes first")
}

func TestRerank_ProvenanceAndConfidenceMultipliersAreBounded(t *testing.T) {
	assert.Equal(t, 1.10, provenanceMultiplier(OriginHuman))
	assert.Equal(t, 1.10, provenanceMultiplier(OriginVerified))
	assert.Equal(t, 1.0, provenanceMultiplier(OriginObserved))
	assert.Equal(t, 1.0, provenanceMultiplier(OriginDerived))
	assert.Equal(t, 1.0, provenanceMultiplier(""))
	assert.Equal(t, 1.05, confidenceMultiplier("high"))
	assert.Equal(t, 1.05, confidenceMultiplier(" High "))
	assert.Equal(t, 1.0, confidenceMultiplier("medium"))
	assert.Equal(t, 1.0, confidenceMultiplier(""))
	assert.Equal(t, 0.92, confidenceMultiplier("low"))
}

// An event cell restored in bulk carries the time it was ingested in
// UpdatedAt, however long ago the event happened.
func TestRerank_AnEventAgesFromItsEventDate(t *testing.T) {
	now := time.Now()
	old := now.Add(-60 * 24 * time.Hour).UTC().Format(time.RFC3339)
	assert.Equal(t, now.Add(-60*24*time.Hour).UTC().Truncate(time.Second), effectiveTime(withEventDate(rerankItem("e", KindEvent, "", "", now), old)).UTC())
	assert.Equal(t, now.Truncate(time.Nanosecond).Unix(), effectiveTime(withEventDate(rerankItem("n", "note", "", "", now), old)).Unix(), "only an event ages from its event date")
	assert.Equal(t, now.Unix(), effectiveTime(withEventDate(rerankItem("b", KindEvent, "", "", now), "not a date")).Unix(), "an unparseable date falls back to UpdatedAt")
	assert.Equal(t, now.Unix(), effectiveTime(rerankItem("c", KindEvent, "", "", now)).Unix(), "no event date, no change")

	s := NewSearcher(SearchConfig{})
	stale := withEventDate(rerankItem("stale-event", KindEvent, "", "", now), old)
	fresh := rerankItem("fresh-event", KindEvent, "", "", now)
	fresh.EventDate = now.UTC().Format(time.RFC3339)
	out := s.Rerank([]Item{stale, fresh}, 10)
	score := map[string]float64{}
	for _, r := range out {
		score[r.Item.ID] = r.Score
	}
	assert.Less(t, score["stale-event"], RerankBaseScore(0)*confidenceMultiplier("")*0.9, "ingested now, happened two months ago: aged by the event")
	assert.InDelta(t, RerankBaseScore(1), score["fresh-event"], 0.001)
}

func withEventDate(it Item, d string) Item { it.EventDate = d; return it }

// ── which restatement survives ───────────────────────────────────────────

func candidate(id, origin, confidence string, updated time.Time, score float64) rerankCandidate {
	return rerankCandidate{result: SearchResult{Item: rerankItem(id, KindStableFact, origin, confidence, updated), Score: score}}
}

func TestCollapseBetter_ConfidenceAndOriginComeBeforeRecency(t *testing.T) {
	now := time.Now()
	s := NewSearcher(SearchConfig{})
	oldHighHuman := candidate("old", OriginHuman, "high", now.Add(-24*time.Hour), 1)
	newLowDerived := candidate("new", OriginDerived, "low", now, 1)
	assert.True(t, s.collapseBetter(oldHighHuman, newLowDerived), "a fresh low-confidence restatement does not replace an older high-confidence fact")
	assert.False(t, s.collapseBetter(newLowDerived, oldHighHuman))

	sameConfidenceDerived := candidate("d", OriginDerived, "high", now, 1)
	sameConfidenceHuman := candidate("h", OriginHuman, "high", now.Add(-time.Hour), 1)
	assert.True(t, s.collapseBetter(sameConfidenceHuman, sameConfidenceDerived), "at equal confidence the more trusted origin wins")

	a, b := candidate("a", OriginDerived, "high", now, 1), candidate("b", OriginDerived, "high", now.Add(-time.Hour), 1)
	assert.True(t, s.collapseBetter(a, b), "then the newer one")
	higher := candidate("c", OriginDerived, "low", now.Add(-time.Hour), 2)
	assert.True(t, s.collapseBetter(higher, a), "a higher score still decides first")
}

func TestBetterRoutingFact_ConfidenceThenOriginThenNewerThenID(t *testing.T) {
	now := time.Now()
	mkf := func(id, origin, confidence string, age time.Duration) Item {
		return rerankItem(id, KindStableFact, origin, confidence, now.Add(-age))
	}
	assert.True(t, BetterRoutingFact(mkf("a", OriginDerived, "high", time.Hour), mkf("b", OriginHuman, "medium", 0)))
	assert.True(t, BetterRoutingFact(mkf("a", OriginHuman, "high", time.Hour), mkf("b", OriginDerived, "high", 0)), "origin before recency")
	assert.True(t, BetterRoutingFact(mkf("a", OriginDerived, "high", 0), mkf("b", OriginDerived, "high", time.Hour)))
	assert.True(t, BetterRoutingFact(mkf("a", OriginDerived, "high", 0), mkf("b", OriginDerived, "high", 0)) != BetterRoutingFact(mkf("b", OriginDerived, "high", 0), mkf("a", OriginDerived, "high", 0)), "never both or neither: the id breaks a full tie")
	assert.Equal(t, 3, originRank(OriginHuman))
	assert.Equal(t, 2, originRank(OriginVerified))
	assert.Equal(t, 1, originRank(OriginObserved))
	assert.Equal(t, 0, originRank(OriginDerived))
	assert.Equal(t, 0, originRank(""))
}
