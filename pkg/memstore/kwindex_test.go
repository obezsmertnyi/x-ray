package memstore

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── trigrams ──────────────────────────────────────────────────────────────

func TestTrigrams_Normal(t *testing.T) {
	assert.Equal(t, []string{"wid", "idg", "dge", "get"}, trigrams("widget"))
}

func TestTrigrams_Short(t *testing.T) {
	assert.Equal(t, []string{"re"}, trigrams("re"))
	assert.Equal(t, []string{"r"}, trigrams("r"))
	assert.Equal(t, []string{"rec"}, trigrams("rec"))
}

// ── trigramIndex ──────────────────────────────────────────────────────────

func TestTrigramIndex_ItemScore(t *testing.T) {
	idx := NewTrigramIndex()
	idx.Index("id1", "Widget RAG", "Enterprise knowledge graph")

	// Exact word present → full overlap.
	assert.Equal(t, 1.0, idx.ItemScore("id1", []string{"widget"}))
	// "widge" (3 trigrams: wid,idg,dge) — every one of them also appears in
	// "widget" (wid,idg,dge,get) → full overlap even though "widge" is a prefix.
	assert.Equal(t, 1.0, idx.ItemScore("id1", []string{"widge"}))
	// "widges" (4 trigrams: wid,idg,dge,ges) shares 3/4 with "widget" → 0.75.
	assert.InDelta(t, 0.75, idx.ItemScore("id1", []string{"widges"}), 0.01)
	// Unrelated token → 0.
	assert.Equal(t, 0.0, idx.ItemScore("id1", []string{"zzz"}))
}

func TestTrigramIndex_RemoveClears(t *testing.T) {
	idx := NewTrigramIndex()
	idx.Index("id1", "Widget RAG", "content")
	idx.Remove("id1")

	assert.Equal(t, 0.0, idx.ItemScore("id1", []string{"widget"}))
}

func TestTrigramIndex_UpdateReplaces(t *testing.T) {
	idx := NewTrigramIndex()
	idx.Index("id1", "Widget RAG", "content")
	idx.Index("id1", "Other REC", "content") // update same ID

	assert.Equal(t, 0.0, idx.ItemScore("id1", []string{"widget"}), "old posting must be cleared")
	assert.Equal(t, 1.0, idx.ItemScore("id1", []string{"rec"}))
}

// TestTrigramIndex_ReindexReusesInternedSlot pins the leak-avoidance
// invariant: re-indexing an id already present reuses its interned index
// rather than minting a new one, so a long-lived process re-indexing the
// same items repeatedly (every memory-cell upsert) does not grow itemIDs/
// itemNames without bound.
func TestTrigramIndex_ReindexReusesInternedSlot(t *testing.T) {
	idx := NewTrigramIndex()
	idx.Index("id1", "Widget RAG", "content")

	idx.Mu.RLock()
	firstIdx := idx.itemIDs["id1"]
	itemCountAfterFirst := len(idx.itemIDs)
	namesLenAfterFirst := len(idx.itemNames)
	idx.Mu.RUnlock()

	idx.Index("id1", "Other REC", "different content entirely")

	idx.Mu.RLock()
	secondIdx := idx.itemIDs["id1"]
	itemCountAfterSecond := len(idx.itemIDs)
	namesLenAfterSecond := len(idx.itemNames)
	idx.Mu.RUnlock()

	assert.Equal(t, firstIdx, secondIdx, "re-indexing the same id must reuse its interned index")
	assert.Equal(t, itemCountAfterFirst, itemCountAfterSecond, "interned item count must not grow on re-index")
	assert.Equal(t, namesLenAfterFirst, namesLenAfterSecond, "itemNames must not grow on re-index")

	assert.Equal(t, 0.0, idx.ItemScore("id1", []string{"widget"}), "old content must be cleared")
	assert.Equal(t, 1.0, idx.ItemScore("id1", []string{"rec"}), "new content must be indexed")
}

