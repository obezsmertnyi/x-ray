package vectorstore

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/fogfish/hnsw"
	"github.com/kshard/vector"
	"github.com/rs/zerolog"
)

// MemoryCellIndexEntry is the per-cell record indexed in MemoryCellStore: the
// searchable metadata projection of a memory cell (Title+Content is what gets
// embedded; the other fields drive filtering). Not a document/file.
type MemoryCellIndexEntry struct {
	ID         string
	Kind       string
	Scope      string
	Subject    string
	Title      string
	Content    string
	Confidence string
	Status     string
	Tags       []string
	EventDate  string // RFC3339, only set for kind=event
}

// CellSearchResult is a memory cell returned by MemoryCellStore.Search.
type CellSearchResult struct {
	Entry MemoryCellIndexEntry
	Score float32
}

// cellEntry is a single indexed cell with its embedding.
type cellEntry struct {
	Cell      MemoryCellIndexEntry
	Embedding []float32
}

// cellNodeEmbedding is the HNSW vector node for a memory cell.
type cellNodeEmbedding struct {
	Key string
	Vec vector.F32
}

var cellSurface = vector.ContraMap[vector.F32, cellNodeEmbedding]{
	Surface:   vector.Cosine(),
	ContraMap: func(e cellNodeEmbedding) vector.F32 { return e.Vec },
}

// subjectIndex is a per-subject HNSW index — subjects never share a graph.
type subjectIndex struct {
	mu      sync.RWMutex
	entries map[string]cellEntry
	index   *hnsw.HNSW[cellNodeEmbedding]
	// dirty is set when the graph holds nodes that entries no longer backs: a
	// delete (hnsw has no incremental delete) or an update that replaced an
	// embedding. freshen rebuilds once before the next graph read, so a delete
	// storm costs one rebuild instead of one per delete. Until then every read
	// filters hits by presence in entries, so a deleted id is never returned.
	dirty bool
}

func newSubjectIndex() *subjectIndex {
	return &subjectIndex{entries: make(map[string]cellEntry), index: newGraph()}
}

func newGraph() *hnsw.HNSW[cellNodeEmbedding] {
	return hnsw.New(cellSurface,
		hnsw.WithEfConstruction(200),
		hnsw.WithM(16),
		hnsw.WithM0(32),
	)
}

// freshen rebuilds the graph if it is dirty. The graph is built outside the
// lock from a snapshot and swapped in under a brief write lock, so readers wait
// only for the swap, not the build (~4s at 10k nodes). Entries inserted while
// the build ran went to the old graph; they are added to the new one before it
// is swapped in. A delete during the build leaves its node in the new graph
// and sets dirty again, so the next read rebuilds.
func (s *subjectIndex) freshen() {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return
	}
	s.dirty = false
	snapshot := make(map[string]cellEntry, len(s.entries))
	for id, e := range s.entries {
		snapshot[id] = e
	}
	s.mu.Unlock()

	g := newGraph()
	for id, e := range snapshot {
		if len(e.Embedding) > 0 {
			g.Insert(cellNodeEmbedding{Key: id, Vec: vector.F32(e.Embedding)})
		}
	}

	s.mu.Lock()
	for id, e := range s.entries {
		if old, ok := snapshot[id]; ok && slices.Equal(old.Embedding, e.Embedding) {
			continue
		}
		if len(e.Embedding) > 0 {
			g.Insert(cellNodeEmbedding{Key: id, Vec: vector.F32(e.Embedding)})
		}
	}
	s.index = g
	s.mu.Unlock()
}

func (s *subjectIndex) insert(doc MemoryCellIndexEntry, embedding []float32) {
	s.mu.Lock()
	// Re-seeding an unchanged cell (every reconcile does) must not dirty the
	// graph; replacing an embedding leaves the old node behind, which does.
	if old, ok := s.entries[doc.ID]; ok && !slices.Equal(old.Embedding, embedding) {
		s.dirty = true
	}
	s.entries[doc.ID] = cellEntry{Cell: doc, Embedding: embedding}
	s.index.Insert(cellNodeEmbedding{Key: doc.ID, Vec: vector.F32(embedding)})
	s.mu.Unlock()
}

