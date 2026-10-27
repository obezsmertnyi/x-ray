package memstore

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	memoryv1 "github.com/triageagent-dev/x-ray/v2/api/v1"
)

// ── Golden equivalence: index-time-tokenization + bounded-edit-distance
// optimization must not change a single search result ──────────────────────
//
// This file freezes the lexical scoring path exactly as it existed before
// the optimization (the lexicalCache and editDistanceWithin doc comments) under `reference*` names — plain strings in, no wordTokens,
// no lexicalCache, no ASCII fast path, no DP early exit, just the original
// ToLower/NormalizeQuery/lexFields-per-call plus the original
// unbounded editDistance. TestLexicalSearch_GoldenEquivalence runs the real
// (optimized) search and this frozen reference side by side over an
// identical, realistic corpus and a wide query set, and asserts they return
// the exact same ranked ID sequence for every query — the proof that the
// optimization changed cost, not results.
//
// referenceEditDistance is NOT redefined here: editDistance itself was never
// touched by the optimization (only a new editDistanceWithin sibling was
// added, used solely by the optimized fuzzyWordMatchFields), so the
// reference path below calls the real, unmodified editDistance directly.

func referenceMatchesKWTokenFields(h string, hf []string, t string) bool {
	if strings.Contains(h, t) {
		return true
	}
	return referenceFuzzyWordMatchFields(hf, t)
}

func referenceTokenMatchQualityFields(nt string, ntf []string, nc string, ncf []string, t string) int {
	if strings.Contains(nt, t) {
		return wTokExactTle
	}
	if referenceFuzzyWordMatchFields(ntf, t) {
		return wTokFuzzyTle
	}
	if strings.Contains(nc, t) {
		return wTokExactCnt
	}
	if referenceFuzzyWordMatchFields(ncf, t) {
		return wTokFuzzyCnt
	}
	return 0
}

func referenceFuzzyWordMatchFields(words []string, t string) bool {
	tn := runeCountReference(t)
	tol := fuzzyTolerance(tn)
	if tol == 0 {
		return false
	}
	for i, word := range words {
		wn := runeCountReference(word)
		if absDiff(wn, tn) <= tol && editDistance(t, word) <= tol {
			return true
		}
		if i+1 < len(words) {
			jn := wn + runeCountReference(words[i+1])
			jtol := fuzzyTolerance(jn)
			if absDiff(jn, tn) <= jtol && editDistance(t, word+words[i+1]) <= jtol {
				return true
			}
		}
	}
	return false
}

