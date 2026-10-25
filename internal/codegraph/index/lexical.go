package index

import (
	"sort"
	"strings"
	"sync"
)

// Lexical (keyword) retrieval for the graph index, fused with the semantic
// channel by Reciprocal Rank Fusion in Search.
//
// Why this exists: cosine similarity over a 256-dim MRL-truncated embedding
// loses queries whose target is phrased in different vocabulary, even when the
// query's exact terms appear repeatedly in the target document. Measured on the
// Claude Code memory corpus (docs/todo/cc-memory-eval.md): "I edited the widget
// cli source, what do I need to run?" missed a document containing `widget` ×6,
// `cli` ×6 and `build-cli` ×4, losing to a document about the same topic. That
// is a missing keyword channel, not a ranking-weight problem — no threshold on
// a similarity-only signal recovers a term the embedding never encoded.
//
// Ported from an earlier in-house hybrid-search implementation. The
// scoring constants and the trigram thresholds are kept identical to it so the
// two stay comparable; the adaptation is that items here are graph nodes keyed
// by QN rather than memory cells keyed by ID.

// trigramIndex is an in-memory inverted index over the corpus VOCABULARY: 3-char
// grams point at the words containing them, and words point at the node QNs
// using them. A query token matches a node when it shares enough trigrams with
// one of that node's words, which catches no-space variants and typos.
//
// The two-level shape is what makes the gate cheap. Pointing grams straight at
// documents does not discriminate in a code corpus: 8,830 Go declarations
// produce only 8,291 DISTINCT trigrams — fewer grams than documents — and the
// common ones are everywhere. "fun" and "unc" sit in 100% of them because every
// declaration contains the word func; "ing" 92%, "the" 78%. A nine-word query
// therefore walked 73,261 document postings, 8.3x the corpus, and grew linearly
// with the repo. The same corpus holds 25,432 distinct WORDS, so the identical
// query touches 3,312 vocabulary entries, 0.38x the corpus: "fun" is in 100% of
// the documents but in only a handful of the words.
//
// Words are matched per-word rather than as one concatenated blob, so
// "goroutine" does not match "coroutines" merely because their letters overlap.
// Keying by word also removes the per-node gram cache an earlier form kept: a
// word's trigram set is now shared by every node using it instead of being
// rebuilt and held once per occurrence.
type trigramIndex struct {
	mu        sync.RWMutex
	words     map[string][]string            // QN → indexed words
	wordDocs  map[string]map[string]struct{} // word → set of QNs
	gramWords map[string]map[string]struct{} // trigram → set of words containing it

	// titleDocs maps a normalized title to the QNs carrying it; titles remembers
	// each QN's, so a re-index can retract the old one. Backs the exact-title
	// channel in Search.
	titleDocs map[string]map[string]struct{}
	titles    map[string]string
}

func newTrigramIndex() *trigramIndex {
	return &trigramIndex{
		words:     make(map[string][]string),
		wordDocs:  make(map[string]map[string]struct{}),
		gramWords: make(map[string]map[string]struct{}),
		titleDocs: make(map[string]map[string]struct{}),
		titles:    make(map[string]string),
	}
}

// trigrams returns all 3-char substrings of s, or s itself when shorter.
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

// reset drops every posting. Callers rebuilding the whole index take the write
// lock once via this rather than deleting item by item.
func (idx *trigramIndex) reset() {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.words = make(map[string][]string)
	idx.wordDocs = make(map[string]map[string]struct{})
	idx.gramWords = make(map[string]map[string]struct{})
	idx.titleDocs = make(map[string]map[string]struct{})
	idx.titles = make(map[string]string)
}