// TestTrigramIndex_ReindexBatchesPostingsCorrectly exercises the batched
// per-gram posting-list update index()/clearItemPostingsLocked use: it
// re-indexes a middle item in a small multi-item corpus (so its contiguous
// range in a shared gram's posting list has neighbors on both sides) and
// checks every invariant the batching must preserve — sortedness, no
// duplicate refs, no stale refs to the item's old-only words, and identical
// scores between the wordGramFlat-based reference path (itemScore) and the
// gramPosting-based production path (queryOverlapScores) it changed.
func TestTrigramIndex_ReindexBatchesPostingsCorrectly(t *testing.T) {
	idx := NewTrigramIndex()
	corpus := []struct{ id, title, content string }{
		{"id0", "Widget RAG platform", "enterprise knowledge graph service vault"},
		{"id1", "Other REC device", "recording platform gateway secret"},
		{"id2", "Widget Gateway", "authentication vault secret rotation"},
		{"id3", "Middle Target", "original stale words go here today"},
		{"id4", "Trailing Item", "cluster issue resolved transient vault"},
		{"id5", "Another Widget", "gateway rotation secret vault entry"},
	}
	for _, it := range corpus {
		idx.Index(it.id, it.title, it.content)
	}

	const target = "id3"
	oldWords := strings.Fields(strings.ToLower("Middle Target original stale words go here today"))
	idx.Index(target, "Middle Target Renamed", "fresh vault rotation content entirely different phrasing")

	idx.Mu.RLock()
	defer idx.Mu.RUnlock()

	for gid, list := range idx.gramPosting {
		require.True(t, slices.IsSorted(list), "gram id %d posting list must stay sorted after re-index", gid)
		deduped := slices.Compact(slices.Clone(list))
		require.Len(t, deduped, len(list), "gram id %d posting list must have no duplicate refs after re-index", gid)
	}

	// No posting still references the target's OLD word content, unless the
	// same gram legitimately survives via the new content.
	targetIdx := idx.itemIDs[target]
	newFlat := idx.wordGramFlat[targetIdx]
	inNewContent := make(map[uint32]bool, len(newFlat))
	for _, gid := range newFlat {
		inNewContent[gid] = true
	}
	for _, w := range oldWords {
		for _, g := range trigrams(w) {
			gid, ok := idx.gramIDs[g]
			if !ok || inNewContent[gid] {
				continue
			}
			lo, hi := idx.itemPostingRangeLocked(gid, targetIdx)
			assert.Equal(t, lo, hi, "gram %q must not reference the re-indexed item's stale word any more", g)
		}
	}

	// The gramPosting-based production path (queryOverlapScores) must still
	// agree, item for item, with the wordGramFlat-based reference (itemScore)
	// — the batched insert/delete must leave gramPosting exactly the set of
	// refs wordGramFlat implies, not merely sorted and dedup-clean.
	toks := []string{"widget", "vault", "rotation", "fresh", "gateway", "recording", "issue", "renamed", "phrasing"}
	tokGrams := TokenGrams(toks)
	got := idx.QueryOverlapScores(tokGrams)
	for _, it := range append(corpus, struct{ id, title, content string }{target, "", ""}) {
		want := idx.ItemScoreGrams(it.id, tokGrams)
		assert.Equal(t, want, got[it.id], "id=%s", it.id)
	}
}

