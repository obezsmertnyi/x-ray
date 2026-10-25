package xray

import (
	"math"
	"sort"
	"time"
)

// Recall shaping: a pure re-rank step over the candidate pool Search already
// built. It reorders and collapses items the lexical and semantic channels
// found; it never writes a cell and never adds one the caller could not see.

const (
	// maxRerankFetchLimit caps the pool Search pulls so the re-rank has
	// candidates to promote: limit*rerankPoolFactor, at most this many.
	maxRerankFetchLimit = 60
	rerankPoolFactor    = 4

	// rerankRankDecay is how fast rerankBaseScore falls with input rank:
	// hyperbolic, gentle enough that a boosted item several ranks back can
	// overtake rank 0, strictly decreasing so unboosted items keep their order.
	rerankRankDecay = 0.08

	// watchItemActiveMultiplier boosts an active, unexpired watch_item: content
	// that exists to flag something worth monitoring.
	watchItemActiveMultiplier = 1.7

	// recencyHalfLife is the age at which recencyMultiplier sits halfway
	// between 1.0 (fresh) and recencyFloor (very old). recencyFloor bounds how
	// much age alone can demote an item: it reorders ties, not relevance.
	recencyHalfLife = 7 * 24 * time.Hour
	recencyFloor    = 0.85
)

// rerankBaseScore turns a 0-based input rank into the base score the
// multipliers scale: 1/(1+rank*rerankRankDecay).
func rerankBaseScore(rank int) float64 {
	return 1.0 / (1.0 + float64(rank)*rerankRankDecay)
}

// recencyMultiplier is a mild multiplier from a cell's UpdatedAt age. A zero or
// future time is neutral.
func recencyMultiplier(updatedAt, now time.Time) float64 {
	if updatedAt.IsZero() {
		return 1.0
	}
	age := now.Sub(updatedAt)
	if age <= 0 {
		return 1.0
	}
	decay := math.Pow(0.5, age.Hours()/recencyHalfLife.Hours())
	return recencyFloor + (1.0-recencyFloor)*decay
}

// activeWatchItem reports whether it is a watch_item that is active and not
// past its StaleAfter deadline.
func activeWatchItem(it *MemoryItem, now time.Time) bool {
	if it.Kind != "watch_item" || !isActive(it.Status) {
		return false
	}
	return it.StaleAfter == nil || it.StaleAfter.After(now)
}

// SetRecallShaping turns the near-duplicate collapse and state-aware re-rank on
// or off (default on). With it off, Search returns the fused order unchanged.
// Safe to call at any time.
func (s *MemoryStore) SetRecallShaping(on bool) { s.noShaping.Store(!on) }

// SetWriteDedup turns write-side near-duplicate merging on or off (default on):
// an Upsert that restates an existing stable_fact, operational_pattern,
// watch_item or hypothesis lands on that cell instead of minting a peer.
func (s *MemoryStore) SetWriteDedup(on bool) { s.noWriteDedup.Store(!on) }

// poolSize is how many candidates each channel fetches for limit: the
// second-stage fanout when source collapse runs (exactly that
// when SetCandidateFanout was called), and otherwise at least
// limit*rerankPoolFactor (capped) so the re-rank has something to promote.
func (s *MemoryStore) poolSize(limit int) int {
	want := limit
	if s.sourceCollapseRel() != "" {
		want = limit * s.candidateFanoutFactor()
		if s.fanout.Load() != 0 {
			return want // an explicit fanout is taken as given
		}
	}
	if !s.noShaping.Load() {
		shaped := limit * rerankPoolFactor
		if shaped > maxRerankFetchLimit {
			shaped = maxRerankFetchLimit
		}
		if shaped > want {
			want = shaped
		}
	}
	return want
}

// collapseNearDuplicates keeps the best-ranked member of each near-duplicate
// cluster among the dedupKinds items of pool, in rank order; every other kind
// passes through. A survivor that absorbed others reports how many in
// Collapsed (on a copy: the stored item is never touched).
func collapseNearDuplicates(pool []*MemoryItem) []*MemoryItem {
	var eligible []*MemoryItem
	for _, it := range pool {
		if dedupKinds[it.Kind] {
			eligible = append(eligible, it)
		}
	}
	if len(eligible) < 2 {
		return pool
	}
	drop := make(map[string]bool)
	absorbed := make(map[string]int)
	for _, cluster := range clusterNearDuplicates(eligible) {
		for _, it := range cluster[1:] {
			drop[it.ID] = true
		}
		if n := len(cluster) - 1; n > 0 {
			absorbed[cluster[0].ID] = n
		}
	}
	out := make([]*MemoryItem, 0, len(pool))
	for _, it := range pool {
		if drop[it.ID] {
			continue
		}
		if n := absorbed[it.ID]; n > 0 {
			c := *it
			c.Collapsed = n
			it = &c
		}
		out = append(out, it)
	}
	return out
}

// stateRerank scores each item of pool by its rank times the active
// watch-item and recency multipliers and returns them best-first, deterministic
// (a stable sort with an id tie-break). The pool is in current-relevance order.
func stateRerank(pool []*MemoryItem, now time.Time) []*MemoryItem {
	if len(pool) < 2 {
		return pool
	}
	type scored struct {
		it    *MemoryItem
		score float64
	}
	sc := make([]scored, len(pool))
	for i, it := range pool {
		score := rerankBaseScore(i) * recencyMultiplier(it.UpdatedAt, now)
		if activeWatchItem(it, now) {
			score *= watchItemActiveMultiplier
		}
		sc[i] = scored{it, score}
	}
	sort.SliceStable(sc, func(i, j int) bool {
		if sc[i].score != sc[j].score {
			return sc[i].score > sc[j].score
		}
		return sc[i].it.ID < sc[j].it.ID
	})
	out := make([]*MemoryItem, len(sc))
	for i, v := range sc {
		out[i] = v.it
	}
	return out
}

// shape applies the recall shaping to pool: collapse near-duplicates, then
// re-rank by state.
func (s *MemoryStore) shape(pool []*MemoryItem) []*MemoryItem {
	if s.noShaping.Load() {
		return pool
	}
	return stateRerank(collapseNearDuplicates(pool), time.Now())
}