// remove drops id from entries and reports whether it was there. The graph is
// rebuilt lazily (see dirty).
func (s *subjectIndex) remove(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[id]; !ok {
		return false
	}
	delete(s.entries, id)
	s.dirty = true
	return true
}

func (s *subjectIndex) search(embedding []float32, topK int, kindFilter string) []CellSearchResult {
	s.freshen()

	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.entries) == 0 {
		return nil
	}
	// A kind-filtered search exact-scans that kind's entries instead of asking
	// the graph. Fetching topK*N global neighbours and dropping the other kinds
	// starves a minority kind: its cells never reach the fetch window when a
	// populous kind fills it, so the search returns nothing for a kind that has
	// plenty of matching cells. Scanning one kind is at most a few hundred dot
	// products.
	if kindFilter != "" {
		return scanEntriesByKind(embedding, s.entries, topK, kindFilter)
	}

	q := cellNodeEmbedding{Vec: vector.F32(embedding)}
	neighbors := s.index.Search(q, topK, 50)
	results := make([]CellSearchResult, 0, len(neighbors))
	for _, nb := range neighbors {
		entry, ok := s.entries[nb.Key]
		if !ok {
			continue
		}
		dist := s.index.Distance(q, nb)
		sim := float32(1.0) - dist/2.0
		results = append(results, CellSearchResult{Entry: entry.Cell, Score: sim})
		if len(results) >= topK {
			break
		}
	}

	sort.Slice(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	// Relevance cutoff is applied on the merged cross-subject result (applyScoreGap)
	// so the gap is measured against the global top, not a per-subject top.
	return results
}

// scanEntriesByKind scores every entry of kind (every entry when kind is empty)
// against embedding with the same cosine surface the graph uses, best first,
// truncated to topK (all of them when topK <= 0). The caller holds the index's
// read lock.
func scanEntriesByKind(embedding []float32, entries map[string]cellEntry, topK int, kind string) []CellSearchResult {
	results := make([]CellSearchResult, 0, len(entries))
	for _, entry := range entries {
		if (kind != "" && entry.Cell.Kind != kind) || len(entry.Embedding) == 0 {
			continue
		}
		dist := cellSurface.Surface.Distance(embedding, entry.Embedding)
		results = append(results, CellSearchResult{Entry: entry.Cell, Score: float32(1.0) - dist/2.0})
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	if topK > 0 && len(results) > topK {
		results = results[:topK]
	}
	return results
}

// defaultSemanticScoreGap is the default absolute cosine-similarity drop, below
// the top hit, past which semantic results are dropped as near-misses. An
// absolute gap (not a % of top) because nomic@256d compresses short-query
// similarities into a narrow band (~0.88–0.92 observed) where a ratio cutoff
// keeps obvious junk — the top match and an unrelated cell differ by only ~4%
// but ~0.04 in absolute cosine. Measured: for "prediction presentation" the one
// real match sits 0.023+ above every unrelated neighbor, so 0.02 isolates it.
// Overridable per-deployment via --semantic-score-gap (SetScoreGap).
const defaultSemanticScoreGap float32 = 0.02

// MemoryCellStore holds a per-subject HNSW index. Each subject's cells are
// isolated so cross-subject interference is impossible.
type MemoryCellStore struct {
	mu       sync.RWMutex
	subjects map[string]*subjectIndex
	embedder *Embedder
	logger   zerolog.Logger
	scoreGap float32 // absolute cosine gap below top to keep; 0 disables the cutoff

	// queryCache is the exact-match LRU in front of every query-side embed this
	// store makes. Documents are embedded once, at index time, and never cached.
	// nil when embedder is nil.
	queryCache *queryEmbedCache

	// dim is the embedding dimension of every vector in the store (one embedder,
	// so one dimension across subjects): the embedder's configured output
	// dimension when known, else the first vector inserted or seeded. 0 means
	// not yet known. Guarded by mu.
	dim int
}

// NewMemoryCellStore creates an empty MemoryCellStore.
func NewMemoryCellStore(embedder *Embedder, logger zerolog.Logger) *MemoryCellStore {
	s := &MemoryCellStore{
		subjects: make(map[string]*subjectIndex),
		embedder: embedder,
		logger:   logger,
		scoreGap: defaultSemanticScoreGap,
	}
	if embedder != nil {
		s.queryCache = newQueryEmbedCache(embedder, defaultQueryEmbedCacheCapacity, embedder.maxInputChars)
		s.dim = embedder.OutputDims()
	}
	return s
}

// embedQuery routes a search-query embedding through the store's cache.
func (s *MemoryCellStore) embedQuery(ctx context.Context, text string) ([]float32, error) {
	if s.queryCache == nil {
		return s.embedder.EmbedQuery(ctx, text)
	}
	return s.queryCache.Get(ctx, text)
}

// SearchTiming is the embed-versus-index split of one search: an embedder
// latency spike and an in-memory scan spike are indistinguishable from the total.
type SearchTiming struct {
	Embed time.Duration
	Index time.Duration
}

type timingSinkKey struct{}

// WithTimingSink asks Search, SearchSubject and SearchExhaustive to report their
// phase split to fn.
func WithTimingSink(ctx context.Context, fn func(SearchTiming)) context.Context {
	return context.WithValue(ctx, timingSinkKey{}, fn)
}

func reportTiming(ctx context.Context, t SearchTiming) {
	if fn, ok := ctx.Value(timingSinkKey{}).(func(SearchTiming)); ok {
		fn(t)
	}
}

// QueryEmbedCacheStats reads the query-embedding cache counters; zero when the
// store has no cache.
func (s *MemoryCellStore) QueryEmbedCacheStats() QueryEmbedCacheStats {
	if s.queryCache == nil {
		return QueryEmbedCacheStats{}
	}
	return s.queryCache.Stats()
}

// checkDim sets the store dimension from vec when it is unknown, else reports
// whether vec has it, logging a mismatch.
func (s *MemoryCellStore) checkDim(id string, vec []float32) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dim == 0 {
		s.dim = len(vec)
		return true
	}
	if len(vec) != s.dim {
		s.logger.Warn().Str("id", id).Int("got", len(vec)).Int("want", s.dim).
			Msg("memory cell store: embedding dimension mismatch, rejecting insert")
		return false
	}
	return true
}