// index adds or replaces one node's postings. Trigrams are computed only for a
// word the corpus has not seen before, so indexing cost tracks vocabulary
// growth rather than corpus size — the later files of a repo are nearly free
// because they reuse words the earlier ones already introduced.
func (idx *trigramIndex) index(qn, title, content string) {
	words := lexFields(strings.ToLower(title + " " + content))

	idx.mu.Lock()
	defer idx.mu.Unlock()

	if old, ok := idx.words[qn]; ok {
		for _, w := range old {
			idx.dropWordLocked(w, qn)
		}
	}
	if old, ok := idx.titles[qn]; ok {
		if docs := idx.titleDocs[old]; docs != nil {
			delete(docs, qn)
			if len(docs) == 0 {
				delete(idx.titleDocs, old)
			}
		}
	}
	if nt := normalizeQuery(title); nt != "" {
		idx.titles[qn] = nt
		docs, seen := idx.titleDocs[nt]
		if !seen {
			docs = make(map[string]struct{})
			idx.titleDocs[nt] = docs
		}
		docs[qn] = struct{}{}
	}
	idx.words[qn] = words
	for _, w := range words {
		docs, seen := idx.wordDocs[w]
		if !seen {
			docs = make(map[string]struct{})
			idx.wordDocs[w] = docs
			for _, g := range trigrams(w) {
				ws := idx.gramWords[g]
				if ws == nil {
					ws = make(map[string]struct{})
					idx.gramWords[g] = ws
				}
				ws[w] = struct{}{}
			}
		}
		docs[qn] = struct{}{}
	}
}

// exactTitle returns the QNs whose title, normalized the same way the query is,
// equals the query exactly. Sorted so the result is deterministic when several
// nodes share a title.
func (idx *trigramIndex) exactTitle(normQuery string) []string {
	if normQuery == "" {
		return nil
	}
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	docs := idx.titleDocs[normQuery]
	if len(docs) == 0 {
		return nil
	}
	out := make([]string, 0, len(docs))
	for qn := range docs {
		out = append(out, qn)
	}
	sort.Strings(out)
	return out
}

// dropWordLocked removes one (word, node) pair, and the word itself once no
// node uses it. The vocabulary is garbage-collected rather than left to grow:
// a long-lived writable server re-indexes the same files repeatedly, and every
// renamed identifier would otherwise stay in gramWords forever.
// Caller must hold idx.mu for writing.
func (idx *trigramIndex) dropWordLocked(w, qn string) {
	docs, ok := idx.wordDocs[w]
	if !ok {
		return // already dropped — a node may list the same word twice
	}
	delete(docs, qn)
	if len(docs) > 0 {
		return
	}
	delete(idx.wordDocs, w)
	for _, g := range trigrams(w) {
		ws := idx.gramWords[g]
		delete(ws, w)
		if len(ws) == 0 {
			delete(idx.gramWords, g)
		}
	}
}

// tokenGrams precomputes each query token's trigrams once, so scanning N
// candidates does not recompute them N times.
func tokenGrams(toks []string) [][]string {
	out := make([][]string, len(toks))
	for i, t := range toks {
		out[i] = trigrams(t)
	}
	return out
}

// tokThreshold is the overlap fraction a token must clear to count as a match,
// scaled by token length: short tokens must match exactly (the token IS its
// trigram), medium tokens allow one substitution, long tokens stay tight to
// prevent near-miss collisions between long similar words.
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

// lexCand is one candidate's lexical evidence: how many query tokens it matched
// and its mean per-token trigram overlap.
type lexCand struct {
	matched int
	frac    float64
}

// minCoverage is the number of query tokens a candidate must match to be
// considered at all. Requiring *every* token is wrong for natural-language
// questions: "who is supposed to write the implementation code on this project?"
// carries the token "supposed", which the answering memory does not contain even
// though it contains every other token several times over. An all-tokens gate
// therefore rejects exactly the document it should return. Requiring roughly
// two-thirds keeps the gate meaningful while tolerating the incidental words a
// question drags along; full coverage still wins on rank via the miss penalty in
// lexScore.
func minCoverage(nToks int) int {
	if nToks <= 2 {
		return nToks // short queries are all signal — demand all of it
	}
	n := (nToks*2 + 2) / 3 // ceil(2n/3)
	if n < 2 {
		n = 2
	}
	return n
}

