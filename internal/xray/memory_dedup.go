package xray

import (
	"sort"
	"strings"
	"time"
)

// dedupKinds are the kinds a writer can restate under a fresh id: a restated
// fact, pattern, watch item or hypothesis lands on the cell it repeats instead
// of minting a peer (write side), is collapsed to one hit (read side) and is
// merged by ConsolidateMemory (sweep). Every other kind is keyed by what it is
// (an event has a date, a note is the writer's own) and is never deduplicated.
var dedupKinds = map[string]bool{
	"stable_fact":         true,
	"operational_pattern": true,
	"watch_item":          true,
	"hypothesis":          true,
}

// dedupStopWords are connective words dropped from the token set two items are
// compared on. They carry no discriminating signal and would inflate the
// similarity of two items that merely share sentence structure.
var dedupStopWords = map[string]struct{}{
	"the": {}, "and": {}, "for": {}, "are": {}, "with": {}, "this": {},
	"that": {}, "from": {}, "not": {}, "has": {}, "have": {}, "been": {},
	"its": {}, "into": {}, "due": {}, "when": {}, "while": {}, "should": {},
	"these": {}, "those": {}, "than": {}, "then": {}, "also": {}, "but": {},
	"any": {}, "all": {}, "per": {}, "via": {}, "was": {}, "were": {},
	"will": {}, "can": {},
}

// dedupPunctuation is stripped before tokenising, so quoting and sentence
// punctuation do not stay glued to the adjacent word.
const dedupPunctuation = ".,:;()'\"`"

const (
	// nearDupThreshold is the Jaccard similarity of two token sets at or above
	// which two comparable items are one fact restated.
	nearDupThreshold = 0.3
	// nearDupMinTokens is the size of the smaller token set below which Jaccard
	// stops being a signal: only near-identical text (nearDupShortThreshold)
	// counts then.
	nearDupMinTokens      = 8
	nearDupShortThreshold = 0.8
	// nearDupContainment treats a short restatement wholly contained in a long
	// fact as a duplicate even when the union dilutes the Jaccard score.
	nearDupContainment = 0.9
)

// dedupTokens is the token set title+content are compared on: tokens of three
// or more characters (or any length with a digit), minus stop words.
func dedupTokens(it *MemoryItem) map[string]struct{} {
	text := strings.Map(func(r rune) rune {
		if strings.ContainsRune(dedupPunctuation, r) {
			return -1
		}
		return r
	}, normalizeQuery(it.Title+" "+it.Content))
	out := make(map[string]struct{})
	for _, tok := range strings.Fields(text) {
		// A short token is noise unless it carries a digit: "q3" and "q4" are
		// what tell two otherwise identical plans apart.
		if len(tok) < 3 && !strings.ContainsAny(tok, "0123456789") {
			continue
		}
		if _, stop := dedupStopWords[tok]; stop {
			continue
		}
		out[tok] = struct{}{}
	}
	return out
}

// dedupComparable reports whether a (the existing side) and b (the incoming
// side) may be scored for near-duplication: same kind, same scope, same
// subject, and never a model-written b against a human-written a.
func dedupComparable(a, b *MemoryItem) bool {
	if a.Kind != b.Kind || a.Scope != b.Scope || !strings.EqualFold(a.Subject, b.Subject) {
		return false
	}
	if effectiveOrigin(a.Origin) == OriginHuman && effectiveOrigin(b.Origin) == OriginDerived {
		return false
	}
	return true
}

// nearDuplicate scores a and b. It returns (0, false) when they are not
// comparable or either has no tokens; otherwise score is the Jaccard similarity
// of their token sets.
func nearDuplicate(a, b *MemoryItem) (score float64, dup bool) {
	if !dedupComparable(a, b) {
		return 0, false
	}
	ta, tb := dedupTokens(a), dedupTokens(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0, false
	}
	inter := 0
	for tok := range ta {
		if _, ok := tb[tok]; ok {
			inter++
		}
	}
	score = float64(inter) / float64(len(ta)+len(tb)-inter)

	small, large := ta, tb
	if len(large) < len(small) {
		small, large = large, small
	}
	if len(small) < nearDupMinTokens {
		return score, score >= nearDupShortThreshold
	}
	if score >= nearDupThreshold {
		return score, true
	}
	contained := 0
	for tok := range small {
		if _, ok := large[tok]; ok {
			contained++
		}
	}
	return score, float64(contained)/float64(len(small)) >= nearDupContainment
}

// findNearDuplicate returns the highest-scoring active near-duplicate of item
// among candidates. An item never duplicates itself, and a retired cell is not
// a live target to update in place.
func findNearDuplicate(candidates []*MemoryItem, item *MemoryItem) (best *MemoryItem, score float64, ok bool) {
	for _, c := range candidates {
		if !isActive(c.Status) || c.ID == item.ID {
			continue
		}
		s, dup := nearDuplicate(c, item)
		if !dup {
			continue
		}
		if !ok || s > score {
			best, score, ok = c, s, true
		}
	}
	return best, score, ok
}

