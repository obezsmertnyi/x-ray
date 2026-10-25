package xray

import (
	"math"
	"strings"
	"unicode/utf8"
)

// Distinctiveness weighting for the lexical channel. A conversational query
// ("what is wrong with the big query") is a few filler tokens and one that names
// the thing asked about. Weighing every token by whether it matched, and not by
// how much matching it says, lets a cell that happens to contain the filler words
// outrank the one that mentions the entity. Each query token is weighted by a
// smoothed inverse document frequency computed from the candidate set, so a
// token nearly every cell contains counts for little whether it matches or not,
// and a token few cells contain dominates both the match bonus and the miss
// penalty.

// idfWeight is log(1 + n/df), the smoothed inverse document frequency. n is the
// size of the candidate set and df the number of candidates that contain the
// token as a word.
//
//   - df == n (every cell has it): the minimum, log 2, as uninformative as a
//     token gets.
//   - df small: grows as df shrinks toward 1.
//   - df <= 0 (no cell has it as a word, the usual case for filler against a
//     corpus of technical notes): floored to n, not to 1. Add-one smoothing
//     would give an unseen token the largest weight, backwards here: a token the
//     corpus knows nothing about cannot be its distinguishing feature, and
//     several such tokens summed would outweigh one rare entity that a cell does
//     match.
func idfWeight(df, n int) float64 {
	if n <= 0 {
		return 1
	}
	if df <= 0 {
		df = n
	}
	return math.Log1p(float64(n) / float64(df))
}

// tokenWeights computes idfWeight once per distinct token of toks against the
// document frequencies df over n candidates. nil for no tokens.
func tokenWeights(toks []string, df map[string]int, n int) map[string]float64 {
	if len(toks) == 0 {
		return nil
	}
	w := make(map[string]float64, len(toks))
	for _, t := range toks {
		if _, ok := w[t]; !ok {
			w[t] = idfWeight(df[t], n)
		}
	}
	return w
}

// tokenWeight looks t up in weights, defaulting to 1.0 (uniform) when weights is
// nil or has no entry: a nil map reproduces the unweighted scoring exactly.
func tokenWeight(weights map[string]float64, t string) float64 {
	if w, ok := weights[t]; ok {
		return w
	}
	return 1.0
}

// wCoverageMassThreshold is the bar an item that fails the count-based coverage
// gate must clear to be admitted anyway: the tokens it did match must carry at
// least this share of the query's weight. Purely additive: an item that clears
// the count gate is admitted as before, so this only ever adds candidates. A
// single rare-entity match against four filler tokens clears it; an item
// matching nothing distinctive does not.
const wCoverageMassThreshold = 0.35

// docFreqs counts, for each query token, the candidates that contain it as a
// whole word of their title or haystack.
func docFreqs(cands []*MemoryItem, toks []string) map[string]int {
	if len(toks) == 0 {
		return nil
	}
	want := make(map[string]struct{}, len(toks))
	for _, t := range toks {
		want[t] = struct{}{}
	}
	df := make(map[string]int, len(toks))
	for _, it := range cands {
		seen := make(map[string]struct{}, len(toks))
		for _, field := range [2]string{strings.ToLower(it.Title), strings.ToLower(itemHaystack(it))} {
			for _, w := range strings.Fields(field) {
				if _, ok := want[w]; ok {
					seen[w] = struct{}{}
				}
			}
		}
		for w := range seen {
			df[w]++
		}
	}
	return df
}

// weightedMatchFraction is the share of the query's weight that the tokens a
// title and haystack match account for. It tests the predicate the count gate
// uses, so the two always agree on which tokens matched.
func weightedMatchFraction(nt, nc string, toks []string, weights map[string]float64) float64 {
	if len(toks) == 0 {
		return 1
	}
	var matched, total float64
	for _, t := range toks {
		w := tokenWeight(weights, t)
		total += w
		if matchesKWToken(nt, t) || matchesKWToken(nc, t) {
			matched += w
		}
	}
	if total <= 0 {
		return 0
	}
	return matched / total
}

// itemHaystack widens an item's content with its own id for lexical matching, so
// a query that is, or contains, a cell's id can reach it. Ids the store minted
// itself (16 hex digits) say nothing and are left out. Only the matching sees
// this; the item is never changed.
func itemHaystack(it *MemoryItem) string {
	if it.ID == "" || isGeneratedID(it.ID) {
		return it.Content
	}
	return it.Content + " " + it.ID
}

// isGeneratedID reports whether id is one newID produced.
func isGeneratedID(id string) bool {
	if len(id) != 16 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// asciiOnly reports whether s holds only single-byte runes, in which case byte
// indexing is rune indexing and a DP can skip the []rune conversion.
func asciiOnly(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

func absDiff(a, b int) int {
	if a > b {
		return a - b
	}
	return b - a
}

// editDistanceWithin reports whether the Levenshtein distance between a and b is
// at most max: editDistance(a, b) <= max for every input, without the []rune
// conversion in the ASCII case and giving up as soon as the answer cannot be
// within max. The early exit is sound because row minimums of the DP are
// non-decreasing: once one exceeds max, so does the final distance.
func editDistanceWithin(a, b string, max int) bool {
	if a == b {
		return true
	}
	if max <= 0 {
		return false
	}
	if asciiOnly(a) && asciiOnly(b) {
		if absDiff(len(a), len(b)) > max {
			return false
		}
		return withinDP(len(a), len(b), max, func(i, j int) bool { return a[i] == b[j] })
	}
	ra, rb := []rune(a), []rune(b)
	if absDiff(len(ra), len(rb)) > max {
		return false
	}
	return withinDP(len(ra), len(rb), max, func(i, j int) bool { return ra[i] == rb[j] })
}

// withinDP is the row-by-row edit-distance DP over la x lb cells with the
// row-minimum early exit; same(i, j) says whether a[i] equals b[j].
func withinDP(la, lb, max int, same func(i, j int) bool) bool {
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 0; i < la; i++ {
		curr[0] = i + 1
		rowMin := curr[0]
		for j := 0; j < lb; j++ {
			cost := 1
			if same(i, j) {
				cost = 0
			}
			v := min3(curr[j]+1, prev[j+1]+1, prev[j]+cost)
			curr[j+1] = v
			if v < rowMin {
				rowMin = v
			}
		}
		if rowMin > max {
			return false
		}
		prev, curr = curr, prev
	}
	return prev[lb] <= max
}