// candidates returns QNs matching at least minCoverage of the query tokens,
// with each one's matched-token count and mean overlap fraction, plus how many
// candidates the minCoverage gate then REJECTED (matched at least one query
// token, but too few of them to clear minCoverage). The rejection count is the
// direct measure of how often that gate actually does something, as opposed to
// every candidate sailing through. One read lock covers the whole scan.
//
// The scan works on the vocabulary and reaches documents only at the end. For
// each token it finds the words that could match, scores each ONCE, and expands
// only the survivors to the nodes using them. Nothing is approximate: a node's
// score for a token is the best fraction over its matching words, exactly what
// a per-node walk computes, reached without touching the words a node holds
// that the query never mentioned. On this repo's own 8,830-declaration corpus a
// six-token query expands 1,523 word-to-node entries, 0.17x the corpus, where
// walking document postings touched 12,723.
//
// Finding those words is where the cost actually sits, and unioning the token's
// whole trigram list is the wrong way to do it — the vocabulary is 3.2x the
// corpus (28,547 words), and a common gram points at thousands of words, so the
// union costs MORE than the document postings it replaced (21,351 entries for
// that same query). Pigeonhole removes it. A word clearing tokThreshold shares
// at least `need` of the token's `k` trigram positions, so it can lack at most
// k-need of them; probing any k-need+1 positions therefore cannot miss it. The
// probe takes the RAREST positions, and the thresholds keep it small: 0.90
// demands every gram up to k=9, so the probe is a single posting for tokens
// shorter than 12 characters, and since 9/10 already clears 0.90 it grows by
// one posting per further ten grams (two at k=10, three at k=20); the 0.75 bar
// for short tokens means one posting at k=3 and two at k=4. Candidate words
// found there are then verified against all k positions, which is what makes
// the pruning exact rather than a heuristic.
//
// The form this replaced walked documents. Seeding unioned every trigram of
// every token, which on a 12,972-node map admitted 10,900 candidates per query
// (84% of the corpus), and scoring each one then walked every word of the node:
// ~41M map probes per query, 235ms measured, linear in BOTH corpus size and
// query length — the worst possible shape, since agents ask long questions. An
// upper-bound prefilter and a dense-gram skip cut that to 20ms by dropping most
// of the seed before scoring it, but both were workarounds for grams that do
// not discriminate between documents.
func (idx *trigramIndex) candidates(tokGrams [][]string) (map[string]lexCand, int) {
	if len(tokGrams) == 0 {
		return nil, 0
	}
	need := minCoverage(len(tokGrams))

	idx.mu.RLock()
	defer idx.mu.RUnlock()

	acc := make(map[string]lexCand)
	var lists []map[string]struct{}   // one word set per trigram POSITION of the token
	var order []int                   // those positions, rarest first
	seen := make(map[string]struct{}) // words already verified for this token
	best := make(map[string]float64)  // qn → best matching word's overlap, this token
	for _, tg := range tokGrams {
		k := len(tg)
		if k == 0 {
			continue
		}
		thr := tokThreshold(k)

		// hits is the smallest shared-position count that clears the threshold,
		// derived by the same comparison the scoring uses so the two cannot
		// drift apart on a rounding edge.
		hits := k
		for n := 0; n <= k; n++ {
			if float64(n)/float64(k) >= thr {
				hits = n
				break
			}
		}

		lists = lists[:0]
		order = order[:0]
		for i, g := range tg {
			lists = append(lists, idx.gramWords[g])
			order = append(order, i)
		}
		sort.Slice(order, func(a, b int) bool { return len(lists[order[a]]) < len(lists[order[b]]) })
		probe := k - hits + 1
		if probe > k {
			probe = k
		}

		clear(seen)
		clear(best)
		for _, i := range order[:probe] {
			for w := range lists[i] {
				if _, done := seen[w]; done {
					continue
				}
				seen[w] = struct{}{}
				n := 0
				for _, l := range lists {
					if _, ok := l[w]; ok {
						n++
					}
				}
				f := float64(n) / float64(k)
				if f < thr {
					continue // this word cannot match; its nodes are never touched
				}
				for qn := range idx.wordDocs[w] {
					if f > best[qn] {
						best[qn] = f
					}
				}
			}
		}
		for qn, f := range best {
			c := acc[qn]
			c.matched++
			c.frac += f
			acc[qn] = c
		}
	}

	out := make(map[string]lexCand, len(acc))
	rejected := 0
	for qn, c := range acc {
		if c.matched < need {
			rejected++
			continue
		}
		out[qn] = lexCand{matched: c.matched, frac: c.frac / float64(len(tokGrams))}
	}
	return out, rejected
}

