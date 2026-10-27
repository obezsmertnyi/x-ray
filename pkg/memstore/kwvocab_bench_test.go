package memstore

import (
	"fmt"
	"strings"
	"testing"

	memoryv1 "github.com/triageagent-dev/x-ray/v2/api/v1"
)

// buildVocabBenchIndex builds a vocabIndex shaped like a live memory corpus
// at realistic scale: itemCount items, each with a multi-word title and a
// longer content field drawn from a vocabSize-word vocabulary — the same
// sparse-vocabulary shape buildBenchIndexSparse (kwindex_bench_test.go) uses
// to exercise an inverted index's actual win, since a query token then
// overlaps only a small fraction of items instead of nearly all of them.
// Items are fed through vocabWordsForItem/vi.Index, the same write path
// Upsert uses, rather than poking the postings/bucket maps
// directly, so the benchmark exercises the real index-time bucketing too.
func buildVocabBenchIndex(items, titleWords, contentWords, vocabSize int) *vocabIndex {
	base := []string{
		"application", "degraded", "workloads", "components", "rollout", "deployment",
		"vault", "clienterror", "secret", "namespace", "preview", "auth", "service",
		"oomkilled", "crashloopbackoff", "probe", "readiness", "unhealthy", "container",
		"restart", "issue", "resolved", "recurring", "transient", "cluster", "pod",
	}
	vocab := make([]string, 0, vocabSize)
	vocab = append(vocab, base...)
	for len(vocab) < vocabSize {
		vocab = append(vocab, fmt.Sprintf("svc%d", len(vocab)))
	}

	vi := newVocabIndex()
	for i := 0; i < items; i++ {
		var tb, cb strings.Builder
		for w := 0; w < titleWords; w++ {
			if w > 0 {
				tb.WriteByte(' ')
			}
			tb.WriteString(vocab[(i*7+w*3)%len(vocab)])
		}
		for w := 0; w < contentWords; w++ {
			if w > 0 {
				cb.WriteByte(' ')
			}
			cb.WriteString(vocab[(i*11+w*5)%len(vocab)])
		}
		it := Item{MemoryCellSpec: memoryv1.MemoryCellSpec{
			ID:      fmt.Sprintf("bench-%d", i),
			Title:   tb.String(),
			Content: cb.String(),
		}}
		words, joins := vocabWordsForItem(it)
		vi.Index(it.ID, words, joins)
	}
	return vi
}

// BenchmarkVocabIndex_CandidateIDsForTokens measures candidateIDsForToken/
// candidateIDsForTokens at realistic corpus scale (~700 items, multi-word
// titles/contents) — the pair a live CPU profile attributed 45% of the
// lexical search phase to (split RuneCountInString 42% / editDistanceWithin
// 22% / strings.Contains 21% within the function), before the wordsByLen/
// joinsByLen bucketing in kwvocab.go. The token mix exercises the three cost
// regimes fuzzyTolerance produces: "ok" is tol==0 (word Contains scan only,
// no fuzzy scan of any kind — the (*) short-circuit in candidateIDsForToken's
// doc comment); "vault" is tol==1 with a literal Contains hit (fuzzy scan
// still runs per the outer short-circuit, since tol>0 is a property of the
// token alone); "aplication" is tol==1 with no literal substring anywhere in
// the corpus, forcing the full length-banded edit-distance fuzzy scan over
// both wordsByLen and joinsByLen to resolve.
func BenchmarkVocabIndex_CandidateIDsForTokens(b *testing.B) {
	vi := buildVocabBenchIndex(700, 6, 24, 300)
	toks := []string{"ok", "vault", "aplication"}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = vi.CandidateIDsForTokens(toks)
	}
}