// runeCountReference mirrors the original utf8.RuneCountInString call sites
// verbatim (a plain named wrapper so this file needs no separate utf8
// import-and-shadow dance).
func runeCountReference(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

func referenceSoftMatchCoverage(title, content string, toks []string) (matched int, ok bool) {
	if len(toks) == 0 {
		return 0, true
	}
	nt := strings.ToLower(title)
	nc := strings.ToLower(content)
	ntf, ncf := lexFields(nt), lexFields(nc)
	for _, t := range toks {
		if referenceMatchesKWTokenFields(nt, ntf, t) || referenceMatchesKWTokenFields(nc, ncf, t) {
			matched++
		}
	}
	need := len(toks)
	if len(toks) >= 3 {
		need = len(toks) - 1
	}
	return matched, matched >= need
}

// referenceLexScore mirrors lexScoreFields' weighting over plain, uncached
// splits. The weights computation itself is not part of the caching
// optimization this file freezes a reference for: both the real and the
// reference path call the same tokenWeights and tokenWeight helpers over the
// same vocabIndex, so this only reproduces the per-token match, mean and
// miss-penalty arithmetic.
func referenceLexScore(title, content, q string, toks []string, weights map[string]float64, trigramFrac float64) int {
	nt := NormalizeQuery(title)
	nc := NormalizeQuery(content)

	score := 0
	if q != "" && nt == q {
		score += wExactTitle
	} else if q != "" && strings.Contains(nt, q) {
		score += wTitleContain
	}
	ntf, ncf := lexFields(nt), lexFields(nc)

	var matchedWeight, totalWeight float64
	for _, t := range toks {
		w := tokenWeight(weights, t)
		totalWeight += w
		tq := referenceTokenMatchQualityFields(nt, ntf, nc, ncf, t)
		if tq > 0 {
			matchedWeight += w
		}
		score += int(float64(tq) * w)
	}
	if len(toks) > 0 && totalWeight > 0 {
		mean := totalWeight / float64(len(toks))
		score = int(float64(score) / mean)
	}

	score += int(trigramFrac * wTrigram)

	if len(toks) > 0 && totalWeight > 0 && matchedWeight < totalWeight {
		score -= int(((totalWeight - matchedWeight) / totalWeight) * float64(len(toks)) * wMissPenalty)
	}
	return score
}

// referenceWeightedMatchFraction mirrors weightedMatchFraction over plain,
// uncached fields: the reference-path counterpart of rankAndLimit's weighted
// coverage fallback, needed to keep TestLexicalSearch_GoldenEquivalence
// meaningful now that the production path can admit a candidate the strict
// count gate alone would reject.
func referenceWeightedMatchFraction(nt string, ntf []string, nc string, ncf []string, toks []string, weights map[string]float64) float64 {
	if len(toks) == 0 {
		return 1
	}
	var matchedWeight, totalWeight float64
	for _, t := range toks {
		w := tokenWeight(weights, t)
		totalWeight += w
		if referenceMatchesKWTokenFields(nt, ntf, t) || referenceMatchesKWTokenFields(nc, ncf, t) {
			matchedWeight += w
		}
	}
	if totalWeight <= 0 {
		return 0
	}
	return matchedWeight / totalWeight
}

// referenceRankAndLimit is rankAndLimit as it was before the optimization,
// calling the reference* functions above instead of the cache-backed
// production ones. The weights-aware scoring, the coverage fallback and the
// coverage-cutoff exemption are real algorithm changes both paths share, not
// part of the caching optimization under test.
func referenceRankAndLimit(q string, toks []string, cands []Item, idx *trigramIndex, trigramCands map[string]struct{}, weights map[string]float64, limit int) ([]Item, bool) {
	type scored struct {
		it          Item
		score       int
		matched     int
		viaFallback bool // see rankAndLimit's identically-named field
	}
	var hits []scored
	trigramContributed := false
	var tokGrams [][]string
	if idx != nil && len(toks) > 0 {
		tokGrams = TokenGrams(toks)
	}
	for _, it := range cands {
		if len(toks) > 0 {
			haystack := idHaystack(it)
			matched, ok := referenceSoftMatchCoverage(it.Title, haystack, toks)
			viaFallback := false
			if !ok {
				nt, nc := strings.ToLower(it.Title), strings.ToLower(haystack)
				ntf, ncf := lexFields(nt), lexFields(nc)
				if referenceWeightedMatchFraction(nt, ntf, nc, ncf, toks, weights) < wCoverageMassThreshold {
					continue
				}
				viaFallback = true
			}
			var frac float64
			if idx != nil {
				frac = idx.ItemScoreGrams(it.ID, tokGrams)
			}
			sc := referenceLexScore(it.Title, haystack, q, toks, weights, frac)
			if _, ok := trigramCands[it.ID]; ok {
				trigramContributed = true
			}
			hits = append(hits, scored{it, sc, matched, viaFallback})
		} else {
			hits = append(hits, scored{it, 0, 0, false})
		}
	}

	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		if !hits[i].it.UpdatedAt.Equal(&hits[j].it.UpdatedAt) {
			return hits[i].it.UpdatedAt.After(hits[j].it.UpdatedAt.Time)
		}
		return hits[i].it.ID < hits[j].it.ID
	})

	if len(hits) > 1 {
		topCov := 0
		for _, h := range hits {
			if !h.viaFallback && h.matched > topCov {
				topCov = h.matched
			}
		}
		kept := hits[:0]
		for _, h := range hits {
			spared := false
			if !h.viaFallback && h.matched < topCov {
				haystack := idHaystack(h.it)
				nt, nc := strings.ToLower(h.it.Title), strings.ToLower(haystack)
				spared = referenceWeightedMatchFraction(nt, lexFields(nt), nc, lexFields(nc), toks, weights) >= wCoverageMassThreshold
			}
			if h.viaFallback || h.matched >= topCov || spared {
				kept = append(kept, h)
			}
		}
		hits = kept
	}

	if limit <= 0 {
		limit = 10
	}
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]Item, len(hits))
	for i, h := range hits {
		out[i] = h.it
	}
	return out, trigramContributed
}

// ── Corpus ───────────────────────────────────────────────────────────────

// goldenCorpusVocab is a mix of service and operations vocabulary (service
// names, failure modes, report language), combined pseudo-randomly per item so
// titles and content vary in length and word overlap the way a real memory
// corpus does, rather than every item sharing one boilerplate sentence.
var goldenCorpusVocab = []string{
	"application", "degraded", "workloads", "components", "rollout", "deployment",
	"vault", "clienterror", "secret", "namespace", "preview", "auth", "service",
	"oomkilled", "crashloopbackoff", "probe", "readiness", "unhealthy", "container",
	"restart", "issue", "resolved", "recurring", "transient", "cluster", "pod",
	"checkout", "payments", "billing", "gateway", "latency", "timeout", "retries",
	"database", "connection", "pool", "exhausted", "queue", "backlog", "webhook",
	"reflection", "knowledge", "memory", "cell", "graph", "blast", "radius",
	"reconciler", "hysteresis", "chronic", "sweep", "reflex", "rule", "policy",
}

// goldenCorpusNonASCII supplies titles/content with multibyte runes — accents,
// CJK, emoji — so the golden test exercises editDistanceWithin's rune
// fallback path (asciiOnly(a) && asciiOnly(b) is false), not just its ASCII
// fast path.
var goldenCorpusNonASCII = []struct{ title, content string }{
	{"Déjà vu issue café", "connexion à la base de données échouée, réessai en cours"},
	{"WidgetREC 服务降级", "数据库连接池耗尽，请求超时增多，正在重试"},
	{"Zürich région dégradée", "société générale — passerelle de paiement indisponible"},
	{"🔥 checkout on fire", "payments gateway 🔥 timing out, retries piling up"},
	{"naïve Bayes anomaly señor", "método de detección de anomalías en el servicio de pagos"},
	{"東京クラスタ再起動", "ポッドが OOMKilled になり再起動を繰り返しています"},
	{"Ångström probe café", "readiness probe için zaman aşımı — µs seviyesinde gecikme"},
	{"Größe des Vault-Geheimnisses", "geheimnis konnte nicht geladen werden, Namensraum: préview"},
	{"emoji storm 🚨🚨🚨", "multiple 🔥 issues across 🌍 clusters, blast radius rising"},
	{"चेकआउट सेवा में देरी", "भुगतान गेटवे धीमा है और पुनः प्रयास बढ़ रहे हैं"},
}