// normalizeQuery lowercases, splits CamelCase boundaries, and turns separators
// into spaces so "build-cli" and "buildCLI" both reduce to "build cli".
func normalizeQuery(q string) string {
	var sb strings.Builder
	runes := []rune(q)
	for i, r := range runes {
		if i > 0 && r >= 'A' && r <= 'Z' {
			if prev := runes[i-1]; prev >= 'a' && prev <= 'z' {
				sb.WriteByte(' ')
			}
		}
		sb.WriteRune(r)
	}
	q = strings.ToLower(sb.String())
	q = strings.ReplaceAll(q, "_", " ")
	q = strings.ReplaceAll(q, "-", " ")
	return q
}

// lexFields splits normalized text into tokens on whitespace and punctuation
// that carries no lexical signal, so "`pip install vllm`." yields "pip",
// "install", "vllm".
func lexFields(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\r', '.', ',', ';', ':', '!', '?', '"', '\'',
			'`', '(', ')', '[', ']', '{', '}', '<', '>', '|', '/', '\\', '*', '#', '=':
			return true
		}
		return false
	})
}

// lexStopwords are tokens carrying no retrieval signal. A natural-language
// question ("what do I need to run?") is mostly these, and without removing
// them the every-token-must-match gate rejects the very documents it should
// find, since no memory contains the word "need" near the word "widget".
var lexStopwords = map[string]bool{
	"a": true, "the": true, "an": true, "and": true, "or": true, "but": true,
	"is": true, "are": true, "was": true, "were": true, "be": true, "been": true, "being": true,
	"do": true, "does": true, "did": true, "doing": true, "done": true,
	"i": true, "you": true, "we": true, "it": true, "he": true, "she": true, "they": true,
	"my": true, "your": true, "our": true, "its": true, "their": true,
	"to": true, "of": true, "in": true, "on": true, "at": true, "by": true, "for": true,
	"with": true, "from": true, "into": true, "about": true, "as": true,
	"what": true, "when": true, "where": true, "which": true, "who": true, "whom": true,
	"why": true, "how": true, "that": true, "this": true, "these": true, "those": true,
	"can": true, "could": true, "should": true, "would": true, "will": true, "shall": true,
	"may": true, "might": true, "must": true, "need": true, "want": true,
	"have": true, "has": true, "had": true, "get": true, "got": true,
	"if": true, "then": true, "else": true, "so": true, "just": true, "only": true,
	"any": true, "all": true, "some": true, "no": true, "not": true, "there": true, "here": true,
	"me": true, "am": true, "run": true, "use": true, "used": true, "using": true,
}

// kwTokens splits a normalized query into content-bearing tokens. Stopwords and
// single characters are dropped. When a query is nothing but stopwords the
// result is empty and the lexical channel simply does not vote.
func kwTokens(q string) []string {
	out := make([]string, 0, 8)
	for _, p := range lexFields(q) {
		if len([]rune(p)) < 2 || lexStopwords[p] {
			continue
		}
		out = append(out, p)
	}
	return out
}

// Lexical scoring weights, identical to the original implementation. Whole-query
// title matches dominate, then graded per-token quality, with the trigram
// fraction as a tie-breaker only.
const (
	wExactTitle   = 1000
	wTitleContain = 600
	wTokExactTle  = 100
	wTokExactCnt  = 40
	wTrigram      = 20
	wMissPenalty  = 200 // per unmatched token — keeps full coverage above partial
)

