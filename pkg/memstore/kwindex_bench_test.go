package memstore

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// buildBenchIndex builds an index shaped like a live memory corpus: a few
// hundred cells whose title+content run to a couple of hundred characters, i.e.
// a few dozen indexed words each.
func buildBenchIndex(items, wordsPerItem int) *trigramIndex {
	idx := NewTrigramIndex()
	vocab := []string{
		"application", "degraded", "workloads", "components", "rollout", "deployment",
		"vault", "clienterror", "secret", "namespace", "preview", "auth", "service",
		"oomkilled", "crashloopbackoff", "probe", "readiness", "unhealthy", "container",
		"restart", "issue", "resolved", "recurring", "transient", "cluster", "pod",
	}
	for i := 0; i < items; i++ {
		var sb strings.Builder
		for w := 0; w < wordsPerItem; w++ {
			sb.WriteString(vocab[(i+w)%len(vocab)])
			sb.WriteByte(' ')
		}
		idx.Index(fmt.Sprintf("cell-%d", i), "Application degraded in preview", sb.String())
	}
	return idx
}

// BenchmarkTrigramItemScore_Search reproduces the per-search cost of the
// lexical half of a hybrid search: itemScore is called once per candidate item,
// and each call walks every indexed word of that item for every query token.
// In a measured deployment this path averaged 620 ms per search against about
// 500 candidates, ten times the cost of the whole semantic half; this benchmark
// is where that time lives.
func BenchmarkTrigramItemScore_Search(b *testing.B) {
	for _, sz := range []struct {
		items, words int
	}{{100, 40}, {500, 40}, {500, 120}} {
		idx := buildBenchIndex(sz.items, sz.words)
		toks := []string{"vault", "clienterror", "secret", "preview", "auth", "degraded", "rollout", "probe"}
		ids := make([]string, sz.items)
		for j := range ids {
			ids[j] = fmt.Sprintf("cell-%d", j)
		}
		// Mirrors rankAndLimit: token trigrams hoisted once per query, then
		// one itemScoreGrams per candidate.
		b.Run(fmt.Sprintf("items=%d/words=%d", sz.items, sz.words), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				tg := TokenGrams(toks)
				for _, id := range ids {
					_ = idx.ItemScoreGrams(id, tg)
				}
			}
		})
		// The pre-hoist call shape, kept as the regression guard: if
		// trigrams(tok) ever creeps back inside the per-item loop, this is
		// what it costs.
		b.Run(fmt.Sprintf("unhoisted/items=%d/words=%d", sz.items, sz.words), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				for _, id := range ids {
					_ = idx.ItemScore(id, toks)
				}
			}
		})
	}
}

// BenchmarkTrigramItemScore_Search_Scale isolates the trigram-scoring
// channel in production shape at 220 items (the golden-corpus size)
// and 2000 items (BenchmarkLexicalScoring_
// Scale's size). The "old" subtest reproduces the former per-candidate call shape
// (itemScoreGrams once per candidate, in a loop, the O(corpus) walker); "new" is queryOverlapScores called
// ONCE for the whole candidate set, the shape rankAndLimit now uses.
func BenchmarkTrigramItemScore_Search_Scale(b *testing.B) {
	for _, sz := range []struct{ items, words int }{{220, 60}, {2000, 60}} {
		idx := buildBenchIndex(sz.items, sz.words)
		toks := []string{"vault", "clienterror", "secret", "preview", "auth", "degraded", "rollout", "probe"}
		ids := make([]string, sz.items)
		for j := range ids {
			ids[j] = fmt.Sprintf("cell-%d", j)
		}

		b.Run(fmt.Sprintf("old/items=%d", sz.items), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				tg := TokenGrams(toks)
				for _, id := range ids {
					_ = idx.ItemScoreGrams(id, tg)
				}
			}
		})
		b.Run(fmt.Sprintf("new/items=%d", sz.items), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				tg := TokenGrams(toks)
				_ = idx.QueryOverlapScores(tg)
			}
		})
	}
}

// buildBenchIndexSparse builds an index closer to production's actual
// vocabulary shape than buildBenchIndex: a LARGE (vocabSize-word), varied
// vocabulary from which each item draws only a small subset, so a given
// query token overlaps with a SMALL FRACTION of items — the regime an
// inverted (posting-list) index is built for. buildBenchIndex's 26-word
// vocabulary, cycled so every item contains nearly every word, is the
// opposite (adversarial-dense) case: see BenchmarkTrigramItemScore_Search_
// Scale's doc comment for why that shape doesn't show gramPosting's win. A
// real memory corpus (thousands of cells about distinct services,
// issue reports, and free-text notes) looks like this benchmark, not that one.
// sparseVocab returns the vocabSize-word vocabulary buildBenchIndexSparse and
// sparseItemContent draw from: a fixed realistic base topped up with
// synthetic svcN filler words up to vocabSize.
func sparseVocab(vocabSize int) []string {
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
	return vocab
}