// dimMismatch reports whether a query vector disagrees with the store
// dimension. A store with nothing indexed never mismatches.
func (s *MemoryCellStore) dimMismatch(vec []float32) bool {
	s.mu.RLock()
	dim := s.dim
	s.mu.RUnlock()
	if dim == 0 || len(vec) == dim {
		return false
	}
	s.logger.Warn().Int("got", len(vec)).Int("want", dim).
		Msg("memory cell store: query embedding dimension mismatch, rejecting search")
	return true
}

// snapshotSubjects copies the subject index pointers under the lock, so a
// concurrent getOrCreate cannot race the iteration.
func (s *MemoryCellStore) snapshotSubjects() []*subjectIndex {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*subjectIndex, 0, len(s.subjects))
	for _, idx := range s.subjects {
		out = append(out, idx)
	}
	return out
}

// SetScoreGap overrides the semantic relevance cutoff (absolute cosine gap below
// the top hit). 0 disables the cutoff (return raw top-K). Wired from the
// --semantic-score-gap flag.
func (s *MemoryCellStore) SetScoreGap(gap float32) {
	if gap >= 0 {
		s.scoreGap = gap
	}
}

// applyScoreGap assumes results are sorted by Score descending and trims the
// tail that falls more than s.scoreGap below the top score. A zero gap disables
// trimming.
func (s *MemoryCellStore) applyScoreGap(results []CellSearchResult) []CellSearchResult {
	if s.scoreGap <= 0 || len(results) <= 1 {
		return results
	}
	floor := results[0].Score - s.scoreGap
	for i, r := range results {
		if r.Score < floor {
			return results[:i]
		}
	}
	return results
}