// buildGoldenCorpus seeds state with a realistic mixed corpus of at least
// minItems active memory items, spanning several kinds/scopes, ASCII and
// non-ASCII text, so a diverse and representative candidate set backs the
// golden equivalence queries. Every item goes through the real
// Upsert write path, so it carries a real, write-path-built
// lexicalCache exactly like real data would.
func buildGoldenCorpus(t *testing.T, state *Store, minItems int) []Item {
	t.Helper()
	var seeded []Item
	kinds := []string{"stable_fact", "operational_pattern", "watch_item", "note", "hypothesis", "recent_change"}

	for i := 0; i < minItems; i++ {
		var title, content string
		if i%23 == 0 {
			// Sprinkle non-ASCII items throughout the corpus (roughly 1 in 23,
			// ~9-10 across 200+ items) rather than clustering them at the end.
			na := goldenCorpusNonASCII[(i/23)%len(goldenCorpusNonASCII)]
			title = fmt.Sprintf("%s %d", na.title, i)
			content = na.content
		} else {
			var tb, cb strings.Builder
			titleWords := 2 + i%4
			for w := 0; w < titleWords; w++ {
				if w > 0 {
					tb.WriteByte(' ')
				}
				tb.WriteString(goldenCorpusVocab[(i*7+w*3)%len(goldenCorpusVocab)])
			}
			contentWords := 6 + i%12
			for w := 0; w < contentWords; w++ {
				if w > 0 {
					cb.WriteByte(' ')
				}
				cb.WriteString(goldenCorpusVocab[(i*11+w*5)%len(goldenCorpusVocab)])
			}
			title = fmt.Sprintf("%s issue %d", strings.Title(tb.String()), i) //nolint:staticcheck // test data, deprecated linter warning acceptable
			content = cb.String()
		}
		kind := kinds[i%len(kinds)]
		spec := memoryv1.MemoryCellSpec{
			ID:         fmt.Sprintf("golden-%04d", i),
			Kind:       kind,
			Scope:      "global",
			Title:      title,
			Content:    content,
			Confidence: "high",
			Status:     "active",
		}
		if kind == "hypothesis" || kind == "unknown" {
			// ValidateMemoryItem requires stale_after for these two kinds.
			far := metav1.NewTime(time.Now().Add(30 * 24 * time.Hour))
			spec.StaleAfter = &far
		}
		if kind == "stable_fact" {
			// ValidateMemoryItem requires high confidence for stable_fact —
			// already satisfied by Confidence:"high" above, kept explicit.
			spec.Confidence = "high"
		}
		item, errMsg := state.Upsert(Item{MemoryCellSpec: spec})
		require.Empty(t, errMsg)
		seeded = append(seeded, item)
	}
	require.GreaterOrEqual(t, len(seeded), minItems)
	return seeded
}

// goldenQueries is >=50 varied queries covering: exact title matches, fuzzy
// mishears (1-edit off a real word), CamelCase/underscore/dash joins,
// multi-token prose (coverage-gate territory), short (<=2 char) tokens (tol=0
// exact-only path), id-substring lookups, non-ASCII queries against the
// non-ASCII corpus items, and queries with no match at all.
var goldenQueries = []string{
	// Exact / substring
	"application degraded", "checkout payments", "vault secret", "database connection pool",
	"crashloopbackoff", "oomkilled pod", "reflection knowledge memory", "blast radius",
	"chronic sweep", "reflex rule policy", "readiness probe", "billing gateway",
	// Fuzzy mishears (1 edit off a real vocab word)
	"aplication degraded", "chekout payments", "vualt secret", "databse connection",
	"crashloopbakcoff", "oomkiled pod", "reflcetion knowledge", "blst radius",
	"chronc sweep", "refelx rule", "readyness probe", "biling gateway",
	// CamelCase / underscore / dash joins (normalizeQuery split territory)
	"CrashLoopBackOff", "OOMKilled", "billing-service", "billing_service", "WidgetREC",
	// Multi-token prose (coverage-gate: 3+ tokens allow one miss)
	"why does the checkout service keep timing out", "what caused the database connection pool to be exhausted",
	"how does the reflection pass build memory cells", "explain the chronic signal sweep behavior",
	"payments gateway latency issue root cause", "webhook queue backlog resolved recurring",
	// Short (<=2 char) tokens — tol=0, exact-only fuzzy path
	"pod", "db", "ns", "auth", "ok",
	// id-substring lookups (idHaystack widening)
	"golden-0001", "golden-0042", "golden-0100", "0099",
	// Non-ASCII queries (rune-fallback path)
	"café issue", "服务降级", "passerelle de paiement", "🔥 checkout", "señor anomaly",
	"再起動", "Vault-Geheimnisses", "🚨🚨🚨", "चेकआउट सेवा", "Ångström probe",
	// No-match / noise queries
	"completely unrelated text about gardening", "sourdough bread recipe", "quarterly tax filing",
	"unicorn migration festival", "zzz totally different zzz",
	// Adjacent-pair CamelCase-join mishears
	"lunchflow", "widgetreck",
}

