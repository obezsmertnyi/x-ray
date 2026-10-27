package memstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestKwTokens_DropsConversationalStopwords(t *testing.T) {
	assert.Equal(t, []string{"status", "auth"}, KwTokens(NormalizeQuery("what is the status of auth?")))
	assert.Equal(t, []string{"wrong", "payment", "api"}, KwTokens(NormalizeQuery("What's wrong with payment-api?")), "the 's' left by an apostrophe is dropped with the stopwords")
	assert.Equal(t, []string{"plan", "q3"}, KwTokens(NormalizeQuery("a plan for q3")), "a single letter is noise, a short token with a digit is not")
}

func TestKwTokens_AQueryOfOnlyStopwordsKeepsItsWords(t *testing.T) {
	assert.Equal(t, []string{"what", "is", "it"}, KwTokens(NormalizeQuery("what is it?")), "filtering everything would leave the lexical channel mute")
	assert.Equal(t, []string{"to", "be", "or", "not", "to", "be"}, KwTokens("to be or not to be"))
}

func TestKeywordSearch_FillerWordsNoLongerAdmitAnUnrelatedItem(t *testing.T) {
	s := NewStore()
	upsert(t, s, mk("prose", "note", "weekly status", "what is the status of the roadmap and who owns it at the moment"))
	upsert(t, s, mk("auth", "note", "auth service", "the auth service issues the session tokens"))
	got := s.KeywordSearch("what is the status of auth", "", "", 5)
	assert.Equal(t, "auth", got[0].ID, "the entity outranks prose that merely contains the filler")
}

func TestDedupTokens_AreNotFilteredByQueryStopwords(t *testing.T) {
	it := mk("a", "note", "Run the migration", "we need to use the new tool")
	toks := DedupTokens(it)
	assert.Contains(t, toks, "run", "the dedup list is its own: a query stopword is still a word of the item")
	assert.Contains(t, toks, "use")
	assert.NotContains(t, toks, "the")
}
