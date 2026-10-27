package memstore

import "strings"

// memoryDedupStopWords are common English connective words dropped from the
// token set used to compare memory items for near-duplication. They carry no
// discriminating signal and would inflate similarity between two items that
// merely share ordinary sentence structure rather than the same fact.
var memoryDedupStopWords = map[string]struct{}{
	"the": {}, "and": {}, "for": {}, "are": {}, "with": {}, "this": {},
	"that": {}, "from": {}, "not": {}, "has": {}, "have": {}, "been": {},
	"its": {}, "into": {}, "due": {}, "when": {}, "while": {}, "should": {},
	"these": {}, "those": {}, "than": {}, "then": {}, "also": {}, "but": {},
	"any": {}, "all": {}, "per": {}, "via": {}, "was": {}, "were": {},
	"will": {}, "can": {},
}

// memoryDedupPunctuation is stripped from normalized text before it is
// tokenized for near-duplicate comparison. NormalizeQuery only handles CamelCase boundaries and the "_"/"-" separators; quoting and
// sentence punctuation would otherwise stay glued to the adjacent token
// (a quoted name, a trailing comma, a parenthesis) and keep two paraphrases
// of the same fact from sharing that token.
const memoryDedupPunctuation = ".,:;()'\"`"

// DedupTokens returns the normalized token set used to compare item's
// title+content against another item's for near-duplication. Stop words are
// dropped, and so are tokens shorter than three characters unless they carry a
// digit: "q3" and "q4" are what tell two otherwise identical plans apart.
func DedupTokens(item Item) map[string]struct{} {
	text := NormalizeQuery(item.Title + " " + item.Content)
	text = strings.Map(func(r rune) rune {
		if strings.ContainsRune(memoryDedupPunctuation, r) {
			return -1
		}
		return r
	}, text)

	out := make(map[string]struct{})
	for _, tok := range lexFields(text) {
		if len(tok) < 3 && !strings.ContainsAny(tok, "0123456789") {
			continue
		}
		if _, stop := memoryDedupStopWords[tok]; stop {
			continue
		}
		out[tok] = struct{}{}
	}
	return out
}

// DedupComparable reports whether a and b are eligible to be scored for
// near-duplication at all. Two items must share a Kind, a Scope and a Subject
// (compared case-insensitively), and must carry the same partition tag
// (PartitionFromTags). An item with no partition tag only compares against
// another untagged item: "" is a distinct identity, not a wildcard. When both
// items carry a service/ tag (ServiceTagValue) the values must match; when at
// most one side carries the tag the items are still comparable, because a
// service-scoped restatement of an unscoped fact is exactly the case the
// read-side collapse needs to catch. A model-written b is never comparable to a
// human-written a, so a restated claim cannot absorb a person's note.
//
// a is the existing side of the comparison in every caller: FindNearDuplicate
// scores each candidate (a) against the incoming item (b), and
// ClusterNearDuplicates scores a cluster's first member (a) against each item
// tested for membership (b). So a is where "is this a safe merge target" lives.
//
// A candidate (a) tagged SharedTag is never comparable, so it is never a merge
// target. SharedTag is a trust mark set by a privileged writer; it makes a cell
// visible to every partition, and an ordinary write must never rewrite that
// content in place through dedup. Such a cell can still be updated by naming
// its ID, which goes through the explicit existing-ID path rather than a
// near-duplicate scan.
//
// A candidate (a) tagged author/<identity> is comparable only against an
// incoming item (b) carrying the identical author/<identity> tag. The tag marks
// the content as that identity's own claim, so only a restatement from the same
// identity may fold into it. An incoming item with no author tag, or a different
// one, always mints its own cell. A machine rewrite strips author tags from its
// own items before they reach this comparison.
func DedupComparable(a, b Item) bool {
	if a.Kind != b.Kind {
		return false
	}
	if HasSharedTag(a.Tags) {
		return false
	}
	if aAuthor, ok := AuthorTagValue(a); ok {
		bAuthor, bOK := AuthorTagValue(b)
		if !bOK || bAuthor != aAuthor {
			return false
		}
	}
	if a.Scope != b.Scope || !strings.EqualFold(a.Subject, b.Subject) {
		return false
	}
	if effectiveOrigin(a.Origin) == OriginHuman && effectiveOrigin(b.Origin) == OriginDerived {
		return false
	}
	if PartitionFromTags(a.Tags) != PartitionFromTags(b.Tags) {
		return false
	}
	aSvc, aOK := ServiceTagValue(a)
	bSvc, bOK := ServiceTagValue(b)
	if aOK && bOK && aSvc != bSvc {
		return false
	}
	return true
}