func TestGoldenQueries_MeetsMinimumCount(t *testing.T) {
	require.GreaterOrEqual(t, len(goldenQueries), 50, "task requires >=50 varied queries")
}

// TestLexicalSearch_GoldenEquivalence is the required equivalence proof: the
// optimized production search path (state.keywordSearch, which walks
// rankAndLimit → softMatchCoverageFields/lexScoreFields → lowerFieldsFor/
// normFieldsFor → the write-path-built lexicalCache, and
// fuzzyWordMatchFields → editDistanceWithin) must return byte-identical
// ranked ID sequences to referenceRankAndLimit (the frozen pre-optimization
// implementation) for every query, over an identical, shared candidate set —
// including a length-0/no-token edge case, the exact same trigram index, and
// the exact same trigramCands set the real code path computes.
func TestLexicalSearch_GoldenEquivalence(t *testing.T) {
	state := newTestStore()
	buildGoldenCorpus(t, state, 220)

	for _, query := range goldenQueries {
		t.Run(query, func(t *testing.T) {
			const limit = 10

			// Real (optimized) path.
			realResults := state.KeywordSearch(query, "", "", limit)

			// Reference (frozen pre-optimization) path, over the identical
			// candidate set and trigram index the real path used internally.
			q := NormalizeQuery(query)
			toks := KwTokens(q)

			state.mu.RLock()
			cands := make([]Item, 0, len(state.cells))
			for _, it := range state.cells {
				if it.IsActive() {
					cands = append(cands, it.Clone())
				}
			}
			idx := state.kw
			vocab := state.vocab
			state.mu.RUnlock()

			var trigramCands map[string]struct{}
			if q != "" && idx != nil {
				if ids := idx.Search(q); len(ids) > 0 {
					trigramCands = make(map[string]struct{}, len(ids))
					for _, id := range ids {
						trigramCands[id] = struct{}{}
					}
				}
			}
			// The SAME weights the real path computes (see keywordSearchWithTrigramHit)
			// — over the identical vocab and the identical pre-restriction corpus
			// size, so this is not a second, potentially-divergent DF/IDF
			// implementation, just the one production entry point called directly.
			weights := tokenWeights(toks, vocab, len(cands))
			refResults, _ := referenceRankAndLimit(q, toks, cands, idx, trigramCands, weights, limit)

			realIDs := idsOf(realResults)
			refIDs := idsOf(refResults)
			assert.Equal(t, refIDs, realIDs, "optimized and reference lexical scan must return the same ranked ID sequence for query %q", query)
		})
	}
}

func idsOf(items []Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.ID
	}
	return out
}

// TestLexicalSearch_GoldenEquivalence_EmptyQuery covers the len(toks)==0
// branch (every candidate passes with score 0) separately, since
// goldenQueries above are all non-empty.
func TestLexicalSearch_GoldenEquivalence_EmptyQuery(t *testing.T) {
	state := newTestStore()
	buildGoldenCorpus(t, state, 220)

	realResults := state.KeywordSearch("   ", "", "", 10)
	refResults, _ := func() ([]Item, bool) {
		q := NormalizeQuery("   ")
		toks := KwTokens(q)
		state.mu.RLock()
		cands := make([]Item, 0, len(state.cells))
		for _, it := range state.cells {
			if it.IsActive() {
				cands = append(cands, it.Clone())
			}
		}
		idx := state.kw
		state.mu.RUnlock()
		return referenceRankAndLimit(q, toks, cands, idx, nil, nil, 10)
	}()

	assert.Equal(t, idsOf(refResults), idsOf(realResults))
}

// TestEditDistanceWithin_MatchesEditDistance exhaustively checks the
// optimized bounded/early-exit/ASCII-fast-path edit distance against the
// unmodified editDistance reference over a wide mix of ASCII and non-ASCII
// word pairs and every max value the fuzzy-match path can pass (0, 1) plus a
// couple of larger ones for full generality — proving
// editDistanceWithin(a, b, max) == (editDistance(a, b) <= max) always.
func TestEditDistanceWithin_MatchesEditDistance(t *testing.T) {
	words := []string{
		"", "a", "ab", "abc", "widget", "widges", "widgt", "widgett",
		"crashloopbackoff", "crashloopbakcoff", "crashloopback0ff",
		"oomkilled", "oomkiled", "oomkillled", "readiness", "readyness",
		"déjà", "déja", "dejaa", "café", "cafe", "naïve", "naive",
		"服务降级", "服务降极", "服务", "🔥checkout", "🔥checkut", "checkout🔥",
		"Ångström", "Angstrom", "Größe", "Groesse",
	}
	maxes := []int{0, 1, 2, 3}
	for _, a := range words {
		for _, b := range words {
			for _, max := range maxes {
				want := editDistance(a, b) <= max
				got := editDistanceWithin(a, b, max)
				if got != want {
					t.Errorf("editDistanceWithin(%q, %q, %d) = %v, want %v (editDistance=%d)",
						a, b, max, got, want, editDistance(a, b))
				}
			}
		}
	}
}

