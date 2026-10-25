package xray

import (
	"strings"
	"sync"
)

// trigramIndex is an in-memory inverted index that maps 3-char grams of
// individual words to item IDs. It accelerates KeywordSearch by letting a
// query token match a haystack word when they share enough trigrams — catching
// no-space variants ("BillingREC" → "Billing REC") and partial prefixes.
//
// Each item is indexed per-word, not as a concatenated blob, so "goroutine"
// does not accidentally match "coroutines" just because the letters overlap.
type trigramIndex struct {
	mu      sync.RWMutex
	posting map[string]map[string]struct{} // trigram → set of item IDs
	words   map[string][]string            // item ID → indexed words
}

func newTrigramIndex() *trigramIndex {
	return &trigramIndex{
		posting: make(map[string]map[string]struct{}),
		words:   make(map[string][]string),
	}
}

// trigrams returns all 3-char substrings of s (or s itself if shorter).
func trigrams(s string) []string {
	r := []rune(s)
	if len(r) < 3 {
		return []string{s}
	}
	out := make([]string, 0, len(r)-2)
	for i := 0; i <= len(r)-3; i++ {
		out = append(out, string(r[i:i+3]))
	}
	return out
}

// index adds or replaces an item. Words from title and content are indexed
// individually so long-word trigrams only match the same word, not adjacent words.
func (idx *trigramIndex) index(id, title, content string) {
	words := strings.Fields(strings.ToLower(title + " " + content))

	idx.mu.Lock()
	defer idx.mu.Unlock()

	// Remove old postings for this ID.
	if old, ok := idx.words[id]; ok {
		for _, w := range old {
			for _, g := range trigrams(w) {
				delete(idx.posting[g], id)
			}
		}
	}
	idx.words[id] = words
	for _, w := range words {
		for _, g := range trigrams(w) {
			if idx.posting[g] == nil {
				idx.posting[g] = make(map[string]struct{})
			}
			idx.posting[g][id] = struct{}{}
		}
	}
}

// remove deletes an item from the index.
func (idx *trigramIndex) remove(id string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if old, ok := idx.words[id]; ok {
		for _, w := range old {
			for _, g := range trigrams(w) {
				delete(idx.posting[g], id)
			}
		}
		delete(idx.words, id)
	}
}

// tokThreshold returns the overlap fraction a token must clear to count as a
// trigram match, scaled by token length:
//   - short (≤4 chars, 1-2 trigrams): 1.0 — must match exactly (token IS its trigram)
//   - medium (5-6 chars, 3-4 trigrams): 0.75 — catches "billng"→"billing" (3/4), rejects distant words
//   - long (≥7 chars, 5+ trigrams): 0.90 — tight; prevents "goroutine"↔"coroutines" false positives
func tokThreshold(nTrigrams int) float64 {
	switch {
	case nTrigrams <= 2:
		return 1.0
	case nTrigrams <= 4:
		return 0.75
	default:
		return 0.90
	}
}

// tokOverlapLocked is tokOverlap without locking; the caller must hold idx.mu.
func (idx *trigramIndex) tokOverlapLocked(id, tok string) float64 {
	tgrams := trigrams(tok)
	if len(tgrams) == 0 {
		return 0
	}
	words, ok := idx.words[id]
	if !ok {
		return 0
	}
	best := 0.0
	for _, w := range words {
		wtg := make(map[string]struct{}, len(w))
		for _, g := range trigrams(w) {
			wtg[g] = struct{}{}
		}
		n := 0
		for _, g := range tgrams {
			if _, ok := wtg[g]; ok {
				n++
			}
		}
		if frac := float64(n) / float64(len(tgrams)); frac > best {
			best = frac
			if best >= 1.0 {
				break
			}
		}
	}
	return best
}

// tokMatchesItem returns true if tok clears its length-scaled overlap threshold
// against any single word in the item — the boolean gate built on tokOverlap.
// Caller must hold idx.mu (used by search under RLock).
func (idx *trigramIndex) tokMatchesItem(id, tok string) bool {
	return idx.tokOverlapLocked(id, tok) >= tokThreshold(len(trigrams(tok)))
}

// itemScore returns the mean per-token trigram overlap (0..1) for the item
// across all query tokens — the continuous relevance signal used by lexScore.
// Acquires the read lock once for the whole token set.
func (idx *trigramIndex) itemScore(id string, toks []string) float64 {
	if len(toks) == 0 {
		return 0
	}
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	sum := 0.0
	for _, t := range toks {
		sum += idx.tokOverlapLocked(id, t)
	}
	return sum / float64(len(toks))
}

// search returns item IDs where every query token trigram-matches at least one
// word in the item. Used as a pre-filter before exact/fuzzy matchesKW.
func (idx *trigramIndex) search(query string) []string {
	toks := kwTokens(normalizeQuery(query))
	if len(toks) == 0 {
		return nil
	}

	// Collect candidate IDs from the first token's trigrams.
	firstTok := toks[0]
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	candidates := make(map[string]struct{})
	for _, g := range trigrams(firstTok) {
		for id := range idx.posting[g] {
			candidates[id] = struct{}{}
		}
	}

	// Filter: each candidate must trigram-match every token.
	for id := range candidates {
		for _, tok := range toks {
			if !idx.tokMatchesItem(id, tok) {
				delete(candidates, id)
				break
			}
		}
	}

	out := make([]string, 0, len(candidates))
	for id := range candidates {
		out = append(out, id)
	}
	return out
}
