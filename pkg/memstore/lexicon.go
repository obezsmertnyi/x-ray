package memstore

// Lexicon is the store's vocabulary index on its own: a corpus-wide inverted
// index from the words of each document to the ids that contain them, which
// answers "which documents could match this query" without scanning them all.
// A host with its own documents (not memory items) uses it to prefilter a
// scan before scoring.
//
// A query token matches a document when the token is a substring of one of the
// document's words, or when it is within one edit of a word (or of two adjacent
// words joined without a space) and is longer than two characters. The probing
// is length-banded: a token is compared only with the entries whose length is
// within one of its own, so the cost does not grow with the whole vocabulary.
// The set is exact, not a heuristic: it holds every document the store's own
// lexical match would admit for the token, so scoring the set finds everything
// a scan of all documents would. It is safe for concurrent use.
type Lexicon struct {
	vi *vocabIndex
}

// NewLexicon returns an empty Lexicon.
func NewLexicon() *Lexicon { return &Lexicon{vi: newVocabIndex()} }

// Index (re)indexes the document id from its title and content, replacing
// whatever id contributed before. Text is split into words the way the store
// splits an item's: lower-cased, punctuation stripped.
func (l *Lexicon) Index(id, title, content string) {
	var it Item
	it.ID, it.Title, it.Content = id, title, content
	buildLexicalCacheText(&it, content)
	words, joins := vocabWordsForItem(it)
	l.vi.Index(id, words, joins)
}

// Remove drops everything id contributed. An unknown id is a no-op.
func (l *Lexicon) Remove(id string) { l.vi.Remove(id) }

// Candidates returns the ids of the documents that can match at least one token
// of query, the query being tokenised as the store tokenises one (punctuation
// removed, conversational filler dropped unless the query is only filler). A
// query with no tokens returns nil.
func (l *Lexicon) Candidates(query string) map[string]struct{} {
	return l.vi.CandidateIDsForTokens(KwTokens(NormalizeQuery(query)))
}

// CandidatesForTokens is Candidates for tokens the caller has already produced
// with KwTokens.
func (l *Lexicon) CandidatesForTokens(tokens []string) map[string]struct{} {
	return l.vi.CandidateIDsForTokens(tokens)
}

// DocFreq is the number of documents that contain word as a whole word,
// lower-cased: the figure a distinctiveness weight is computed from.
func (l *Lexicon) DocFreq(word string) int { return l.vi.wordDocFreq(word) }

// Len is the number of documents indexed.
func (l *Lexicon) Len() int {
	l.vi.Mu.RLock()
	defer l.vi.Mu.RUnlock()
	return len(l.vi.byItemWords)
}