// ── Benchmarks ───────────────────────────────────────────────────────────

// BenchmarkLexicalScoring measures the real, end-to-end production hot path
// (state.keywordSearch → keywordSearchWithTrigramHit → rankAndLimit) against
// the golden corpus (220 items, realistic mixed vocabulary, ASCII and
// non-ASCII) for a representative slice of the golden query set — the
// server-side "lexical phase" analog. The corpus is built once outside the
// timed loop (b.ResetTimer), so every iteration measures pure search cost
// against a warm, cached corpus — exactly the steady-state production
// shape (a corpus that changes far less often than it is searched).
func BenchmarkLexicalScoring(b *testing.B) {
	state := newTestStore()
	buildGoldenCorpusB(b, state, 220)

	queries := []string{
		"application degraded", "checkout payments", "vault secret", "database connection pool",
		"crashloopbackoff", "oomkilled pod", "aplication degraded", "chekout payments",
		"why does the checkout service keep timing out", "how does the reflection pass build memory cells",
		"CrashLoopBackOff", "billing-service", "café issue", "服务降级", "lunchflow",
		"completely unrelated text about gardening",
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		q := queries[i%len(queries)]
		_ = state.KeywordSearch(q, "", "", 10)
	}
}

// BenchmarkLexicalScoring_Scale is BenchmarkLexicalScoring at 2000 items
// instead of 220 — the vocabIndex candidate restriction (kwvocab.go) turns
// rankAndLimit's candidate scan from O(active items) into O(items whose
// vocabulary overlaps the query), so its benefit is corpus-size-dependent
// and barely visible at 220 items sharing a ~50-word vocabulary (most items
// already share words with most queries). This is the size where the
// structural difference is measurable.
func BenchmarkLexicalScoring_Scale(b *testing.B) {
	state := newTestStore()
	buildGoldenCorpusB(b, state, 2000)

	queries := []string{
		"application degraded", "checkout payments", "vault secret", "database connection pool",
		"crashloopbackoff", "oomkilled pod", "aplication degraded", "chekout payments",
		"why does the checkout service keep timing out", "how does the reflection pass build memory cells",
		"CrashLoopBackOff", "billing-service", "café issue", "服务降级", "lunchflow",
		"completely unrelated text about gardening",
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		q := queries[i%len(queries)]
		_ = state.KeywordSearch(q, "", "", 10)
	}
}

// BenchmarkLexicalScoring_ScaleBoundedVocab is BenchmarkLexicalScoring_Scale
// with item text drawn ONLY from goldenCorpusVocab — no per-item unique
// numeric suffix embedded in the title/content text (the item ID stays
// unique; only the SEARCHABLE text is bounded-vocabulary). This is the more
// realistic shape for the vocabIndex's win: a real memory
// corpus reuses a bounded set of service names, error codes, and phrases
// across thousands of records rather than minting a unique word per record,
// so the corpus-wide vocabulary stays roughly constant-sized as item count
// grows, and the fraction of items sharing zero vocabulary with a given
// query grows correspondingly — exactly the case vocabIndex's candidate
// restriction is for.
func BenchmarkLexicalScoring_ScaleBoundedVocab(b *testing.B) {
	state := newTestStore()
	for i := 0; i < 2000; i++ {
		var tb, cb strings.Builder
		titleWords := 2 + i%4
		for w := 0; w < titleWords; w++ {
			if w > 0 {
				tb.WriteByte(' ')
			}
			tb.WriteString(goldenCorpusVocab[(i*7+w*3)%len(goldenCorpusVocab)])
		}
		contentWords := 6 + i%12
		for w := 0; w < contentWords; w++ {
			if w > 0 {
				cb.WriteByte(' ')
			}
			cb.WriteString(goldenCorpusVocab[(i*11+w*5)%len(goldenCorpusVocab)])
		}
		spec := memoryv1.MemoryCellSpec{
			ID:         fmt.Sprintf("bounded-%04d", i),
			Kind:       "stable_fact",
			Scope:      "global",
			Title:      strings.Title(tb.String()), //nolint:staticcheck
			Content:    cb.String(),
			Confidence: "high",
			Status:     "active",
		}
		if _, errMsg := state.Upsert(Item{MemoryCellSpec: spec}); errMsg != "" {
			b.Fatalf("seed failed: %s", errMsg)
		}
	}

	queries := []string{
		"application degraded", "checkout payments", "vault secret", "database connection pool",
		"crashloopbackoff", "oomkilled pod", "aplication degraded", "chekout payments",
		"reflection knowledge memory", "chronic sweep reflex rule", "readiness probe billing gateway",
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		q := queries[i%len(queries)]
		_ = state.KeywordSearch(q, "", "", 10)
	}
}