// TestTrigramIndex_RemoveReaddReusesSlot guards the invariant that a
// remove()-then-index() cycle on the same id must reuse its interned slot
// (itemIDs keeps the entry across remove) rather than abandoning it, since
// that cycle is a real path — Expire followed by a later upsert,
// or indexMemoryItemLocked's inactive branch followed by the item becoming
// active again — and abandoning the slot would leak one itemNames entry per
// cycle.
func TestTrigramIndex_RemoveReaddReusesSlot(t *testing.T) {
	idx := NewTrigramIndex()
	idx.Index("id1", "Widget RAG", "content")

	idx.Mu.RLock()
	namesLenBefore := len(idx.itemNames)
	idsLenBefore := len(idx.itemIDs)
	idx.Mu.RUnlock()

	for i := 0; i < 1000; i++ {
		idx.Remove("id1")
		idx.Index("id1", "Widget RAG", "content")
	}

	idx.Mu.RLock()
	namesLenAfter := len(idx.itemNames)
	idsLenAfter := len(idx.itemIDs)
	idx.Mu.RUnlock()

	assert.Equal(t, namesLenBefore, namesLenAfter, "1000 remove+index cycles on one id must not grow itemNames")
	assert.Equal(t, idsLenBefore, idsLenAfter, "1000 remove+index cycles on one id must not grow itemIDs")
	assert.Equal(t, 1.0, idx.ItemScore("id1", []string{"widget"}), "item must score correctly after the final index")

	idx.Remove("id1")
	assert.Equal(t, 0.0, idx.ItemScore("id1", []string{"widget"}), "item must score 0 after a final remove")
}

// TestTrigramIndex_RemoveDoesNotAffectSharedGrams guards gramPosting's
// removal discipline: two items sharing a word must each keep their own
// posting entry independent of the other's lifecycle — id1's removal must
// splice out only id1's own wordRef entries, never the whole posting list,
// which would silently blind id2 too.
func TestTrigramIndex_RemoveDoesNotAffectSharedGrams(t *testing.T) {
	idx := NewTrigramIndex()
	idx.Index("id1", "Widget RAG", "content")
	idx.Index("id2", "Widget Other", "content")

	assert.Equal(t, 1.0, idx.ItemScore("id1", []string{"widget"}))
	assert.Equal(t, 1.0, idx.ItemScore("id2", []string{"widget"}))

	idx.Mu.RLock()
	id1Idx := idx.itemIDs["id1"]
	id2Idx := idx.itemIDs["id2"]
	gid := idx.gramIDs["wid"]
	idx.Mu.RUnlock()

	idx.Remove("id1")

	assert.Equal(t, 0.0, idx.ItemScore("id1", []string{"widget"}), "removed item must score 0")
	assert.Equal(t, 1.0, idx.ItemScore("id2", []string{"widget"}), "surviving item sharing the gram must be unaffected")

	idx.Mu.RLock()
	list := idx.gramPosting[gid]
	idx.Mu.RUnlock()
	_, stillPosted := slices.BinarySearch(list, wordRef(id1Idx, 0))
	_, id2Posted := slices.BinarySearch(list, wordRef(id2Idx, 0))
	assert.False(t, stillPosted, "id1's own posting entry must be gone")
	assert.True(t, id2Posted, "id2's posting entry for the same gram must survive id1's removal")
}

// TestTrigramIndex_RepeatedGramMultiplicity exercises trigrams()' documented
// duplicate-substring behavior (a token like "aaaa" yields the same 3-gram
// twice) end to end through the new posting-merge path: tokenBestOverlapLocked
// must count each occurrence separately, exactly like the frozen
// tokOverlapGramsLocked reference — see that function's equivalence proof.
// Without per-occurrence counting this would silently read as a lower
// fraction (or, filtered through the wrong denominator, an inflated one).
func TestTrigramIndex_RepeatedGramMultiplicity(t *testing.T) {
	idx := NewTrigramIndex()
	// "aaaa" -> trigrams ["aaa","aaa"] (duplicate). Word "aaaaa" contains
	// "aaa" too, so every occurrence of the query token's repeated gram must
	// count as a hit against it.
	idx.Index("id1", "aaaaa", "unrelated content here")

	want := idx.ItemScore("id1", []string{"aaaa"})
	require.Equal(t, 1.0, want, "every occurrence of the repeated gram is present in the word's set")

	tg := trigrams("aaaa")
	require.Len(t, tg, 2, "aaaa must yield two (duplicate) trigrams")
	got := idx.QueryOverlapScores([][]string{tg})
	assert.Equal(t, want, got["id1"], "posting-merge path must match the per-occurrence reference count")
}