// tokenMatchQuality grades one token against a node's title and content:
// title hit beats content hit, and an exact substring beats nothing.
func tokenMatchQuality(nt, nc, t string) int {
	if strings.Contains(nt, t) {
		return wTokExactTle
	}
	if strings.Contains(nc, t) {
		return wTokExactCnt
	}
	return 0
}

// lexScore ranks one node for a tokenised query. matched is the token-coverage
// count from candidates; since the gate admits partial coverage, the miss
// penalty is what keeps a document containing every token above one that merely
// cleared the threshold.
func lexScore(title, content, q string, toks []string, matched int, trigramFrac float64) int {
	nt := normalizeQuery(title)
	nc := normalizeQuery(content)

	score := 0
	if q != "" && nt == q {
		score += wExactTitle
	} else if q != "" && strings.Contains(nt, q) {
		score += wTitleContain
	}
	for _, t := range toks {
		score += tokenMatchQuality(nt, nc, t)
	}
	score += int(trigramFrac * wTrigram)
	if len(toks) > 0 && matched < len(toks) {
		score -= (len(toks) - matched) * wMissPenalty
	}
	return score
}

// rrfK damps the contribution of deep ranks in the fusion below. 60 is the
// value from the original RRF paper and the one triage uses.
const rrfK = 60

// fuseRRF combines two rankings by Reciprocal Rank Fusion: an item's fused score
// is the sum of 1/(rrfK+rank) over the lists it appears in, ranks 0-based in
// input order. RRF is used rather than a weighted score blend because the two
// channels' scores are not on comparable scales — cosine sits in a narrow band
// near 0.5-0.8 while lexScore is unbounded integers — so only their ranks can
// be meaningfully combined. An item found by both channels outranks one found
// by either alone, which is the whole point.
//
// It also partitions the returned hits by provenance — promotions (keyword
// only; sem's own ranked list, already over-fetched to searchK depth, never
// surfaced this item at all), semanticOnly, and both — since fuseRRF is the
// only place that knows both channels' membership and can classify the FINAL
// top-k this way. The three counts always sum to len(out): every hit came
// from add(sem) and/or add(lex), so it lands in exactly one bucket.
func fuseRRF(exact, lex, sem []Hit, limit int) (out []Hit, promotions, semanticOnly, both int) {
	n := len(exact) + len(lex) + len(sem)
	fused := make(map[string]float64, n)
	byQN := make(map[string]Hit, n)
	inSem := make(map[string]bool, len(sem))
	inLex := make(map[string]bool, len(lex)+len(exact))
	add := func(hits []Hit, seen map[string]bool) {
		for rank, h := range hits {
			fused[h.QN] += 1.0 / float64(rrfK+rank)
			seen[h.QN] = true
			// Keep the semantic hit when present: its Score is the cosine value
			// callers display, and lexical hits carry a synthetic score.
			if _, ok := byQN[h.QN]; !ok {
				byQN[h.QN] = h
			}
		}
	}
	add(sem, inSem) // semantic first so its Hit (with real cosine) wins the byQN slot
	add(lex, inLex)
	// Same 1/(rrfK+rank) scale, so no weights are introduced. A named node is
	// necessarily lexical rank 0 too (exact title dominates lexScore), and two
	// rank-0 votes beat the best a rival can hold: semantic 0 plus lexical 1.
	add(exact, inLex)

	qns := make([]string, 0, len(fused))
	for qn := range fused {
		qns = append(qns, qn)
	}
	sort.SliceStable(qns, func(i, j int) bool {
		if fused[qns[i]] != fused[qns[j]] {
			return fused[qns[i]] > fused[qns[j]]
		}
		return qns[i] < qns[j]
	})

	if limit <= 0 {
		limit = 10
	}
	out = make([]Hit, 0, limit)
	for _, qn := range qns {
		if len(out) >= limit {
			break
		}
		h := byQN[qn]
		h.Fused = fused[qn]
		out = append(out, h)
		switch {
		case inSem[qn] && inLex[qn]:
			both++
		case inLex[qn]:
			promotions++
		default: // inSem[qn] only — every qn in `fused` came from sem and/or lex
			semanticOnly++
		}
	}
	return out, promotions, semanticOnly, both
}