// buildGoldenCorpusB is buildGoldenCorpus for a *testing.B caller (require
// needs TestingT, which *testing.B also satisfies, but b.Helper() has a
// different receiver type than t.Helper(), so this is a thin duplicate
// rather than a generic-over-TestingT wrapper).
func buildGoldenCorpusB(b *testing.B, state *Store, minItems int) {
	b.Helper()
	for i := 0; i < minItems; i++ {
		var title, content string
		if i%23 == 0 {
			na := goldenCorpusNonASCII[(i/23)%len(goldenCorpusNonASCII)]
			title = fmt.Sprintf("%s %d", na.title, i)
			content = na.content
		} else {
			var tb, cb strings.Builder
			titleWords := 2 + i%4
			for w := 0; w < titleWords; w++ {
				if w > 0 {
					tb.WriteByte(' ')
				}
				tb.WriteString(goldenCorpusVocab[(i*7+w*3)%len(goldenCorpusVocab)])
			}
			contentWords := 6 + i%12
			for w := 0; w < contentWords; w++ {
				if w > 0 {
					cb.WriteByte(' ')
				}
				cb.WriteString(goldenCorpusVocab[(i*11+w*5)%len(goldenCorpusVocab)])
			}
			title = fmt.Sprintf("%s issue %d", strings.Title(tb.String()), i) //nolint:staticcheck
			content = cb.String()
		}
		kinds := []string{"stable_fact", "operational_pattern", "watch_item", "note", "hypothesis", "recent_change"}
		kind := kinds[i%len(kinds)]
		spec := memoryv1.MemoryCellSpec{
			ID:         fmt.Sprintf("golden-%04d", i),
			Kind:       kind,
			Scope:      "global",
			Title:      title,
			Content:    content,
			Confidence: "high",
			Status:     "active",
		}
		if kind == "hypothesis" || kind == "unknown" {
			far := metav1.NewTime(time.Now().Add(30 * 24 * time.Hour))
			spec.StaleAfter = &far
		}
		if _, errMsg := state.Upsert(Item{MemoryCellSpec: spec}); errMsg != "" {
			b.Fatalf("seed failed: %s", errMsg)
		}
	}
}

// TestFuzzyWordMatchFields_CachedEqualsUncached proves the wordTokens
// wrapper (used by both the fresh-computation wrapper functions and the
// index-time cache) doesn't change fuzzyWordMatchFields' answer versus
// passing the same words through the old []string-shaped call.
func TestFuzzyWordMatchFields_CachedEqualsUncached(t *testing.T) {
	cases := []struct {
		words []string
		tok   string
	}{
		{[]string{"widget", "rag", "project"}, "widges"},
		{[]string{"launch", "flow"}, "lunchflow"},
		{[]string{"crashloopbackoff", "pod"}, "crashloopbakcoff"},
		{[]string{"服务", "降级"}, "服务"},
		{[]string{"re", "rec"}, "r2"},
		{[]string{}, "anything"},
	}
	for _, c := range cases {
		got := fuzzyWordMatchFields(newWordTokens(c.words), c.tok)
		want := referenceFuzzyWordMatchFields(c.words, c.tok)
		assert.Equal(t, want, got, "words=%v tok=%q", c.words, c.tok)
	}
}

// ── Vocabulary index (kwvocab.go) equivalence ───────────────────────────────

// referenceMatchesTokenAnywhere is the brute-force, per-item definition of
// "could this item possibly contribute to the coverage count for token t" —
// exactly matchesKWTokenFields(nt, ntf, t) || matchesKWTokenFields(nc, ncf, t)
// against the item's lower-cased fields, using the reference (non-cached,
// pre-optimization) matching functions so this check is independent of both
// the lexicalCache AND the vocabIndex under test.
func referenceMatchesTokenAnywhere(it Item, t string) bool {
	haystack := idHaystack(it)
	nt := strings.ToLower(it.Title)
	nc := strings.ToLower(haystack)
	ntf, ncf := lexFields(nt), lexFields(nc)
	return referenceMatchesKWTokenFields(nt, ntf, t) || referenceMatchesKWTokenFields(nc, ncf, t)
}

// TestVocabIndex_CandidateIDs_MatchesPerItemScan is the direct cross-check
// backing candidateIDsForToken's equivalence proof: for every token in
// goldenQueries (tokenized) plus a few hand-picked edge cases, the set of
// item IDs the vocabulary resolves must equal EXACTLY (not just a superset
// of) the set obtained by brute-force testing every item individually with
// the reference matcher.
func TestVocabIndex_CandidateIDs_MatchesPerItemScan(t *testing.T) {
	state := newTestStore()
	items := buildGoldenCorpus(t, state, 220)

	vocab := newVocabIndex()
	for _, it := range items {
		words, joins := vocabWordsForItem(it)
		vocab.Index(it.ID, words, joins)
	}

	// Collect every distinct token exercised across the golden query set,
	// plus a handful of hand-picked edge cases (very short tokens, tokens
	// with no possible match, tokens matching only via a strict substring of
	// a longer word).
	tokenSet := map[string]struct{}{
		"crash": {}, "auth": {}, "db": {}, "ok": {}, "zzz": {},
		"widgetrec": {}, "lunchflow": {}, "readi": {},
	}
	for _, q := range goldenQueries {
		for _, tok := range KwTokens(NormalizeQuery(q)) {
			tokenSet[tok] = struct{}{}
		}
	}

	for tok := range tokenSet {
		t.Run(tok, func(t *testing.T) {
			got := vocab.CandidateIDsForToken(tok)

			want := map[string]struct{}{}
			for _, it := range items {
				if referenceMatchesTokenAnywhere(it, tok) {
					want[it.ID] = struct{}{}
				}
			}

			gotIDs := make([]string, 0, len(got))
			for id := range got {
				gotIDs = append(gotIDs, id)
			}
			wantIDs := make([]string, 0, len(want))
			for id := range want {
				wantIDs = append(wantIDs, id)
			}
			sort.Strings(gotIDs)
			sort.Strings(wantIDs)
			assert.Equal(t, wantIDs, gotIDs, "token %q: vocab-resolved candidate set must exactly equal the brute-force per-item scan", tok)
		})
	}
}