// TestTrigramIndex_QueryOverlapScores_MatchesItemScore is a small, direct
// cross-check of the new batch API against the frozen per-item reference —
// see memory_search_golden_test.go's "Trigram posting-list equivalence"
// section for the exhaustive version over the full golden corpus.
func TestTrigramIndex_QueryOverlapScores_MatchesItemScore(t *testing.T) {
	idx := NewTrigramIndex()
	idx.Index("id1", "Widget RAG", "Enterprise knowledge graph")
	idx.Index("id2", "Other REC", "Recording platform")
	idx.Index("id3", "服务降级", "数据库连接池耗尽")

	toks := []string{"widget", "rag", "zzz"}
	tokGrams := TokenGrams(toks)
	got := idx.QueryOverlapScores(tokGrams)

	for _, id := range []string{"id1", "id2", "id3", "nonexistent"} {
		want := idx.ItemScore(id, toks)
		assert.Equal(t, want, got[id], "id=%s", id)
	}
}

// TestTrigramIndex_Search_MatchesTokMatchesItemBruteForce cross-checks the
// round-5 posting-merge search() against a brute-force reference built on
// the unchanged tokMatchesItem predicate — the same equivalence argument
// search()'s doc comment makes, verified directly instead of just argued.
func TestTrigramIndex_Search_MatchesTokMatchesItemBruteForce(t *testing.T) {
	idx := NewTrigramIndex()
	corpus := map[string][2]string{
		"id1": {"Widget RAG", "Enterprise knowledge graph"},
		"id2": {"Other REC", "Recording platform"},
		"id3": {"Widget Other", "totally different content"},
		"id4": {"服务降级 café", "connexion échouée réessai"},
	}
	for id, tc := range corpus {
		idx.Index(id, tc[0], tc[1])
	}

	for _, q := range []string{"Widget RAG", "widges", "rec", "café", "服务", "completely unrelated"} {
		t.Run(q, func(t *testing.T) {
			got := idx.Search(q)

			toks := KwTokens(NormalizeQuery(q))
			var want []string
			if len(toks) > 0 {
				for id := range corpus {
					ok := true
					idx.Mu.RLock()
					for _, tok := range toks {
						if !idx.TokMatchesItem(id, tok) {
							ok = false
							break
						}
					}
					idx.Mu.RUnlock()
					if ok {
						want = append(want, id)
					}
				}
			}

			assert.ElementsMatch(t, want, got)
		})
	}
}

// ── trigramIndex.Search / tokMatchesItem (pre-filter, ported from xray's
// internal/xray/kwindex.go) ────────────────────────────────────────────────

func TestTokThreshold_ScalesByLength(t *testing.T) {
	assert.Equal(t, 1.0, tokThreshold(2), "short tokens require exact match")
	assert.Equal(t, 0.75, tokThreshold(4), "medium tokens allow 1-in-4 miss")
	assert.Equal(t, 0.90, tokThreshold(5), "long tokens are tight")
}

func TestTrigramIndex_TokMatchesItem(t *testing.T) {
	idx := NewTrigramIndex()
	idx.Index("id1", "Widget RAG", "Enterprise knowledge graph")

	assert.True(t, idx.TokMatchesItem("id1", "widget"), "exact word must match")
	assert.True(t, idx.TokMatchesItem("id1", "widges"), "1-trigram-off medium word clears 0.75 threshold")
	assert.False(t, idx.TokMatchesItem("id1", "zzz"), "unrelated token must not match")
}

func TestTrigramIndex_Search_FindsCandidates(t *testing.T) {
	idx := NewTrigramIndex()
	idx.Index("id1", "Widget RAG", "Enterprise knowledge graph")
	idx.Index("id2", "Other REC", "Recording platform")

	ids := idx.Search("Widget RAG")
	require.Contains(t, ids, "id1")
	assert.NotContains(t, ids, "id2")
}

func TestTrigramIndex_Search_EmptyQueryReturnsNil(t *testing.T) {
	idx := NewTrigramIndex()
	idx.Index("id1", "Widget RAG", "content")
	assert.Nil(t, idx.Search(""))
}