func (s *MemoryCellStore) getOrCreate(subject string) *subjectIndex {
	s.mu.RLock()
	idx := s.subjects[subject]
	s.mu.RUnlock()
	if idx != nil {
		return idx
	}
	s.mu.Lock()
	if s.subjects[subject] == nil {
		s.subjects[subject] = newSubjectIndex()
	}
	idx = s.subjects[subject]
	s.mu.Unlock()
	return idx
}

// Index embeds and stores a memory cell document.
func (s *MemoryCellStore) Index(ctx context.Context, doc MemoryCellIndexEntry) ([]float32, error) {
	embedding, err := s.embedder.EmbedDocument(ctx, cellEmbedText(doc))
	if err != nil {
		return nil, fmt.Errorf("embed cell %s: %w", doc.ID, err)
	}
	if !s.checkDim(doc.ID, embedding) {
		return nil, fmt.Errorf("embed cell %s: dimension %d does not match index dimension", doc.ID, len(embedding))
	}
	s.getOrCreate(doc.Subject).insert(doc, embedding)
	return embedding, nil
}

// SeedFromEmbedding inserts a pre-embedded cell without calling the embedding
// API. It returns false, inserting nothing, when the vector's length disagrees
// with the store dimension (a cell embedded by another model): the caller
// should re-embed it. An empty embedding has nothing to seed and returns true.
//
// The dimension is the embedder's configured output dimension when known, so a
// stale wrong-dimension cell seeded first cannot lock the store to a wrong
// shape; a seeded vector only sets it when it is not known.
func (s *MemoryCellStore) SeedFromEmbedding(doc MemoryCellIndexEntry, embedding []float32) bool {
	if len(embedding) == 0 {
		return true
	}
	if !s.checkDim(doc.ID, embedding) {
		return false
	}
	s.getOrCreate(doc.Subject).insert(doc, embedding)
	return true
}

// Search returns top-k results for the given subject's index only.
// The subject field in the doc is used to route to the correct index.
func (s *MemoryCellStore) Search(ctx context.Context, query string, topK int, kindFilter string) ([]CellSearchResult, error) {
	// This method is kept for interface compatibility but subject-scoped
	// search should use SearchSubject.
	embedStart := time.Now()
	embedding, err := s.embedQuery(ctx, query)
	timing := SearchTiming{Embed: time.Since(embedStart)}
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	if s.dimMismatch(embedding) {
		return nil, nil
	}
	indexStart := time.Now()
	var all []CellSearchResult
	for _, idx := range s.snapshotSubjects() {
		all = append(all, idx.search(embedding, topK, kindFilter)...)
	}
	timing.Index = time.Since(indexStart)
	reportTiming(ctx, timing)
	sort.Slice(all, func(i, j int) bool { return all[i].Score > all[j].Score })
	logSearchScores(s.logger, "Search", query, all)
	all = s.applyScoreGap(all)
	if len(all) > topK {
		all = all[:topK]
	}
	return all, nil
}