// clusterNearDuplicates groups items in one greedy pass that preserves input
// order: each item joins the first cluster whose first member it
// near-duplicates, else it starts a cluster of its own.
func clusterNearDuplicates(items []*MemoryItem) [][]*MemoryItem {
	var clusters [][]*MemoryItem
	for _, item := range items {
		placed := false
		for i, cluster := range clusters {
			if _, dup := nearDuplicate(cluster[0], item); dup {
				clusters[i] = append(clusters[i], item)
				placed = true
				break
			}
		}
		if !placed {
			clusters = append(clusters, []*MemoryItem{item})
		}
	}
	return clusters
}

// mergeTags is the union of a and b, a's order first.
func mergeTags(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, list := range [][]string{a, b} {
		for _, t := range list {
			if _, dup := seen[t]; dup {
				continue
			}
			seen[t] = struct{}{}
			out = append(out, t)
		}
	}
	return out
}

// ConsolidationGroup is one near-duplicate cluster found by ConsolidateMemory:
// Keep is the id a reader should trust going forward, Superseded lists every
// other member (parallel to Scores, each member's score against Keep).
type ConsolidationGroup struct {
	Kind       string    `json:"kind"`
	Keep       string    `json:"keep"`
	KeepTitle  string    `json:"keep_title"`
	Superseded []string  `json:"superseded"`
	Scores     []float64 `json:"scores"`
}

// ConsolidationReport is the outcome of one ConsolidateMemory sweep.
type ConsolidationReport struct {
	Scanned    int                  `json:"scanned"`
	Groups     []ConsolidationGroup `json:"groups"`
	Superseded int                  `json:"superseded"`
	Applied    bool                 `json:"applied"`
}

func confidenceRank(c string) int {
	switch strings.ToLower(c) {
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	}
	return 0
}

func originRank(o string) int {
	switch effectiveOrigin(o) {
	case OriginHuman:
		return 3
	case OriginVerified:
		return 2
	case OriginObserved:
		return 1
	}
	return 0
}

// betterFact reports whether a should be kept over b: higher confidence, then
// the more trusted origin, then the newer, then the smaller id.
func betterFact(a, b *MemoryItem) bool {
	if ca, cb := confidenceRank(a.Confidence), confidenceRank(b.Confidence); ca != cb {
		return ca > cb
	}
	if oa, ob := originRank(a.Origin), originRank(b.Origin); oa != ob {
		return oa > ob
	}
	if !a.UpdatedAt.Equal(b.UpdatedAt) {
		return a.UpdatedAt.After(b.UpdatedAt)
	}
	return a.ID < b.ID
}

// ConsolidateMemory groups the active public cells of dedupKinds (optionally of
// one subject) into near-duplicate clusters and, when apply is true, retires
// every member but each cluster's keeper (status superseded, the keeper's
// Supersedes lists them). apply=false returns the same groups and writes
// nothing. The second result is the changed cells, which a caller backed by the
// cluster persists.
func (s *MemoryStore) ConsolidateMemory(subject string, apply bool) (ConsolidationReport, []*MemoryItem) {
	report := ConsolidationReport{Applied: apply}
	var changed []*MemoryItem
	for _, kind := range []string{"stable_fact", "operational_pattern", "watch_item", "hypothesis"} {
		var items []*MemoryItem
		for _, it := range s.FilterItems(subject, kind) {
			if !isPrivateItem(it.Kind, it.Scope) {
				items = append(items, it)
			}
		}
		report.Scanned += len(items)
		if len(items) < 2 {
			continue
		}
		sort.SliceStable(items, func(i, j int) bool {
			if !items[i].UpdatedAt.Equal(items[j].UpdatedAt) {
				return items[i].UpdatedAt.After(items[j].UpdatedAt)
			}
			return items[i].ID < items[j].ID
		})
		for _, cluster := range clusterNearDuplicates(items) {
			if len(cluster) < 2 {
				continue
			}
			keep := cluster[0]
			for _, c := range cluster[1:] {
				if betterFact(c, keep) {
					keep = c
				}
			}
			group := ConsolidationGroup{Kind: kind, Keep: keep.ID, KeepTitle: keep.Title}
			for _, c := range cluster {
				if c == keep {
					continue
				}
				score, _ := nearDuplicate(keep, c)
				group.Superseded = append(group.Superseded, c.ID)
				group.Scores = append(group.Scores, score)
			}
			report.Groups = append(report.Groups, group)
			report.Superseded += len(group.Superseded)
			if !apply {
				continue
			}
			kept := *keep
			for _, c := range cluster {
				if c == keep {
					continue
				}
				cc := *c
				changed = append(changed, s.retire(&cc, "superseded"))
				if !containsString(kept.Supersedes, c.ID) {
					kept.Supersedes = append(append([]string(nil), kept.Supersedes...), c.ID)
				}
			}
			kept.UpdatedAt = time.Now()
			s.mu.Lock()
			s.items[kept.ID] = &kept
			s.mu.Unlock()
			changed = append(changed, &kept)
		}
	}
	return report, changed
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