// MemoryNearDupThreshold is the Jaccard token-set similarity at or above
// which two comparable memory items are treated as one fact restated rather
// than two distinct facts. It must sit strictly above the similarity two
// items merely discussing the same subject can share by vocabulary alone,
// and at or below the similarity any genuine paraphrase of one fact shares
// with another. It is the only number this file tunes; the fixture in
// neardup_test.go is what tunes it.
const MemoryNearDupThreshold = 0.3

// NearDupMinTokens is the token-set size below which Jaccard
// similarity stops being a meaningful signal: two short, unrelated items can
// share enough of their (few) tokens to clear MemoryNearDupThreshold by
// coincidence rather than by restating the same fact. Below this size (the
// smaller of the two token sets), only near-identical text
// (memoryNearDupShortTextThreshold) counts as a duplicate — the ordinary
// threshold and the containment rule below both require both sets to have
// reached this size first.
const NearDupMinTokens = 8

// memoryNearDupShortTextThreshold is the Jaccard similarity required for two
// items to be treated as duplicates when the smaller token set has fewer
// than NearDupMinTokens tokens.
const memoryNearDupShortTextThreshold = 0.8

// nearDupContainment is the share of the smaller token set that must lie in the
// larger one for a short restatement of a long fact to count as a duplicate
// although the union dilutes its Jaccard score.
const nearDupContainment = 0.9

// NearDuplicateMemory scores a and b for near-duplication. It returns
// (0, false) when the two items are not comparable (DedupComparable)
// or when either has an empty token set. Otherwise score is the Jaccard
// similarity of their token sets.
//
// When the smaller token set is below NearDupMinTokens, dup is set
// only by memoryNearDupShortTextThreshold — near-identical text — since
// ordinary similarity on a handful of tokens is not a reliable signal.
//
// Otherwise dup is true once score reaches MemoryNearDupThreshold. A pure
// Jaccard score under-counts one specific shape beyond that: a short
// restatement of a long fact, where the union is dominated by the longer
// side's extra tokens and the ratio never clears the threshold even though
// the short side is wholly contained in the long one. So dup is also set
// when at least 90% of the smaller token set is contained in the larger set.
func NearDuplicateMemory(a, b Item) (score float64, dup bool) {
	if !DedupComparable(a, b) {
		return 0, false
	}
	ta := DedupTokens(a)
	tb := DedupTokens(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0, false
	}

	inter := 0
	for tok := range ta {
		if _, ok := tb[tok]; ok {
			inter++
		}
	}
	union := len(ta) + len(tb) - inter
	score = float64(inter) / float64(union)

	small, large := ta, tb
	if len(large) < len(small) {
		small, large = large, small
	}

	if len(small) < NearDupMinTokens {
		return score, score >= memoryNearDupShortTextThreshold
	}

	dup = score >= MemoryNearDupThreshold
	if dup {
		return score, dup
	}

	containInter := 0
	for tok := range small {
		if _, ok := large[tok]; ok {
			containInter++
		}
	}
	if float64(containInter)/float64(len(small)) >= nearDupContainment {
		dup = true
	}
	return score, dup
}

// FindNearDuplicate returns the highest-scoring active near-duplicate of
// item among candidates. Candidates that are not Status == "active", or that
// share item's ID, are ignored — an item never duplicates itself, and a
// superseded/expired cell is not a live target to update in place.
func FindNearDuplicate(candidates []Item, item Item) (best Item, score float64, ok bool) {
	for _, c := range candidates {
		if c.Status != "active" || c.ID == item.ID {
			continue
		}
		s, dup := NearDuplicateMemory(c, item)
		if !dup {
			continue
		}
		if !ok || s > score {
			best, score, ok = c, s, true
		}
	}
	return best, score, ok
}

// ClusterNearDuplicates groups items into near-duplicate clusters with a
// single greedy pass that preserves input order: each item joins the first
// existing cluster whose FIRST member it near-duplicates, else it starts a
// new cluster of its own. Comparing only against each cluster's first member
// (not every member already in it) keeps the pass linear in the number of
// clusters rather than quadratic in cluster size, and gives every cluster a
// stable representative — its first member — for a caller that needs to
// pick one item to keep.
func ClusterNearDuplicates(items []Item) [][]Item {
	var clusters [][]Item
	for _, item := range items {
		placed := false
		for i, cluster := range clusters {
			if _, dup := NearDuplicateMemory(cluster[0], item); dup {
				clusters[i] = append(clusters[i], item)
				placed = true
				break
			}
		}
		if !placed {
			clusters = append(clusters, []Item{item})
		}
	}
	return clusters
}