// TestVocabIndex_IncrementalMaintenance proves the vocabulary stays correct
// across upsert (content change), expire, and delete — the same write-path
// discipline lexicalCache and kwIdx already follow.
func TestVocabIndex_IncrementalMaintenance(t *testing.T) {
	state := newTestStore()

	item := upsertTestMemoryItem(t, state, "stable_fact", "Vault secret rotation", "the vault secret was rotated successfully")
	require.NotNil(t, state.vocab)

	// Present after insert.
	ids := state.vocab.CandidateIDsForToken("vault")
	_, ok := ids[item.ID]
	assert.True(t, ok, "item must be a vocab candidate for a word it contains")

	// Content change must re-derive the vocabulary — searching the OLD word
	// must no longer find it, the NEW word must.
	updated := item
	updated.Title = "Checkout latency spike"
	updated.Content = "checkout requests are timing out under load"
	_, errMsg := state.Upsert(updated)
	require.Empty(t, errMsg)

	idsOld := state.vocab.CandidateIDsForToken("vault")
	_, stillThere := idsOld[item.ID]
	assert.False(t, stillThere, "stale vocab entry must be gone after the item's content changed")

	idsNew := state.vocab.CandidateIDsForToken("checkout")
	_, foundNew := idsNew[item.ID]
	assert.True(t, foundNew, "vocab must reflect the item's new content")

	// Delete must remove it entirely.
	require.True(t, state.Delete(item.ID))
	idsAfterDelete := state.vocab.CandidateIDsForToken("checkout")
	_, foundAfterDelete := idsAfterDelete[item.ID]
	assert.False(t, foundAfterDelete, "deleted item must not remain a vocab candidate")
}

// TestLexicalSearch_GoldenEquivalence_VocabRestricted names, explicitly, the
// property TestLexicalSearch_GoldenEquivalence already exercises implicitly
// (the real search path it calls includes the vocabIndex-based candidate
// restriction wired into keywordSearchWithTrigramHit): restricting
// rankAndLimit's candidate set via the vocabulary must not change a single
// query's ranked result versus scoring every active candidate unrestricted
// (referenceRankAndLimit, which never consults vocabIdx at all). This test
// makes that comparison directly rather than relying on the reader to know
// state.keywordSearch's internals restrict candidates.
func TestLexicalSearch_GoldenEquivalence_VocabRestricted(t *testing.T) {
	state := newTestStore()
	buildGoldenCorpus(t, state, 220)
	require.NotNil(t, state.vocab)

	for _, query := range goldenQueries {
		t.Run(query, func(t *testing.T) {
			const limit = 10
			q := NormalizeQuery(query)
			toks := KwTokens(q)
			if len(toks) == 0 {
				return
			}

			// Real path: goes through vocabIdx.candidateIDsForTokens
			// restriction inside keywordSearchWithTrigramHit.
			restrictedResults := state.KeywordSearch(query, "", "", limit)

			// Unrestricted: build the full candidate set exactly like
			// keywordSearchWithTrigramHit does, but skip the vocab
			// restriction entirely and hand everything to the SAME
			// (optimized) rankAndLimit.
			state.mu.RLock()
			cands := make([]Item, 0, len(state.cells))
			for _, it := range state.cells {
				if it.IsActive() {
					cands = append(cands, it.Clone())
				}
			}
			idx := state.kw
			vocab := state.vocab
			state.mu.RUnlock()
			var trigramCands map[string]struct{}
			if ids := idx.Search(q); len(ids) > 0 {
				trigramCands = make(map[string]struct{}, len(ids))
				for _, id := range ids {
					trigramCands[id] = struct{}{}
				}
			}
			// Same corpus, same vocab, so the SAME weights the restricted
			// (real) path computes internally — this test isolates the
			// restriction step alone, not weight computation.
			weights := tokenWeights(toks, vocab, len(cands))
			unrestrictedResults, _ := rankAndLimit(q, toks, cands, idx, trigramCands, weights, limit)

			assert.Equal(t, idsOf(unrestrictedResults), idsOf(restrictedResults),
				"vocab-restricted candidate scan must return the same ranked IDs as an unrestricted scan for query %q", query)
		})
	}
}

