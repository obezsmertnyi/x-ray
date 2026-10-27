package memstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A lexical answer is confident when its top item covers the query, which lets
// a hybrid search skip the semantic round trip. Coverage by token count alone
// lets an item that matches only the ordinary words of the query, and misses the
// one token that names the thing asked about, short-circuit the search.
func TestKeywordSearchScored_MissingTheDistinctiveTokenIsNotConfident(t *testing.T) {
	s := NewStore()
	for i := 0; i < 20; i++ {
		upsert(t, s, mk(fmtID("n", i), "note", "alpha beta note", "alpha beta and some other ordinary words"))
	}
	upsert(t, s, mk("rare", "fact", "gamma", "the only item that names gamma"))

	items, confident := s.KeywordSearchScored("alpha beta gamma", "note", "", 5)
	require.NotEmpty(t, items)
	toks := KwTokens(NormalizeQuery("alpha beta gamma"))
	nt, ntf, nc, ncf := lowerFieldsFor(items[0])
	matched, _ := softMatchCoverageFields(nt, ntf, nc, ncf, toks)
	require.Equal(t, 2, matched, "the top item covers two of three tokens, which the count rule accepts")
	assert.False(t, confident, "but those two tokens carry little of the query's weight")
}

func TestKeywordSearchScored_FullCoverageIsConfident(t *testing.T) {
	s := NewStore()
	upsert(t, s, mk("a", "note", "alpha beta gamma", "all three words are here"))
	upsert(t, s, mk("b", "note", "unrelated", "something else entirely"))
	_, confident := s.KeywordSearchScored("alpha beta gamma", "", "", 5)
	assert.True(t, confident)
}

func fmtID(prefix string, i int) string { return prefix + string(rune('a'+i)) }