func TestTrigramIndex_Search_NoMatchReturnsEmpty(t *testing.T) {
	idx := NewTrigramIndex()
	idx.Index("id1", "Widget RAG", "content")
	assert.Empty(t, idx.Search("completely unrelated"))
}

// ── lexical helpers ─────────────────────────────────────────────────────────

func TestNormalizeQuery_CamelCaseSplit(t *testing.T) {
	assert.Equal(t, "widget rec", NormalizeQuery("WidgetREC"))
	assert.Equal(t, "ri service", NormalizeQuery("ri_service"))
	assert.Equal(t, "ri service", NormalizeQuery("ri-service"))
}

func TestKwTokens(t *testing.T) {
	assert.Equal(t, []string{"widget", "rag"}, KwTokens("widget rag"))
	assert.Empty(t, KwTokens(""))
}

func TestEditDistance_Runes(t *testing.T) {
	assert.Equal(t, 0, editDistance("widget", "widget"))
	assert.Equal(t, 1, editDistance("widges", "widget"))
	assert.Equal(t, 1, editDistance("déjà", "déja"))
}

func TestFuzzyWordMatch_ToleranceScaling(t *testing.T) {
	// 1-2 char tokens: exact only.
	assert.False(t, fuzzyWordMatch("re rec", "r2"))
	// 3+ char tokens: 1-edit tolerance.
	assert.True(t, fuzzyWordMatch("widget rag", "widges"))
	assert.False(t, fuzzyWordMatch("widget rag", "totallydifferent"))
}

func TestFuzzyWordMatch_CamelCaseJoin(t *testing.T) {
	// NormalizeQuery splits "LaunchFlow" -> "launch flow"; a mishear token
	// like "lunchflow" must still match the original single word.
	assert.True(t, fuzzyWordMatch("launch flow", "lunchflow"))
}

// ── fuseRRF ──────────────────────────────────────────────────────────────

func TestFuseRRF_OrdersByFusedRank(t *testing.T) {
	a := Item{}
	a.ID = "a"
	b := Item{}
	b.ID = "b"
	c := Item{}
	c.ID = "c"

	lex := []Item{a, b} // a rank0, b rank1
	sem := []Item{b, c} // b rank0, c rank1 — b appears in both, should win

	out := fuseRRF(lex, sem, nil, 10, true)
	assert.Equal(t, "b", out[0].ID)
}

func TestFuseRRF_RespectsLimit(t *testing.T) {
	items := make([]Item, 5)
	for i := range items {
		items[i].ID = string(rune('a' + i))
	}
	out := fuseRRF(items, nil, nil, 2, true)
	assert.Len(t, out, 2)
}

// editDistance returns the Levenshtein distance between a and b, measured in
// runes. Must operate on runes, not bytes: a byte-indexed DP gives garbage
// distances for multibyte words because range-over-string yields byte offsets
// while the slices are sized by byte length.
func editDistance(a, b string) int {
	if a == b {
		return 0
	}
	ra := []rune(a)
	rb := []rune(b)
	if len(ra) == 0 {
		return len(rb)
	}
	if len(rb) == 0 {
		return len(ra)
	}
	prev := make([]int, len(rb)+1)
	curr := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := range ra {
		curr[0] = i + 1
		for j := range rb {
			cost := 1
			if ra[i] == rb[j] {
				cost = 0
			}
			curr[j+1] = min3(curr[j]+1, prev[j+1]+1, prev[j]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(rb)]
}

// fuzzyWordMatch reports whether t is within its length-scaled edit tolerance of
// any single word in h. It also tests t against adjacent word pairs joined
// without a space, because NormalizeQuery splits CamelCase: a mishear token
// like "lunchflow" must still match the original single word.
func fuzzyWordMatch(h, t string) bool {
	return fuzzyWordMatchFields(newWordTokens(strings.Fields(h)), t)
}
