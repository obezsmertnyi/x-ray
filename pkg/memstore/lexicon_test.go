package memstore

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

func sortedIDs(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	return ids2(out)
}

func ids2(in []string) []string {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j] < in[j-1]; j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
	return in
}

func testLexicon() *Lexicon {
	l := NewLexicon()
	l.Index("billing", "Billing retries", "Failed charges are retried three times")
	l.Index("queue", "Ingest queue", "The ingest queue drains once an hour.")
	l.Index("db", "Database", "Primary db failover runs nightly")
	return l
}

func TestLexicon_FindsAWholeWordAndASubstring(t *testing.T) {
	l := testLexicon()
	assert.Equal(t, []string{"queue"}, sortedIDs(l.Candidates("drains")))
	assert.Equal(t, []string{"billing"}, sortedIDs(l.Candidates("charge")), "a token is a substring of a word")
}

func TestLexicon_ProbesOneEditOnATokenOfThreeOrMoreCharacters(t *testing.T) {
	l := testLexicon()
	assert.Equal(t, []string{"queue"}, sortedIDs(l.Candidates("quue")), "a missing letter")
	assert.Equal(t, []string{"billing"}, sortedIDs(l.Candidates("retrias")), "a substituted letter")
	assert.Empty(t, l.Candidates("quxxe"), "two edits is too far")
}

func TestLexicon_AShortTokenIsNeverProbed(t *testing.T) {
	l := testLexicon()
	assert.Empty(t, l.Candidates("dx"), "a token of two characters matches by substring only")
	assert.Equal(t, []string{"db"}, sortedIDs(l.Candidates("db")))
}

func TestLexicon_AdjacentWordsJoinedWithinOneEditMatchAToken(t *testing.T) {
	l := testLexicon()
	assert.Equal(t, []string{"queue"}, sortedIDs(l.Candidates("ingestqueue")), "a query that ran two words together")
	assert.Empty(t, l.Candidates("tqu"), "a join is probed by edit distance, never by substring")
}

func TestLexicon_PunctuationAndFillerAreRemovedFromTheQuery(t *testing.T) {
	l := testLexicon()
	assert.Equal(t, []string{"queue"}, sortedIDs(l.Candidates("What drains, exactly?")))
	assert.Nil(t, l.Candidates(""), "an empty query has no tokens")
	assert.Nil(t, l.Candidates("?!"), "punctuation alone has no tokens")
}

func TestLexicon_UnionsTheTokensOfAQuery(t *testing.T) {
	l := testLexicon()
	assert.Equal(t, []string{"billing", "queue"}, sortedIDs(l.Candidates("drains retried")))
}

func TestLexicon_IndexReplacesAndRemoveForgets(t *testing.T) {
	l := testLexicon()
	assert.Equal(t, 3, l.Len())
	l.Index("queue", "Ingest queue", "Now it is flushed on demand")
	assert.Empty(t, l.Candidates("drains"), "the old words are gone")
	assert.Equal(t, []string{"queue"}, sortedIDs(l.Candidates("flushed")))
	l.Remove("queue")
	l.Remove("nobody")
	assert.Empty(t, l.Candidates("flushed"))
	assert.Equal(t, 2, l.Len())
}

func TestLexicon_DocFreqCountsWholeWords(t *testing.T) {
	l := testLexicon()
	l.Index("second", "Another queue", "queue length alarms")
	assert.Equal(t, 2, l.DocFreq("queue"))
	assert.Equal(t, 0, l.DocFreq("que"), "a part of a word is not a word")
}

func TestLexicon_IsSafeForConcurrentUse(t *testing.T) {
	l := testLexicon()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				l.Index("w"+string(rune('a'+i)), "title", "some words to index")
				_ = l.Candidates("words")
				l.Remove("w" + string(rune('a'+i)))
			}
		}()
	}
	wg.Wait()
}