// sparseItemContent generates item i's content string: the same
// index-derived word-picking formula buildBenchIndexSparse uses, factored
// out so a benchmark that re-indexes one item under its own content (rather
// than building the whole corpus) draws from the identical generator.
func sparseItemContent(vocab []string, i, wordsPerItem int) string {
	var sb strings.Builder
	for w := 0; w < wordsPerItem; w++ {
		sb.WriteString(vocab[(i*37+w*101)%len(vocab)])
		sb.WriteByte(' ')
	}
	return sb.String()
}

func buildBenchIndexSparse(items, wordsPerItem, vocabSize int) *trigramIndex {
	idx := NewTrigramIndex()
	vocab := sparseVocab(vocabSize)
	for i := 0; i < items; i++ {
		idx.Index(fmt.Sprintf("cell-%d", i), fmt.Sprintf("Item title %d", i), sparseItemContent(vocab, i, wordsPerItem))
	}
	return idx
}

// BenchmarkTrigramItemScore_Search_SparseVocab is
// BenchmarkTrigramItemScore_Search_Scale over buildBenchIndexSparse instead
// of buildBenchIndex — the realistic-vocabulary counterpart demonstrating
// gramPosting's actual win: with the query's 8 tokens each present in only a
// small fraction of a 300-word vocabulary spread across the corpus, "old"
// still walks every candidate's full word list per token regardless of
// whether that candidate has anything to do with the query, while "new"
// only touches the (item, word) pairs gramPosting says share a gram.
func BenchmarkTrigramItemScore_Search_SparseVocab(b *testing.B) {
	for _, sz := range []struct{ items, words, vocab int }{{220, 20, 300}, {2000, 20, 300}} {
		idx := buildBenchIndexSparse(sz.items, sz.words, sz.vocab)
		toks := []string{"vault", "clienterror", "secret", "preview", "auth", "degraded", "rollout", "probe"}
		ids := make([]string, sz.items)
		for j := range ids {
			ids[j] = fmt.Sprintf("cell-%d", j)
		}

		b.Run(fmt.Sprintf("old/items=%d", sz.items), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				tg := TokenGrams(toks)
				for _, id := range ids {
					_ = idx.ItemScoreGrams(id, tg)
				}
			}
		})
		b.Run(fmt.Sprintf("new/items=%d", sz.items), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				tg := TokenGrams(toks)
				_ = idx.QueryOverlapScores(tg)
			}
		})
	}
}

// BenchmarkTrigramIndex_Reindex isolates the cost of re-indexing a single
// ALREADY-INTERNED item inside a large corpus — the shape every memory-cell
// upsert takes, under the store's write
// lock. The corpus (1,300 items x 200 words, vocab 2000 — the same shape
// TestTrigramIndex_ResidentMemory uses) is built once; only the repeated
// index() call on one middle item is timed, so the cost measured is
// clearItemPostingsLocked's deletes plus index()'s inserts against posting
// lists that already hold entries from items on both sides of this one.
func BenchmarkTrigramIndex_Reindex(b *testing.B) {
	const items, wordsPerItem, vocabSize = 1300, 200, 2000
	const target = 650

	idx := buildBenchIndexSparse(items, wordsPerItem, vocabSize)
	vocab := sparseVocab(vocabSize)
	id := fmt.Sprintf("cell-%d", target)
	title := fmt.Sprintf("Item title %d", target)
	content := sparseItemContent(vocab, target, wordsPerItem)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		idx.Index(id, title, content)
	}
}

// BenchmarkTrigramIndex_BulkBuild is the bulk-build counterpart to
// BenchmarkTrigramIndex_Reindex, same corpus shape: every item is freshly
// interned, so its itemIdx is monotonically increasing and lands at the tail
// of every gram's posting list — the case a single slices.Insert per gram is
// already cheap for, independent of how re-indexing an existing item is
// implemented. Read the two side by side: re-index should not regress bulk
// build's cost to stay fast.
func BenchmarkTrigramIndex_BulkBuild(b *testing.B) {
	const items, wordsPerItem, vocabSize = 1300, 200, 2000

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = buildBenchIndexSparse(items, wordsPerItem, vocabSize)
	}
}

// TestTrigramIndex_ResidentMemory reports trigramIndex's own heap footprint
// for a corpus shaped like a live memory-cell index — ~1,300 items of ~200
// indexed words each, the scale a production Restore pass builds. Not
// a hard-fail assertion (HeapAlloc has run-to-run noise from GC timing and
// other goroutines); run with -v to see the reported delta.
func TestTrigramIndex_ResidentMemory(t *testing.T) {
	const items, wordsPerItem, vocabSize = 1300, 200, 2000

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	idx := buildBenchIndexSparse(items, wordsPerItem, vocabSize)

	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(idx)

	delta := after.HeapAlloc - before.HeapAlloc
	t.Logf("trigramIndex resident: %d items x %d words (vocab=%d) -> HeapAlloc delta %d bytes (%.2f MB)",
		items, wordsPerItem, vocabSize, delta, float64(delta)/(1024*1024))
}