// SearchSubject searches within the given subject's index and merges results
// from the global (empty subject) index, returning top-k by score.
func (s *MemoryCellStore) SearchSubject(ctx context.Context, query, subject string, topK int, kindFilter string) ([]CellSearchResult, error) {
	embedStart := time.Now()
	embedding, err := s.embedQuery(ctx, query)
	timing := SearchTiming{Embed: time.Since(embedStart)}
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	if s.dimMismatch(embedding) {
		return nil, nil
	}
	indexStart := time.Now()
	s.mu.RLock()
	subjectIdx := s.subjects[subject]
	globalIdx := s.subjects[""]
	s.mu.RUnlock()

	var results []CellSearchResult
	if subjectIdx != nil {
		results = append(results, subjectIdx.search(embedding, topK, kindFilter)...)
	}
	if globalIdx != nil && subject != "" {
		results = append(results, globalIdx.search(embedding, topK, kindFilter)...)
	}

	timing.Index = time.Since(indexStart)
	reportTiming(ctx, timing)
	sort.Slice(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	logSearchScores(s.logger, "SearchSubject", query, results)
	results = s.applyScoreGap(results)
	if len(results) > topK {
		results = results[:topK]
	}
	return results, nil
}

// logSearchScores emits the per-result cosine score spread (absolute score and
// %-of-top) at trace level, so the --semantic-score-gap cutoff can be observed
// and re-tuned against live data. Silent below --log-level=trace.
func logSearchScores(logger zerolog.Logger, where, query string, results []CellSearchResult) {
	if len(results) == 0 || logger.GetLevel() > zerolog.TraceLevel {
		return
	}
	ev := logger.Trace().Str("where", where).Str("query", query).Int("n", len(results))
	top := results[0].Score
	for i, r := range results {
		frac := float32(1)
		if top != 0 {
			frac = r.Score / top
		}
		ev = ev.Str(fmt.Sprintf("r%02d", i),
			fmt.Sprintf("%.4f (%.1f%% of top) %q", r.Score, frac*100, truncateTitle(r.Entry.Title, 40)))
	}
	ev.Msg("semantic score spread")
}

func truncateTitle(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Delete removes a memory cell by ID across all subject indexes. The graph is
// rebuilt lazily before the next search (see subjectIndex.dirty), so a sweep
// that deletes hundreds of cells costs one rebuild.
func (s *MemoryCellStore) Delete(_ context.Context, id string) error {
	for _, idx := range s.snapshotSubjects() {
		if idx.remove(id) {
			return nil
		}
	}
	return nil
}

// CollectionSize returns total indexed cells across all subjects.
func (s *MemoryCellStore) CollectionSize() int64 {
	var n int64
	for _, idx := range s.snapshotSubjects() {
		idx.mu.RLock()
		n += int64(len(idx.entries))
		idx.mu.RUnlock()
	}
	return n
}

func cellEmbedText(doc MemoryCellIndexEntry) string {
	parts := []string{doc.Kind}
	if doc.Scope != "" {
		parts = append(parts, doc.Scope)
	}
	if doc.Subject != "" {
		parts = append(parts, doc.Subject)
	}
	parts = append(parts, doc.Title, doc.Content)
	return capEmbedText(strings.Join(parts, " "))
}

// maxCellEmbedChars caps the text one cell sends to the embedder. A local
// embedder rejects an input over its batch size (2048 tokens) outright, and a
// long note passed that and failed to index forever. JSON runs about 2 bytes a
// token and the tokenizer never makes more tokens than bytes, so a cap under
// 2048 bytes is the one that always fits; the head (kind, subject, title, the
// start of the content) is what similarity needs.
const maxCellEmbedChars = 1800

// capEmbedText cuts s to maxCellEmbedChars bytes on a rune boundary.
func capEmbedText(s string) string {
	if len(s) <= maxCellEmbedChars {
		return s
	}
	cut := maxCellEmbedChars
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// SearchExhaustive ranks every entry the caller may see by exact cosine, with no
// graph and no fetch window: the subject's own cells and the global ones, or
// every subject's when subject is empty. It exists for one caller. A graph
// search returns its top-K window, and a caller that drops hits afterwards (a
// cell deleted or retired on another replica is still indexed here) can see the
// whole window rejected although other cells would have qualified. This hands
// back the full ranked population so the caller's filter gets every candidate.
// No score gap is applied.
func (s *MemoryCellStore) SearchExhaustive(ctx context.Context, query, subject string, kindFilter string) ([]CellSearchResult, error) {
	embedding, err := s.embedQuery(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	if s.dimMismatch(embedding) {
		return nil, nil
	}
	var idxs []*subjectIndex
	if subject == "" {
		idxs = s.snapshotSubjects()
	} else {
		s.mu.RLock()
		for _, k := range []string{subject, ""} {
			if idx := s.subjects[k]; idx != nil {
				idxs = append(idxs, idx)
			}
		}
		s.mu.RUnlock()
	}
	var all []CellSearchResult
	for _, idx := range idxs {
		idx.mu.RLock()
		all = append(all, scanEntriesByKind(embedding, idx.entries, 0, kindFilter)...)
		idx.mu.RUnlock()
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Score > all[j].Score })
	return all, nil
}
