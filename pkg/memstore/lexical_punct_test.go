package memstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeQuery_StripsPunctuationAtTokenBoundaries(t *testing.T) {
	cases := map[string]string{
		"what is wrong with payment-api?": "what is wrong with payment api",
		"db?":                             "db",
		"ui, db.":                         "ui db",
		"say \"hello\" (world)":           "say hello world",
		"`pip install vllm`.":             "pip install vllm",
		"a/b|c":                           "a b c",
		"  spaced   out  ":                "spaced out",
		"WidgetREC":                       "widget rec",
	}
	for in, want := range cases {
		assert.Equal(t, want, NormalizeQuery(in), in)
	}
}

func TestKwTokens_HaveNoPunctuation(t *testing.T) {
	assert.Equal(t, []string{"wrong", "payment", "api"}, KwTokens(NormalizeQuery("what is wrong with payment-api?")))
	assert.Equal(t, []string{"db"}, KwTokens("db?"))
}

func TestLexicalCache_WordsCarryNoTrailingPunctuation(t *testing.T) {
	it := Item{}
	it.ID, it.Title, it.Content = "a", "Incident review.", "The bigquery, job: failed (twice)."
	buildLexicalCache(&it)
	assert.Equal(t, []string{"incident", "review"}, it.lex.titleLowerWT.words)
	assert.Contains(t, it.lex.haystackLowerWT.words, "bigquery")
	assert.Contains(t, it.lex.haystackLowerWT.words, "job")
	assert.Contains(t, it.lex.haystackLowerWT.words, "twice")
	assert.NotContains(t, it.lex.haystackLowerWT.words, "bigquery,")
}

func TestVocabulary_DocumentFrequencyCountsAWordFollowedByPunctuation(t *testing.T) {
	s := NewStore()
	upsert(t, s, mk("a", "note", "t", "We use bigquery. It is fast."))
	upsert(t, s, mk("b", "note", "t", "Costs of bigquery, again, matter."))
	upsert(t, s, mk("c", "note", "t", "Nothing relevant here at all."))
	assert.Equal(t, 2, s.vocab.wordDocFreq("bigquery"), "a trailing period or comma does not hide the word from the document frequency")
}

func TestKeywordSearch_ATrailingQuestionMarkStillMatchesExactly(t *testing.T) {
	s := NewStore()
	upsert(t, s, mk("a", "note", "payment-api", "the payment service was down for ten minutes"))
	q := NormalizeQuery("payment-api?")
	toks := KwTokens(q)
	require.Equal(t, []string{"payment", "api"}, toks)

	items := s.KeywordSearch("payment-api?", "", "", 5)
	require.Len(t, items, 1)
	nt, ntf, nc, ncf := normFieldsFor(items[0])
	assert.Equal(t, wTokExactTle, tokenMatchQualityFields(nt, ntf, nc, ncf, toks[1]), "api? no longer degrades to a fuzzy match")
	assert.GreaterOrEqual(t, lexScoreFields(nt, ntf, nc, ncf, q, toks, nil, 0), wExactTitle, "the whole-query title bonus is not lost to the question mark")
}

func TestTrigramIndex_IndexesWordsWithoutPunctuation(t *testing.T) {
	idx := NewTrigramIndex()
	idx.Index("a", "Review.", "the bigquery, job")
	assert.Equal(t, 1.0, idx.ItemScore("a", []string{"bigquery"}))
	assert.Equal(t, 1.0, idx.ItemScore("a", []string{"review"}))
	assert.Equal(t, []string{"a"}, idx.Search("bigquery job?"))
}
