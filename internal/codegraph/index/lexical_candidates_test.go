package index

import (
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// refIndex is the previous document-posting implementation, kept verbatim
// (minus its mutex, which these single-goroutine tests do not need) as both the
// correctness oracle and the benchmark baseline. It maps trigrams straight to
// node QNs and caches every node's per-word trigram sets, then scores with an
// upper-bound prefilter and a dense-gram skip.
//
// Word-level postings must return EXACTLY this — same candidate map, and a
// rejected count no higher — for every query, or the speedup changed ranking,
// which is the one thing it may not do.
type refIndex struct {
	posting   map[string]map[string]struct{}
	words     map[string][]string
	wordGrams map[string][]map[string]struct{}
}

func newRefIndex() *refIndex {
	return &refIndex{
		posting:   make(map[string]map[string]struct{}),
		words:     make(map[string][]string),
		wordGrams: make(map[string][]map[string]struct{}),
	}
}

func (idx *refIndex) index(qn, title, content string) {
	words := lexFields(strings.ToLower(title + " " + content))
	if old, ok := idx.words[qn]; ok {
		for _, w := range old {
			for _, g := range trigrams(w) {
				delete(idx.posting[g], qn)
			}
		}
	}
	idx.words[qn] = words
	grams := make([]map[string]struct{}, len(words))
	for i, w := range words {
		wtg := make(map[string]struct{}, len(w))
		for _, g := range trigrams(w) {
			wtg[g] = struct{}{}
			if idx.posting[g] == nil {
				idx.posting[g] = make(map[string]struct{})
			}
			idx.posting[g][qn] = struct{}{}
		}
		grams[i] = wtg
	}
	idx.wordGrams[qn] = grams
}

func (idx *refIndex) tokOverlapGrams(qn string, tgrams []string) float64 {
	if len(tgrams) == 0 {
		return 0
	}
	grams, ok := idx.wordGrams[qn]
	if !ok {
		return 0
	}
	best := 0.0
	for _, wtg := range grams {
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

// refDenseGramShare is the document-frequency cut the dense-gram skip used.
const refDenseGramShare = 0.20

func refB2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (idx *refIndex) candidates(tokGrams [][]string) (map[string]lexCand, int) {
	if len(tokGrams) == 0 {
		return nil, 0
	}
	need := minCoverage(len(tokGrams))

	total := len(idx.words)
	cut := int(float64(total) * refDenseGramShare)

	bound := make(map[string]int)
	hits := make(map[string]int)
	for _, tg := range tokGrams {
		if len(tg) == 0 {
			continue
		}
		thr := tokThreshold(len(tg))
		dense := 0
		for _, g := range tg {
			if len(idx.posting[g]) > cut {
				dense++
			}
		}
		skip := total > 0 && float64(dense)/float64(len(tg)) < thr

		clear(hits)
		for _, g := range tg {
			if skip && len(idx.posting[g]) > cut {
				continue
			}
			for qn := range idx.posting[g] {
				hits[qn]++
			}
		}
		for qn, n := range hits {
			if _, seeded := bound[qn]; !seeded {
				bound[qn] = 0
			}
			if float64(n+dense*refB2i(skip))/float64(len(tg)) >= thr {
				bound[qn]++
			}
		}
	}

	out := make(map[string]lexCand)
	rejected := 0
	for qn, ub := range bound {
		if ub < need {
			rejected++
			continue
		}
		matched, sum := 0, 0.0
		for _, tg := range tokGrams {
			f := idx.tokOverlapGrams(qn, tg)
			if f >= tokThreshold(len(tg)) {
				matched++
				sum += f
			}
		}
		if matched >= need {
			out[qn] = lexCand{matched: matched, frac: sum / float64(len(tokGrams))}
		} else {
			rejected++
		}
	}
	return out, rejected
}

// corpusWords are shaped like what nodeText actually indexes: Go identifiers
// split on CamelCase, path fragments, and the prose that doc comments and
// bodies carry. Near-miss pairs (search/searches, index/indexer) are deliberate
// — they are what the trigram thresholds exist to separate.
var corpusWords = []string{
	"search", "searches", "searching", "index", "indexer", "indexes", "lexical",
	"trigram", "posting", "candidate", "candidates", "coverage", "reject",
	"rejected", "embed", "embedding", "embedder", "vector", "vectors", "node",
	"nodes", "graph", "edge", "edges", "resolve", "resolver", "resolved",
	"tenant", "tenants", "incident", "incidents", "memory", "cell", "cells",
	"snapshot", "registry", "evict", "eviction", "idle", "ttl", "fuse", "fusion",
	"reciprocal", "rank", "score", "scoring", "threshold", "gate", "walk",
	"internal", "engine", "parser", "mcpserver", "handler", "request", "response",
	"config", "watcher", "reconcile", "controller", "cluster", "manifest",
	"returns", "true", "false", "when", "every", "which", "before", "after",
	"operator", "event", "events", "status", "floor", "bucket", "window",
}

// rareWord builds an identifier unlikely to share trigrams with the common
// vocabulary. Without a long tail of these the corpus has no selectivity at all
// and every measurement made on it is meaningless — which is exactly what the
// first version of this generator produced. Real code corpora have the tail:
// 8,830 Go declarations carry 25,432 distinct words.
func rareWord(rng *rand.Rand) string {
	const alpha = "bcdfghjklmnpqrstvwxyz"
	b := make([]byte, 7)
	for i := range b {
		b[i] = alpha[rng.Intn(len(alpha))]
	}
	return string(b)
}

// testDoc is one synthetic node. The corpus is built once and fed to both
// implementations, so a difference between them can only come from the code.
type testDoc struct{ qn, title, body string }

func testCorpus(nodes, wordsPer int, seed int64) []testDoc {
	rng := rand.New(rand.NewSource(seed))
	docs := make([]testDoc, 0, nodes)
	for i := 0; i < nodes; i++ {
		body := make([]byte, 0, wordsPer*9)
		for w := 0; w < wordsPer; w++ {
			// Roughly a third rare, so selective words exist alongside the
			// dense ones the common vocabulary produces.
			if w%3 == 0 {
				body = append(body, rareWord(rng)...)
			} else {
				body = append(body, corpusWords[rng.Intn(len(corpusWords))]...)
			}
			body = append(body, ' ')
		}
		docs = append(docs, testDoc{
			qn: fmt.Sprintf("repo//internal/pkg%d/file%d.go::Sym%d", i%23, i%311, i),
			title: fmt.Sprintf("Func %s%d in internal/pkg%d/file%d.go",
				corpusWords[rng.Intn(len(corpusWords))], i%97, i%23, i%311),
			body: string(body),
		})
	}
	return docs
}

func buildTestIndex(t testing.TB, nodes, wordsPer int, seed int64) *trigramIndex {
	t.Helper()
	idx := newTrigramIndex()
	for _, d := range testCorpus(nodes, wordsPer, seed) {
		idx.index(d.qn, d.title, d.body)
	}
	return idx
}

func buildRefIndex(t testing.TB, nodes, wordsPer int, seed int64) *refIndex {
	t.Helper()
	idx := newRefIndex()
	for _, d := range testCorpus(nodes, wordsPer, seed) {
		idx.index(d.qn, d.title, d.body)
	}
	return idx
}

// testQueries spans the shapes minCoverage and tokThreshold branch on: one and
// two tokens (all-signal, full coverage demanded), long natural-language
// questions (two-thirds coverage), tokens that exist nowhere, and near-misses
// that must land on the right side of the overlap threshold.
var testQueries = []string{
	"search",
	"lexical",
	"trigram posting",
	"index search",
	"which nodes does the lexical trigram posting list actually admit",
	"evict a project graph that has been idle too long",
	"reciprocal rank fusion of two ranked channels",
	"reject a status request that has no recognisable tenant",
	"searching indexes resolver embedding",
	"zzzz qqqq wwww",
	"searhc lexcial trigam",
	"a",
	"of the",
	"embedder embedding embed vector vectors node nodes graph edge edges",
	"internal engine parser mcpserver handler request response config watcher",
}

// queriesFromCorpus draws real tokens out of an index, so a query is guaranteed
// to match something. testQueries alone are mostly common-vocabulary words and
// never reach the rare tail where the vocabulary actually discriminates.
func queriesFromCorpus(idx *trigramIndex, n int) []string {
	var words []string
	for _, ws := range idx.words {
		words = append(words, ws...)
	}
	sort.Strings(words)
	out := make([]string, 0, n)
	for i := 0; i < n && len(words) > 0; i++ {
		w := words[(i*7919)%len(words)]
		out = append(out, w+" "+words[(i*104729+13)%len(words)])
	}
	return out
}

func compareAgainstRef(t *testing.T, label string, idx *trigramIndex, ref *refIndex, queries []string) {
	t.Helper()
	for _, q := range queries {
		tg := tokenGrams(kwTokens(q))

		gotOut, gotRej := idx.candidates(tg)
		wantOut, wantRej := ref.candidates(tg)

		// The candidate map must match exactly. The rejected count may be
		// LOWER: a node is now only counted as rejected when it actually
		// matched a token, where the posting form counted every node that
		// shared a single trigram with one. It must never be higher — that
		// would mean something real was dropped.
		if gotRej > wantRej {
			t.Errorf("%s q=%q: rejected = %d, reference = %d (must not exceed)",
				label, q, gotRej, wantRej)
		}
		if len(gotOut) == 0 && len(wantOut) == 0 {
			continue
		}
		if !reflect.DeepEqual(gotOut, wantOut) {
			t.Errorf("%s q=%q: candidates differ (%d vs %d entries)",
				label, q, len(gotOut), len(wantOut))
			for qn, w := range wantOut {
				if g, ok := gotOut[qn]; !ok {
					t.Errorf("  MISSING %s (want matched=%d frac=%v)", qn, w.matched, w.frac)
				} else if g != w {
					t.Errorf("  DIFFERS %s: got %+v want %+v", qn, g, w)
				}
			}
			for qn := range gotOut {
				if _, ok := wantOut[qn]; !ok {
					t.Errorf("  EXTRA %s", qn)
				}
			}
		}
	}
}

func TestCandidatesMatchesReferenceExactly(t *testing.T) {
	for _, dims := range []struct{ nodes, words int }{
		{1, 5}, {12, 4}, {200, 25}, {900, 60},
	} {
		idx := buildTestIndex(t, dims.nodes, dims.words, 42)
		ref := buildRefIndex(t, dims.nodes, dims.words, 42)
		// Corpus-derived queries matter as much as the hand-written ones:
		// testQueries are common-vocabulary words, which every document holds.
		queries := append(append([]string{}, testQueries...), queriesFromCorpus(idx, 40)...)
		compareAgainstRef(t, fmt.Sprintf("nodes=%d", dims.nodes), idx, ref, queries)
	}
}

// TestCandidatesMatchesReferenceAfterReindex scores the delete path, which the
// exact-match test above never reaches: word-level postings garbage-collect the
// vocabulary on re-index, so a word left behind (or one dropped while another
// node still used it) shows up here and nowhere else.
func TestCandidatesMatchesReferenceAfterReindex(t *testing.T) {
	const nodes, words = 300, 30
	idx := buildTestIndex(t, nodes, words, 11)
	ref := buildRefIndex(t, nodes, words, 11)

	// Replace every third node's content, and give two of them IDENTICAL text
	// so a shared word is not dropped when only one of its holders changes.
	repl := testCorpus(nodes, words, 77)
	for i := 0; i < nodes; i += 3 {
		d := repl[i]
		body := d.body
		if i%9 == 0 {
			body = repl[0].body
		}
		idx.index(d.qn, d.title, body)
		ref.index(d.qn, d.title, body)
	}

	queries := append(append([]string{}, testQueries...), queriesFromCorpus(idx, 40)...)
	compareAgainstRef(t, "after reindex", idx, ref, queries)
}

// TestVocabularyStaysConsistentWithWords is the structural invariant the scan
// depends on in both directions: every word a node holds must be reachable from
// its trigrams, or real matches go missing; and nothing may survive in the
// vocabulary that no node holds any more, or a long-lived writable server
// accumulates every identifier it has ever seen.
func TestVocabularyStaysConsistentWithWords(t *testing.T) {
	const nodes, words = 250, 20
	idx := buildTestIndex(t, nodes, words, 5)
	repl := testCorpus(nodes, words, 88)
	for i := 0; i < nodes; i += 2 {
		idx.index(repl[i].qn, repl[i].title, repl[i].body)
	}

	live := make(map[string]map[string]struct{}) // word → QNs, from idx.words alone
	for qn, ws := range idx.words {
		for _, w := range ws {
			if live[w] == nil {
				live[w] = make(map[string]struct{})
			}
			live[w][qn] = struct{}{}
		}
	}

	if !reflect.DeepEqual(idx.wordDocs, live) {
		for w, docs := range live {
			if got, ok := idx.wordDocs[w]; !ok {
				t.Errorf("word %q held by %d nodes is missing from wordDocs", w, len(docs))
			} else if !reflect.DeepEqual(got, docs) {
				t.Errorf("word %q: wordDocs has %d nodes, words says %d", w, len(got), len(docs))
			}
		}
		for w := range idx.wordDocs {
			if _, ok := live[w]; !ok {
				t.Errorf("word %q survives in wordDocs but no node holds it", w)
			}
		}
	}

	for w := range live {
		for _, g := range trigrams(w) {
			if _, ok := idx.gramWords[g][w]; !ok {
				t.Errorf("word %q is unreachable from its trigram %q", w, g)
			}
		}
	}
	for g, ws := range idx.gramWords {
		if len(ws) == 0 {
			t.Errorf("trigram %q survives with no words", g)
		}
		for w := range ws {
			if _, ok := live[w]; !ok {
				t.Errorf("trigram %q still points at dead word %q", g, w)
			}
		}
	}
}

// TestCandidatesEmptyAndDegenerateTokens pins the branches a random corpus is
// unlikely to reach: no tokens at all, and a token that produced no trigrams.
func TestCandidatesEmptyAndDegenerateTokens(t *testing.T) {
	idx := buildTestIndex(t, 50, 10, 7)
	ref := buildRefIndex(t, 50, 10, 7)

	if out, rej := idx.candidates(nil); out != nil || rej != 0 {
		t.Errorf("nil tokGrams: got (%v, %d), want (nil, 0)", out, rej)
	}
	if out, rej := idx.candidates([][]string{}); out != nil || rej != 0 {
		t.Errorf("empty tokGrams: got (%v, %d), want (nil, 0)", out, rej)
	}

	// An empty trigram slice among real ones must not crash, and must agree
	// with the reference — it can never match, so it only raises the bar.
	tg := append(tokenGrams(kwTokens("search index")), []string{})
	gotOut, gotRej := idx.candidates(tg)
	wantOut, wantRej := ref.candidates(tg)
	if gotRej > wantRej || !reflect.DeepEqual(gotOut, wantOut) {
		t.Errorf("empty trigram slice: got (%d entries, %d rejected), want (%d, <=%d)",
			len(gotOut), gotRej, len(wantOut), wantRej)
	}
}

const benchQuery = "which nodes does the lexical trigram posting list actually admit"

func benchCandidates(b *testing.B, nodes, words int, postings bool) {
	tg := tokenGrams(kwTokens(benchQuery))
	if postings {
		idx := buildRefIndex(b, nodes, words, 1)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			idx.candidates(tg)
		}
		return
	}
	idx := buildTestIndex(b, nodes, words, 1)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		idx.candidates(tg)
	}
}

func BenchmarkCandidates_Postings_6k(b *testing.B) { benchCandidates(b, 6000, 50, true) }
func BenchmarkCandidates_Vocab_6k(b *testing.B)    { benchCandidates(b, 6000, 50, false) }
func BenchmarkCandidates_Postings_1k(b *testing.B) { benchCandidates(b, 1000, 50, true) }
func BenchmarkCandidates_Vocab_1k(b *testing.B)    { benchCandidates(b, 1000, 50, false) }

// benchIndex measures build cost, not query cost: word-level postings compute
// trigrams once per distinct word instead of once per word occurrence, so this
// is where that shows up.
func benchIndex(b *testing.B, nodes, words int, postings bool) {
	docs := testCorpus(nodes, words, 1)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if postings {
			idx := newRefIndex()
			for _, d := range docs {
				idx.index(d.qn, d.title, d.body)
			}
			continue
		}
		idx := newTrigramIndex()
		for _, d := range docs {
			idx.index(d.qn, d.title, d.body)
		}
	}
}

func BenchmarkIndex_Postings_6k(b *testing.B) { benchIndex(b, 6000, 50, true) }
func BenchmarkIndex_Vocab_6k(b *testing.B)    { benchIndex(b, 6000, 50, false) }