// ── Trigram posting-list equivalence ───────────────────────────────────────
//
// The vocabulary section above restricts rankAndLimit's candidate set. Here the
// trigram-overlap score of each surviving candidate comes from
// queryOverlapScores, one posting-list merge per query that touches only the
// (item, word) pairs sharing a gram with the query. The former way,
// tokOverlapGramsLocked called once per candidate (via itemScoreGrams), walked
// every word of every candidate against every query token's grams, so its cost
// scaled with the candidate set and the vocabulary rather than with how much
// the query matched.
//
// itemScoreGrams/itemScore/tokOverlapGramsLocked/tokOverlapLocked/
// tokMatchesItem are kept verbatim as the frozen reference this section checks
// the posting-merge path against, as referenceRankAndLimit and
// referenceLexScore above freeze the coverage and scoring path. This
// means TestLexicalSearch_GoldenEquivalence (which already routes real
// results through the NEW rankAndLimit/queryOverlapScores and reference
// results through referenceRankAndLimit/itemScoreGrams, both over the same
// state.kw) already exercises this equivalence end to end for every
// goldenQuery; the tests below make that property explicit and check it
// exhaustively at the primitive level instead of only through final rankings.

// TestTrigramIndex_QueryOverlapScores_MatchesGoldenCorpus is the exhaustive
// cross-check backing queryOverlapScores' equivalence proof: for every
// distinct token exercised across goldenQueries (ASCII and non-ASCII, since
// buildGoldenCorpus sprinkles non-ASCII items throughout and several
// goldenQueries target them directly), the batch posting-merge score for
// EVERY golden-corpus item must equal the frozen per-item itemScore
// reference exactly (float64 equality, not InDelta — see
// tokenBestOverlapLocked's doc comment for why bit-identical is the correct
// bar here).
func TestTrigramIndex_QueryOverlapScores_MatchesGoldenCorpus(t *testing.T) {
	state := newTestStore()
	items := buildGoldenCorpus(t, state, 220)
	idx := state.kw
	require.NotNil(t, idx)

	tokenSet := map[string]struct{}{
		"aaaa": {}, "sym": {}, "café": {}, "服务": {},
	}
	for _, q := range goldenQueries {
		for _, tok := range KwTokens(NormalizeQuery(q)) {
			tokenSet[tok] = struct{}{}
		}
	}

	for tok := range tokenSet {
		t.Run(tok, func(t *testing.T) {
			toks := []string{tok}
			got := idx.QueryOverlapScores(TokenGrams(toks))
			for _, it := range items {
				want := idx.ItemScore(it.ID, toks)
				assert.Equal(t, want, got[it.ID], "id=%s tok=%q", it.ID, tok)
			}
		})
	}
}

// TestTrigramIndex_QueryOverlapScores_MultiToken is
// TestTrigramIndex_QueryOverlapScores_MatchesGoldenCorpus for whole
// (multi-token) queries rather than single tokens — the shape rankAndLimit
// actually calls queryOverlapScores with, exercising the cross-token
// summation/division ordering the single-token check above cannot.
func TestTrigramIndex_QueryOverlapScores_MultiToken(t *testing.T) {
	state := newTestStore()
	items := buildGoldenCorpus(t, state, 220)
	idx := state.kw
	require.NotNil(t, idx)

	for _, query := range goldenQueries {
		t.Run(query, func(t *testing.T) {
			toks := KwTokens(NormalizeQuery(query))
			if len(toks) == 0 {
				return
			}
			got := idx.QueryOverlapScores(TokenGrams(toks))
			for _, it := range items {
				want := idx.ItemScore(it.ID, toks)
				assert.Equal(t, want, got[it.ID], "id=%s query=%q", it.ID, query)
			}
		})
	}
}

// TestTrigramIndex_Search_MatchesGoldenCorpus cross-checks the round-5
// posting-merge search() against a brute-force reference built on the
// unchanged tokMatchesItem predicate, scanning every golden-corpus item
// directly instead of relying on any index structure — see search()'s doc
// comment for the equivalence argument this proves.
func TestTrigramIndex_Search_MatchesGoldenCorpus(t *testing.T) {
	state := newTestStore()
	items := buildGoldenCorpus(t, state, 220)
	idx := state.kw
	require.NotNil(t, idx)

	for _, query := range goldenQueries {
		t.Run(query, func(t *testing.T) {
			got := idx.Search(query)

			toks := KwTokens(NormalizeQuery(query))
			var want []string
			if len(toks) > 0 {
				for _, it := range items {
					ok := true
					idx.Mu.RLock()
					for _, tok := range toks {
						if !idx.TokMatchesItem(it.ID, tok) {
							ok = false
							break
						}
					}
					idx.Mu.RUnlock()
					if ok {
						want = append(want, it.ID)
					}
				}
			}

			assert.ElementsMatch(t, want, got, "query %q", query)
		})
	}
}

func upsertTestMemoryItem(t *testing.T, state *Store, kind, title, content string) Item {
	t.Helper()
	item, errMsg := state.Upsert(Item{MemoryCellSpec: memoryv1.MemoryCellSpec{
		Kind:       kind,
		Scope:      "global",
		Title:      title,
		Content:    content,
		Confidence: "high",
		Status:     "active",
	}})
	require.Empty(t, errMsg)
	return item
}
