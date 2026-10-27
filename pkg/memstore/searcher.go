package memstore

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/rs/zerolog"

	xindex "github.com/triageagent-dev/x-ray/v2/pkg/memstore/index"
)

// SearchConfig is what the Searcher asks of its host. Store is required; every
// other field may be nil, and a nil one behaves as the feature being absent.
type SearchConfig struct {
	Store *Store
	// Cells is the vector index behind semantic search, nil when disabled. It is
	// read on every call, so the host may set it late.
	Cells func() *xindex.CellStore
	// Visible reports whether a partition may see item; DocVisible does the same
	// for an index entry before it is hydrated. The host owns what a partition
	// is. A nil predicate means everything is visible.
	Visible    func(item Item, partition string) bool
	DocVisible func(doc xindex.IndexEntry, partition string) bool
	// Hydrate reads a cell the store does not hold (kinds the host keeps out of
	// the working set) from where it lives.
	Hydrate func(ctx context.Context, id string) (Item, bool)
	// Live returns matches from state the host keeps outside the store (live
	// events, say), for a full search. exclude lists ids already in the result.
	Live func(query, kind, partition string, exclude map[string]struct{}) []SearchResult
	// State reports whether the item is known to the host and still open, for
	// kinds that have a lifecycle outside the store: an open one is boosted over
	// a resolved one, an unknown one is left alone. Anchor reports an item the
	// host keeps refreshed as a summary, which outranks lesser matches for a
	// situational query. CollapseKey returns the near-duplicate identity of an
	// item, or "" when it is not eligible: items sharing a key collapse into one.
	State       func(item Item) (open, known bool)
	Anchor      func(item Item) bool
	CollapseKey func(item Item) string
	// LinkExpansion turns on the one-hop ThoughtLink expansion of results. It is
	// read on every call.
	LinkExpansion func() bool
	// Dirty fires after every search, for a stats flush.
	Dirty func()
	// ResolveStrict maps a partition name to its canonical name without falling
	// back to a default; ok is false for an unknown name. Used by Consolidate.
	ResolveStrict func(partition string) (string, bool)
	Logger        zerolog.Logger
	// Stats is where the search counters also go; nil counts only in process.
	Stats StatsSink

	// SemanticGap, when positive, drops semantic matches that score more than
	// this far below the best match the caller can see. SemanticCliff keeps only
	// the matches above the steepest drop in score instead. Both are applied
	// after visibility filtering and before fusion, so what is cut is measured
	// against a hit the caller can read, never against one it cannot.
	SemanticGap   float32
	SemanticCliff bool

	// AlwaysFuse turns the lexical short-circuit off: with a vector index the
	// search always fuses the semantic channel, even when the top lexical item
	// looks like a confident answer. A long question over long cells shares
	// tokens with almost anything, so a lexical answer can look confident
	// without being the answer; a short named-entity query does not need the
	// embedding round trip. Off by default.
	AlwaysFuse bool
	// NoShaping turns the rerank and the near-duplicate collapse off: results
	// come back in fused order. Off by default.
	NoShaping bool
	// SourceRelation, when set, replaces a hit that has an outbound link of this
	// relation by the item it links to (a fact by the episode it was drawn from),
	// provided that item is active and visible; the best-ranked hit to reach a
	// source claims its place, and the source's Via is that hit. A dangling or
	// hidden source keeps the hit itself. Fanout is how many times the limit each
	// channel pulls while this runs, since collapse shrinks the pool again; zero
	// means three, and an explicit value is taken as given.
	SourceRelation string
	Fanout         int
	// SubjectRouting sends the semantic channel through the index of the subject
	// asked for, merged with the global one, instead of the whole index. Off by
	// default, when subject only filters the lexical channel.
	SubjectRouting bool
}

// Searcher is hybrid search over a Store: lexical first, semantic fused in
// when the lexical answer is not confident, then reranked.
type Searcher struct {
	cfg   SearchConfig
	store *Store
	stats *SearchStats
}

// NewSearcher builds a searcher with its own search statistics.
func NewSearcher(cfg SearchConfig) *Searcher {
	return &Searcher{cfg: cfg, store: cfg.Store, stats: NewSearchStats(cfg.Stats)}
}

func (s *Searcher) cells() *xindex.CellStore {
	if s.cfg.Cells == nil {
		return nil
	}
	return s.cfg.Cells()
}

func (s *Searcher) visible(item Item, partition string) bool {
	if partition == PartitionAll || s.cfg.Visible == nil {
		return true
	}
	return s.cfg.Visible(item, partition)
}

func (s *Searcher) docVisible(doc xindex.IndexEntry, partition string) bool {
	if partition == PartitionAll || s.cfg.DocVisible == nil {
		return true
	}
	return s.cfg.DocVisible(doc, partition)
}

func (s *Searcher) hydrate(ctx context.Context, id string) (Item, bool) {
	if s.cfg.Hydrate == nil {
		return Item{}, false
	}
	return s.cfg.Hydrate(ctx, id)
}

func (s *Searcher) linkExpansion() bool { return s.cfg.LinkExpansion != nil && s.cfg.LinkExpansion() }

// With returns a searcher over the same store that shares this one's
// statistics and whose configuration is this one's after mut has changed it.
// The receiver is not modified, so a host swaps the copy in when it flips a
// switch at run time.
func (s *Searcher) With(mut func(*SearchConfig)) *Searcher {
	cfg := s.cfg
	mut(&cfg)
	return &Searcher{cfg: cfg, store: s.store, stats: s.stats}
}

// Stats is the search statistics, nil-safe through its methods.
func (s *Searcher) Stats() *SearchStats {
	if s == nil {
		return nil
	}
	return s.stats
}

// Store is the store searched.
func (s *Searcher) Store() *Store { return s.store }

func (s *Searcher) dirty() {
	if s.cfg.Dirty != nil {
		s.cfg.Dirty()
	}
}

func (s *Searcher) live(query, kind, partition string, exclude map[string]struct{}) []SearchResult {
	if s.cfg.Live == nil {
		return nil
	}
	return s.cfg.Live(query, kind, partition, exclude)
}

func (s *Searcher) filterVisible(items []Item, partition string) []Item {
	if s.cfg.Visible == nil {
		return items
	}
	out := make([]Item, 0, len(items))
	for _, it := range items {
		if s.visible(it, partition) {
			out = append(out, it)
		}
	}
	return out
}

// state reads the host's lifecycle state of an item.
func (s *Searcher) state(item Item) (open, known bool) {
	if s.cfg.State == nil {
		return false, false
	}
	return s.cfg.State(item)
}

// stateMultiplier boosts an open item over a resolved one; an item the host
// does not know gets 1.0.
func (s *Searcher) stateMultiplier(item Item) float64 {
	open, known := s.state(item)
	switch {
	case !known:
		return 1.0
	case open:
		return openMultiplier
	}
	return resolvedMultiplier
}

// isOpen reports whether item is known to the host and still open, used by the
// collapse tie-break only.
func (s *Searcher) isOpen(item Item) bool {
	open, known := s.state(item)
	return known && open
}

// BetterRoutingFact reports whether a should be trusted over b: the higher
// confidence, then the more trusted origin, then the newer, then the smaller ID,
// so two restatements never tie.
func BetterRoutingFact(a, b Item) bool {
	if ar, br := confidenceRank(a.Confidence), confidenceRank(b.Confidence); ar != br {
		return ar > br
	}
	if ao, bo := originRank(a.Origin), originRank(b.Origin); ao != bo {
		return ao > bo
	}
	if !a.UpdatedAt.Equal(&b.UpdatedAt) {
		return a.UpdatedAt.After(b.UpdatedAt.Time)
	}
	return a.ID < b.ID
}

// confidenceRank orders the confidence labels; an unknown label ranks 0.
func confidenceRank(c string) int {
	switch strings.ToLower(strings.TrimSpace(c)) {
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	}
	return 0
}

// SearchResult is one ranked hit. Score is the rerank score (base rank times the
// state, anchor, watch and recency multipliers) and orders results
// within one call only. CollapsedCount is >0 on the survivor of a near-duplicate
// collapse. Via is the ID of the direct hit whose link surfaced an expanded
// result, empty otherwise; an expanded result has Score 0.
type SearchResult struct {
	Item           Item
	Score          float64
	CollapsedCount int
	Via            string
}

// PartitionAll is the partition argument meaning no partition filter: matches
// from every partition. It is not "", which the host may resolve to a default
// partition. Use it only on read-only operator surfaces, never on a write,
// ingest or decision path.
const PartitionAll = "*"

// searchCallerCtxKey is the context key WithSearchCaller sets.
type searchCallerCtxKey struct{}

// WithSearchCaller stamps the calling subsystem's name onto ctx so hybridSearch
// can attribute the call's count and duration to it in the search stats. A call
// with no caller records as "internal".
func WithSearchCaller(ctx context.Context, caller string) context.Context {
	return context.WithValue(ctx, searchCallerCtxKey{}, caller)
}

// searchCallerFromCtx returns the caller stamped by WithSearchCaller,
// defaulting to "internal" when absent or empty.
func searchCallerFromCtx(ctx context.Context) string {
	if c, ok := ctx.Value(searchCallerCtxKey{}).(string); ok && c != "" {
		return c
	}
	return "internal"
}

// HybridSearch runs lexical search first and fuses in semantic results by
// Reciprocal Rank Fusion when the top lexical hit is not a confident
// (full-coverage) match and a cell store is available. A confident lexical hit
// returns without an embedding call, so cells outside the working set
// are only reached on the fused path; callers that need full-store coverage use
// HybridSearchFull.
//
// partition is the caller's resolved partition (empty resolves to the default): results
// are limited to items tagged for it or untagged. Cross-partition results are never
// returned. PartitionAll disables the filter and is for read-only operator surfaces
// only.
func (s *Searcher) HybridSearch(ctx context.Context, query, kind, subject, partition string, limit int) []SearchResult {
	return s.hybridSearch(ctx, query, kind, subject, partition, limit, false)
}

// HybridSearchFull is HybridSearch without the confident-lexical fast path: it
// always consults the cell store, which holds the cells the working
// set excludes. A confident lexical match is only confident within the working
// set. It also adds the host's live matches and promotes an exact id, and costs
// one extra embedding call per search.
func (s *Searcher) HybridSearchFull(ctx context.Context, query, kind, subject, partition string, limit int) []SearchResult {
	return s.hybridSearch(ctx, query, kind, subject, partition, limit, true)
}

// hybridSearch implements HybridSearch and HybridSearchFull; alwaysConsultStore
// skips the confident-lexical fast path.
func (s *Searcher) hybridSearch(ctx context.Context, query, kind, subject, partition string, limit int, alwaysConsultStore bool) []SearchResult {
	if limit <= 0 {
		limit = 10
	}
	trimmedQuery := strings.TrimSpace(query)
	if trimmedQuery == "" || s.store == nil {
		return nil
	}

	keep := func(it Item) bool { return s.visible(it, partition) }

	start := time.Now()
	mode := "lexical"
	caller := searchCallerFromCtx(ctx)
	defer func() {
		if s.stats != nil {
			d := time.Since(start)
			s.stats.recordSearch(mode, d)
			s.stats.recordCaller(caller, d)
		}
		s.dirty()
	}()

	// rerank times Rerank on every return path so the rerank phase count covers
	// every search.
	rerank := func(items []Item) []SearchResult {
		rStart := time.Now()
		out := s.Rerank(items, limit)
		if s.stats != nil {
			s.stats.recordRerankPhase(time.Since(rStart))
		}
		return out
	}

	// finish runs every return path through optional ThoughtLink expansion of the
	// reranked set, then exact-ID promotion (and live matches for a full search).
	// Over-fetch regardless of partition count: Rerank needs surplus candidates to
	// collapse near-duplicates and to promote a lower-ranked open item or anchor.
	fetchLimit := poolSize(s.cfg, limit)

	finish := func(items []Item) []SearchResult {
		items, via := s.collapseToSources(ctx, items, keep, fetchLimit)
		ranked := rerank(items)
		for i := range ranked {
			if v, ok := via[ranked[i].Item.ID]; ok {
				ranked[i].Via = v
			}
		}
		expanded := s.expandViaLinks(ctx, ranked, keep, limit)
		return s.finalizeResults(ctx, trimmedQuery, kind, partition, keep, expanded, alwaysConsultStore, limit)
	}

	lexStart := time.Now()
	lexItems, lexConfident, trigramHit := s.store.KeywordSearchScoredWithTrigramHit(query, kind, subject, fetchLimit)
	if s.stats != nil {
		s.stats.recordLexicalPhase(time.Since(lexStart))
	}
	lexFiltered := make([]Item, 0, len(lexItems))
	for _, it := range lexItems {
		if keep(it) {
			lexFiltered = append(lexFiltered, it)
		}
	}
	// Not truncated to limit here; Rerank truncates after collapse.

	if s.stats != nil {
		s.stats.recordKeywordHit()
		if trigramHit {
			s.stats.recordKWTrigramHit()
		}
	}

	// Confident lexical fast path: no embedding call. alwaysConsultStore disables
	// it.
	cellStore := s.cells()
	if cellStore == nil || (lexConfident && !alwaysConsultStore && !s.cfg.AlwaysFuse) {
		if s.stats != nil {
			s.stats.recordKWVoiceHit()
		}
		return finish(lexFiltered)
	}

	// Low-confidence lexical: fuse with semantic results.
	mode = "fused"
	if s.stats != nil {
		s.stats.recordKWVoiceMiss()
		s.stats.recordSemanticCall()
	}
	// SearchTimed so embed and index scan are timed separately; Index is only
	// meaningful on success.
	var semResults []xindex.CellSearchResult
	var timing xindex.SearchTiming
	var err error
	if s.cfg.SubjectRouting && subject != "" {
		tctx := xindex.WithTimingSink(ctx, func(t xindex.SearchTiming) { timing = t })
		semResults, err = cellStore.SearchSubject(tctx, query, subject, fetchLimit, kind)
	} else {
		semResults, timing, err = cellStore.SearchTimed(ctx, query, fetchLimit, kind)
	}
	if s.stats != nil {
		s.stats.recordEmbedPhase(timing.Embed)
	}
	if err != nil {
		if s.stats != nil {
			s.stats.recordSemanticFallback()
		}
		return finish(lexFiltered)
	}
	if s.stats != nil {
		s.stats.recordIndexPhase(timing.Index)
	}

	hydrateStart := time.Now()
	semItems := make([]Item, 0, len(semResults))
	semKept := make([]xindex.CellSearchResult, 0, len(semResults))
	for _, r := range semResults {
		// Working set first, then the cell store for cells it does not
		// carry.
		it, ok := s.store.Get(r.Document.ID)
		if !ok {
			it, ok = s.hydrate(ctx, r.Document.ID)
		}
		if !ok || !it.IsActive() || !keep(it) {
			continue
		}
		if kind == "" && s.store.cfg.Vocabulary.IsNonSearchable(it.Kind) {
			continue
		}
		// Defense in depth: a private item is never embedded, but hydration from the
		// cell store bypasses that gate.
		if IsPrivateItem(it) {
			continue
		}
		// Defense in depth: system_configuration holds live auth material and is never
		// embedded, but hydration from the cell store bypasses that gate.
		if it.Kind == "system_configuration" {
			continue
		}
		semItems = append(semItems, it)
		semKept = append(semKept, r)
	}
	if s.cfg.SemanticCliff {
		semItems = semItems[:len(xindex.CutAtLargestGap(semKept))]
	} else if s.cfg.SemanticGap > 0 {
		semItems = semItems[:len(xindex.CutAtGap(semKept, s.cfg.SemanticGap))]
	}
	if s.stats != nil {
		s.stats.recordHydratePhase(time.Since(hydrateStart))
	}
	if len(semItems) == 0 {
		// Starvation fallback: the top-fetchLimit semantic window can be filled entirely
		// by cells the loop above drops (other partitions, inactive, non-searchable kinds),
		// leaving nothing although the caller can see matching cells. When both channels
		// are empty and the embed succeeded, rank the whole store instead.
		if len(lexFiltered) == 0 {
			if fallback := s.exhaustiveSemanticFallback(ctx, query, kind, partition, keep, fetchLimit); len(fallback) > 0 {
				if s.stats != nil {
					s.stats.recordExhaustiveFallback()
				}
				return finish(fuseRRF(lexFiltered, fallback, s.exactMatches(query, kind, subject, keep), fetchLimit, lexConfident))
			}
		}
		return finish(lexFiltered)
	}

	// Fuse at fetchLimit so Rerank keeps surplus candidates; it truncates to limit
	// after collapse.
	fused := fuseRRF(lexFiltered, semItems, s.exactMatches(query, kind, subject, keep), fetchLimit, lexConfident)
	return finish(fused)
}

// defaultFanout is how many times the limit each channel pulls when source
// collapse will shrink the pool again.
const defaultFanout = 3

// poolSize is how many candidates each channel fetches for limit: with source
// collapse on, limit times the fanout (an explicit fanout is taken as given);
// otherwise, unless shaping is off, at least limit times four capped at
// maxRerankFetchLimit so the rerank has surplus candidates to promote and to
// collapse.
func poolSize(cfg SearchConfig, limit int) int {
	want := limit
	if cfg.SourceRelation != "" {
		f := cfg.Fanout
		if f == 0 {
			f = defaultFanout
		}
		if f < 1 {
			f = 1
		}
		want = limit * f
		if cfg.Fanout != 0 {
			return want
		}
	}
	if !cfg.NoShaping {
		shaped := limit * 4
		if shaped > maxRerankFetchLimit {
			shaped = maxRerankFetchLimit
		}
		if shaped > want {
			want = shaped
		}
	}
	return want
}

// collapseToSources implements SourceRelation: walking items best first, an item
// with an outbound link of that relation is replaced by its target when the
// target is active and visible; the first item to reach a target claims its
// position and later items collapsing to the same target are dropped. Items
// without such a link pass through, deduplicated against targets already
// claimed. It returns at most limit items and, for each substituted source, the
// id of the item that reached it. A no-op when no relation is configured.
func (s *Searcher) collapseToSources(ctx context.Context, items []Item, keep func(Item) bool, limit int) ([]Item, map[string]string) {
	rel := s.cfg.SourceRelation
	if rel == "" || len(items) == 0 {
		return items, nil
	}
	seen := make(map[string]struct{}, len(items))
	via := make(map[string]string)
	out := make([]Item, 0, len(items))
	for _, it := range items {
		if len(out) >= limit {
			break
		}
		target := ""
		for _, l := range it.Links {
			if l.Relation == rel && l.ToID != "" && l.ToID != it.ID {
				target = l.ToID
				break
			}
		}
		if target != "" {
			if _, dup := seen[target]; dup {
				continue
			}
			src, ok := s.store.Get(target)
			if !ok {
				src, ok = s.hydrate(ctx, target)
			}
			if ok && src.IsActive() && !IsPrivateItem(src) && keep(src) {
				seen[target] = struct{}{}
				via[target] = it.ID
				out = append(out, src)
				continue
			}
			// A dangling or hidden source keeps the fact itself.
		}
		if _, dup := seen[it.ID]; dup {
			continue
		}
		seen[it.ID] = struct{}{}
		out = append(out, it)
	}
	return out, via
}

// exactMatches is the third fusion channel: the items whose title or service
// name is the whole query, limited to what the caller may see.
func (s *Searcher) exactMatches(query, kind, subject string, keep func(Item) bool) []Item {
	var out []Item
	for _, it := range s.store.ExactMatches(query, kind, subject) {
		if keep(it) {
			out = append(out, it)
		}
	}
	return out
}

// exhaustiveFallbackScanCap bounds how many ranked candidates
// exhaustiveSemanticFallback hydrates before giving up. It is a last-resort path
// and a real answer sits near the top of the ranking.
const exhaustiveFallbackScanCap = 500

// exhaustiveSemanticFallback is hybridSearch's last resort when the lexical and
// semantic channels are both empty and the embed succeeded. It ranks the full
// store by exact cosine (no HNSW truncation) and applies the same hydration and
// guards as the semantic loop, stopping at fetchLimit survivors or
// exhaustiveFallbackScanCap candidates. It returns nil when there is no embedder,
// the embed fails or nothing survives.
func (s *Searcher) exhaustiveSemanticFallback(ctx context.Context, query, kind, partition string, keep func(Item) bool, fetchLimit int) []Item {
	cellStore := s.cells()
	if cellStore == nil {
		return nil
	}

	// Document-level pre-filter: skips wrong-partition candidates before hydration. A
	// cost optimization only; keep re-checks every survivor.
	docKeep := func(doc xindex.IndexEntry) bool { return s.docVisible(doc, partition) }

	results, timing, err := cellStore.SearchExhaustiveTimed(ctx, query, kind, docKeep)
	if s.stats != nil {
		// timing.Embed is a cache hit here (shared with the SearchTimed call for the
		// same query); it is recorded anyway so the phase stats count every embed and
		// scan.
		s.stats.recordEmbedPhase(timing.Embed)
		s.stats.recordIndexPhase(timing.Index)
	}
	if err != nil || len(results) == 0 {
		return nil
	}

	out := make([]Item, 0, fetchLimit)
	scanned := 0
	for _, r := range results {
		if len(out) >= fetchLimit || scanned >= exhaustiveFallbackScanCap {
			break
		}
		scanned++
		it, ok := s.store.Get(r.Document.ID)
		if !ok {
			it, ok = s.hydrate(ctx, r.Document.ID)
		}
		if !ok || !it.IsActive() || !keep(it) {
			continue
		}
		if kind == "" && s.store.cfg.Vocabulary.IsNonSearchable(it.Kind) {
			continue
		}
		// Defense in depth, as in the semantic loop above: a private item is never
		// embedded, but hydration from the cell store bypasses that gate.
		if IsPrivateItem(it) {
			continue
		}
		// Defense in depth, as above: system_configuration is never embedded.
		if it.Kind == "system_configuration" {
			continue
		}
		out = append(out, it)
	}
	return out
}

// expandViaLinks optionally expands a reranked result set one hop through
// ThoughtLinks, appending neighbors after the direct hits. It returns base
// unchanged unless the LinkExpansion config returns true.
//
// Only outbound links are followed (a reverse lookup would scan the whole store).
// Neighbors are ordered by the rank of the result that links to them, the
// best-ranked linker wins the dedup, at most limit/2 are added and the total
// stays within limit. Each added item's Via is the linking result's ID. Targets
// already present, inactive, or rejected by keep are skipped.
//
// keep is the partition predicate already applied to the direct hits. A ThoughtLink
// has no partition awareness, so without it a search could follow a link into
// another partition's cell.
func (s *Searcher) expandViaLinks(ctx context.Context, base []SearchResult, keep func(Item) bool, limit int) []SearchResult {
	if !s.linkExpansion() || len(base) == 0 {
		return base
	}
	maxExtra := limit / 2
	room := limit - len(base)
	if maxExtra > room {
		maxExtra = room
	}
	if maxExtra <= 0 {
		return base
	}

	seen := make(map[string]struct{}, len(base))
	for _, r := range base {
		seen[r.Item.ID] = struct{}{}
	}

	type linkTarget struct {
		toID     string
		fromRank int
		weight   float32
	}
	var targets []linkTarget
	for rank, r := range base {
		for _, link := range r.Item.Links {
			if link.ToID == "" || link.ToID == r.Item.ID {
				continue
			}
			if _, ok := seen[link.ToID]; ok {
				continue // already a direct hit, or already queued from a better-ranked linker
			}
			seen[link.ToID] = struct{}{}
			targets = append(targets, linkTarget{toID: link.ToID, fromRank: rank, weight: link.Weight})
		}
	}
	if len(targets) == 0 {
		return base
	}
	// The best-ranked linker comes first; among one linker's targets the strongest
	// link does, so a cap on the number added keeps the edges that matter. A
	// link's own order in its slice carries no meaning.
	sort.SliceStable(targets, func(i, j int) bool {
		if targets[i].fromRank != targets[j].fromRank {
			return targets[i].fromRank < targets[j].fromRank
		}
		return targets[i].weight > targets[j].weight
	})

	out := make([]SearchResult, len(base), len(base)+maxExtra)
	copy(out, base)
	added := 0
	for _, t := range targets {
		if added >= maxExtra {
			break
		}
		// Same hydration as the semantic branch: working set first, then the cell
		// store.
		it, ok := s.store.Get(t.toID)
		if !ok {
			it, ok = s.hydrate(ctx, t.toID)
		}
		if !ok || !it.IsActive() || !keep(it) {
			continue
		}
		out = append(out, SearchResult{Item: it, Via: base[t.fromRank].Item.ID})
		added++
	}
	return out
}

// ExactIDMatchScore is the Score of an exact cell ID hit. It lies
// outside Rerank's scale (base score at most 1.0, largest multiplier 1.7) so it
// is never mistaken for a relevance score. Order is the slice position, not this
// value.
const ExactIDMatchScore = 100.0

// finalizeResults runs on every return path of hybridSearch over the
// partition-filtered, reranked cellResults. It promotes a cell whose ID equals
// trimmedQuery (case-insensitive) to the top whatever its token coverage, since a
// pasted UUID rarely clears the coverage gate. For a full search
// (alwaysConsultStore) it also promotes a matching live match and appends the
// other live matches, so state never stored as cells stays findable. A plain
// HybridSearch never pays for that scan.
func (s *Searcher) finalizeResults(ctx context.Context, trimmedQuery, kind, partition string, keep func(Item) bool, cellResults []SearchResult, alwaysConsultStore bool, limit int) []SearchResult {
	exactItem, hasExact := s.exactIDMatch(ctx, trimmedQuery, kind, keep, alwaysConsultStore)

	var hubResults []SearchResult
	if alwaysConsultStore {
		exclude := make(map[string]struct{}, len(cellResults)+1)
		for _, r := range cellResults {
			exclude[r.Item.ID] = struct{}{}
		}
		if hasExact {
			exclude[exactItem.ID] = struct{}{}
		}
		hubResults = s.live(trimmedQuery, kind, partition, exclude)
		if !hasExact && len(hubResults) > 0 && strings.EqualFold(hubResults[0].Item.ID, trimmedQuery) {
			exactItem, hasExact = hubResults[0].Item, true
			hubResults = hubResults[1:]
		}
	}

	if !hasExact && len(hubResults) == 0 {
		return cellResults
	}

	out := make([]SearchResult, 0, len(cellResults)+len(hubResults)+1)
	if hasExact {
		out = append(out, SearchResult{Item: exactItem, Score: ExactIDMatchScore})
	}
	for _, r := range cellResults {
		if hasExact && r.Item.ID == exactItem.ID {
			continue
		}
		out = append(out, r)
	}
	out = append(out, hubResults...)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// exactIDMatch looks up the cell whose ID equals trimmedQuery. It is cheap
// enough for a hot path: a query containing whitespace returns at
// once; the working set is an O(1) case-exact lookup, with a case-insensitive
// scan only when foldScan is set; other cells are hydrated by the exact ID
// (case-sensitive, to avoid listing the namespace).
//
// kind and partition visibility (keep) apply as for every result: a failing match is
// reported as not found. A private item is never matched, since this lookup has
// no caller identity to compare an owner against.
func (s *Searcher) exactIDMatch(ctx context.Context, trimmedQuery, kind string, keep func(Item) bool, foldScan bool) (Item, bool) {
	if trimmedQuery == "" || strings.ContainsAny(trimmedQuery, " \t\n") {
		return Item{}, false
	}
	if s.store != nil {
		if it, ok := s.store.GetFold(trimmedQuery, foldScan); ok {
			if !it.IsActive() || IsPrivateItem(it) || (kind != "" && it.Kind != kind) || !keep(it) {
				return Item{}, false
			}
			return it, true
		}
	}
	it, ok := s.hydrate(ctx, trimmedQuery)
	if !ok || !it.IsActive() || IsPrivateItem(it) || (kind != "" && it.Kind != kind) || !keep(it) {
		return Item{}, false
	}
	return it, true
}

// Rerank: state-aware re-ranking and near-duplicate collapse over hybridSearch's
// partition-filtered candidates. It only reads and reorders and never writes a cell.
// It runs after the partition filter, so a boost cannot resurrect a dropped item.

// maxRerankFetchLimit caps hybridSearch's over-fetch (limit*4) so a large caller
// limit cannot turn one search into an unbounded scan.
const maxRerankFetchLimit = 60

// The rerank score of a candidate is its base-rank score times six always-on
// multipliers: state, anchor, watch item, provenance, confidence and recency. Relevance still
// dominates for a clearly relevant top hit, but a resolved near-duplicate on a
// strong match loses to a boosted state note a few ranks down.
const (
	// rerankRankDecay sets how fast RerankBaseScore falls with input rank.
	// Hyperbolic: an item several ranks back can still overtake rank 0 once boosted,
	// and two unboosted items keep their order.
	rerankRankDecay = 0.08

	// openMultiplier boosts an item the host reports as open, so it can overtake a
	// resolved item several ranks ahead.
	openMultiplier = 1.6

	// resolvedMultiplier mildly demotes an item the host reports as resolved; it
	// stays findable but should not be the first answer.
	resolvedMultiplier = 0.85

	// anchorMultiplier boosts an item the host keeps refreshed as a summary: the
	// right first answer to a situational query. 1.4 lets an anchor at rank 8
	// (base score 0.61) overtake a rank-0 resolved item (0.85); the minimum is
	// about 1.394, and 1.35 loses.
	anchorMultiplier = 1.4

	// watchItemActiveMultiplier boosts an active, unexpired watch_item, the kind
	// that exists to flag something to be aware of. It is above openMultiplier
	// so a watch item at rank 11 (base score 0.53) can clear a cutoff of 0.86 (minimum
	// about 1.654), yet it stays below a well-ranked status note (1.4 at rank 0).
	watchItemActiveMultiplier = 1.7

	// provenanceBoost and the confidence bounds below keep who wrote an item and
	// how sure they were from being ignored by ranking: a person's or a checked
	// answer is worth a little more than a model's guess at the same rank, a
	// high-confidence item a little more than a medium one, a low-confidence one
	// a little less. They are bounds, not tiers: relevance still decides.
	provenanceBoost = 1.10
	confidenceHigh  = 1.05
	confidenceLow   = 0.92

	// recencyHalfLife is the age at which recencyMultiplier sits exactly
	// halfway between 1.0 (fresh) and recencyFloor (very old).
	recencyHalfLife = 7 * 24 * time.Hour

	// recencyFloor bounds how much age alone can demote an item, to about 15% versus
	// a fresh one. Recency should reorder ties, not overwhelm relevance.
	recencyFloor = 0.85
)

// effectiveTime is the time an item ages from: for an event with a parseable
// EventDate, when the event happened, else UpdatedAt. A cell restored or seeded
// in bulk has the time it was ingested in UpdatedAt, however long ago the event
// was.
func effectiveTime(it Item) time.Time {
	if it.Kind == KindEvent && it.EventDate != "" {
		if t, ok := ParseEventDate(it.EventDate); ok {
			return t
		}
	}
	return it.UpdatedAt.Time
}

// provenanceMultiplier is provenanceBoost for an item a person wrote or a check
// verified, else 1.0.
func provenanceMultiplier(origin string) float64 {
	if origin == OriginHuman || origin == OriginVerified {
		return provenanceBoost
	}
	return 1.0
}

// confidenceMultiplier is confidenceHigh for a high-confidence item,
// confidenceLow for a low-confidence one, else 1.0.
func confidenceMultiplier(confidence string) float64 {
	switch strings.ToLower(strings.TrimSpace(confidence)) {
	case "high":
		return confidenceHigh
	case "low":
		return confidenceLow
	}
	return 1.0
}

// RerankBaseScore converts a 0-based input rank into the base score the
// multipliers scale: 1/(1+rank*rerankRankDecay).
func RerankBaseScore(rank int) float64 {
	return 1.0 / (1.0 + float64(rank)*rerankRankDecay)
}

// recencyMultiplier is 1.0 for a fresh cell, decaying toward recencyFloor with a
// half-life of recencyHalfLife. A zero or future UpdatedAt is neutral (1.0).
func recencyMultiplier(updatedAt time.Time, now time.Time) float64 {
	if updatedAt.IsZero() {
		return 1.0
	}
	age := now.Sub(updatedAt)
	if age <= 0 {
		return 1.0
	}
	decay := math.Pow(0.5, age.Hours()/recencyHalfLife.Hours())
	return recencyFloor + (1.0-recencyFloor)*decay
}

// anchorMultiplierFor returns anchorMultiplier for an item the host reports as
// an anchor, else 1.0.
func (s *Searcher) anchorMultiplierFor(item Item) float64 {
	if s.cfg.Anchor != nil && s.cfg.Anchor(item) {
		return anchorMultiplier
	}
	return 1.0
}

// isActiveWatchItem reports whether item is an active, unexpired watch_item.
func isActiveWatchItem(item Item) bool {
	return item.Kind == "watch_item" && item.IsActive()
}

// watchItemActiveMultiplierFor is the watch_item analogue of
// noteAnchorMultiplierFor above.
func watchItemActiveMultiplierFor(item Item) float64 {
	if isActiveWatchItem(item) {
		return watchItemActiveMultiplier
	}
	return 1.0
}

// collapseKey returns the near-duplicate identity key the host gives item, or ""
// when it is not eligible (two ineligible items do not collapse together).
func (s *Searcher) collapseKey(item Item) string {
	if s.cfg.CollapseKey == nil {
		return ""
	}
	return s.cfg.CollapseKey(item)
}

// rerankCandidate pairs a scored result with its collapse-group key, possibly
// empty.
type rerankCandidate struct {
	result SearchResult
	key    string
}

// knowledgeCollapseKinds are the kinds eligible for knowledge near-duplicate
// collapse. "mindmap" is excluded: it is one cell
// per partition and service under a stable ID.
var knowledgeCollapseKinds = map[string]bool{
	KindStableFact:         true,
	KindOperationalPattern: true,
	KindWatchItem:          true,
	KindHypothesis:         true,
}

// collapseNearDuplicateKnowledge clusters candidates of knowledgeCollapseKinds
// by nearDuplicateMemory (ClusterNearDuplicates) and keeps the best member of
// each cluster per collapseBetter; other kinds pass through. Clustering runs in
// descending score order so a cluster's founder is its anchor. It is O(n^2) in
// the eligible candidates, which are bounded by the fetch window.
func (s *Searcher) collapseNearDuplicateKnowledge(in []SearchResult) []SearchResult {
	var eligible, rest []SearchResult
	for _, r := range in {
		if knowledgeCollapseKinds[r.Item.Kind] {
			eligible = append(eligible, r)
		} else {
			rest = append(rest, r)
		}
	}
	if len(eligible) < 2 {
		return in
	}

	sort.SliceStable(eligible, func(i, j int) bool {
		if eligible[i].Score != eligible[j].Score {
			return eligible[i].Score > eligible[j].Score
		}
		return eligible[i].Item.ID < eligible[j].Item.ID
	})

	byID := make(map[string]SearchResult, len(eligible))
	items := make([]Item, len(eligible))
	for i, r := range eligible {
		byID[r.Item.ID] = r
		items[i] = r.Item
	}

	out := make([]SearchResult, 0, len(rest)+len(eligible))
	out = append(out, rest...)
	for _, cluster := range ClusterNearDuplicates(items) {
		best := rerankCandidate{result: byID[cluster[0].ID]}
		for _, it := range cluster[1:] {
			cand := rerankCandidate{result: byID[it.ID]}
			if s.collapseBetter(cand, best) {
				best = cand
			}
		}
		survivor := best.result
		if n := len(cluster) - 1; n > 0 {
			survivor.CollapsedCount += n
		}
		out = append(out, survivor)
	}
	return out
}

// collapseBetter reports whether a should replace b as a group's survivor:
// higher score, then open over resolved, then higher confidence, then the more
// trusted origin, then newer, then ID. Confidence and origin come before
// recency so a fresh low-confidence restatement does not replace an older
// high-confidence fact.
func (s *Searcher) collapseBetter(a, b rerankCandidate) bool {
	if a.result.Score != b.result.Score {
		return a.result.Score > b.result.Score
	}
	aOpen, bOpen := s.isOpen(a.result.Item), s.isOpen(b.result.Item)
	if aOpen != bOpen {
		return aOpen
	}
	if ac, bc := confidenceRank(a.result.Item.Confidence), confidenceRank(b.result.Item.Confidence); ac != bc {
		return ac > bc
	}
	if ao, bo := originRank(a.result.Item.Origin), originRank(b.result.Item.Origin); ao != bo {
		return ao > bo
	}
	au, bu := a.result.Item.UpdatedAt, b.result.Item.UpdatedAt
	if !au.Equal(&bu) {
		return au.After(bu.Time)
	}
	return a.result.Item.ID < b.result.Item.ID
}

// Rerank scores items (partition-filtered candidates in relevance order, index 0
// best), collapses near-duplicates and truncates to limit. Two collapse passes
// run: key-based (the host's CollapseKey), then collapseNearDuplicateKnowledge. The
// output is deterministic: stable sorts with an ID tie-break.
func (s *Searcher) Rerank(items []Item, limit int) []SearchResult {
	if len(items) == 0 {
		return nil
	}
	if s.cfg.NoShaping {
		out := make([]SearchResult, 0, len(items))
		for i, it := range items {
			out = append(out, SearchResult{Item: it, Score: RerankBaseScore(i)})
		}
		if limit > 0 && len(out) > limit {
			out = out[:limit]
		}
		return out
	}
	now := time.Now()

	scored := make([]rerankCandidate, len(items))
	for i, it := range items {
		score := RerankBaseScore(i) *
			s.stateMultiplier(it) *
			s.anchorMultiplierFor(it) *
			watchItemActiveMultiplierFor(it) *
			provenanceMultiplier(it.Origin) *
			confidenceMultiplier(it.Confidence) *
			recencyMultiplier(effectiveTime(it), now)
		scored[i] = rerankCandidate{
			result: SearchResult{Item: it, Score: score},
			key:    s.collapseKey(it),
		}
	}

	// Group by collapse key (never ""), keep the best per group; the survivor's
	// CollapsedCount records how many were folded in.
	groups := make(map[string][]int)
	for i, c := range scored {
		if c.key == "" {
			continue
		}
		groups[c.key] = append(groups[c.key], i)
	}

	dropped := make(map[int]bool, len(scored))
	collapsedCount := make(map[int]int, len(groups))
	for _, idxs := range groups {
		if len(idxs) < 2 {
			continue
		}
		best := idxs[0]
		for _, j := range idxs[1:] {
			if s.collapseBetter(scored[j], scored[best]) {
				best = j
			}
		}
		for _, j := range idxs {
			if j != best {
				dropped[j] = true
			}
		}
		collapsedCount[best] = len(idxs) - 1
	}

	out := make([]SearchResult, 0, len(scored)-len(dropped))
	for i, c := range scored {
		if dropped[i] {
			continue
		}
		r := c.result
		if n := collapsedCount[i]; n > 0 {
			r.CollapsedCount = n
		}
		out = append(out, r)
	}

	out = s.collapseNearDuplicateKnowledge(out)

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Item.ID < out[j].Item.ID
	})

	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// ConsolidationGroup is one near-duplicate cluster found by a
// ConsolidateMemory sweep: Keep is the ID a reader should trust going
// forward, Superseded lists every other member's ID (parallel to Scores,
// each member's nearDuplicateMemory score against Keep).
type ConsolidationGroup struct {
	Kind       string
	Keep       string
	KeepTitle  string
	Superseded []string
	Scores     []float64
}

// ConsolidationReport is the outcome of one Consolidate sweep. Error is set,
// with every other field zero, when the sweep could not run (empty or unresolved
// partition); a zero Scanned or Groups then does not mean no duplicates.
type ConsolidationReport struct {
	Partition  string
	Scanned    int
	Groups     []ConsolidationGroup
	Superseded int
	Applied    bool
	Error      string `json:"error,omitempty"`
}

// consolidationKinds are the knowledge kinds Consolidate sweeps: the kinds a
// knowledge pass can restate under a new ID. Other kinds are excluded; mindmap is
// keyed by a stable ID, one cell per partition and service.
var consolidationKinds = []string{KindStableFact, KindOperationalPattern, KindWatchItem, KindHypothesis}

// Consolidate groups active stable_fact, operational_pattern, watch_item and
// hypothesis cells visible to partition into near-duplicate clusters and, when apply
// is true, supersedes every member but each cluster's keeper (highest Confidence,
// newest on a tie, per BetterRoutingFact). Superseding removes the members' cells
// from the store; the keeper's Supersedes field holds their IDs. apply=false
// returns the same groups without writing.
//
// partition is required and resolved through ResolveStrict, never defaulting: an
// empty or unknown name fails closed with Error set. Cells are filtered by
// Visible, so in a multi-partition instance an untagged cell is excluded from every
// sweep.
func (s *Searcher) Consolidate(partition string, apply bool) ConsolidationReport {
	partition = strings.TrimSpace(partition)
	if partition == "" {
		return ConsolidationReport{Error: "partition is required"}
	}
	resolved, ok := s.cfg.ResolveStrict(partition)
	if !ok {
		return ConsolidationReport{Partition: partition, Error: "unknown partition " + partition}
	}

	report := ConsolidationReport{Partition: resolved, Applied: apply}

	for _, kind := range consolidationKinds {
		items := s.store.Filtered("active", kind, "", "", "", 0)
		items = s.filterVisible(items, resolved)
		report.Scanned += len(items)
		if len(items) < 2 {
			continue
		}

		// Newest first: the first member of each cluster is its comparison anchor, and
		// the near-duplicate threshold was tuned with this order.
		sort.SliceStable(items, func(i, j int) bool {
			return items[j].UpdatedAt.Before(&items[i].UpdatedAt)
		})

		for _, cluster := range ClusterNearDuplicates(items) {
			if len(cluster) < 2 {
				continue
			}
			keepIdx := 0
			for i := 1; i < len(cluster); i++ {
				if BetterRoutingFact(cluster[i], cluster[keepIdx]) {
					keepIdx = i
				}
			}
			keep := cluster[keepIdx]

			group := ConsolidationGroup{
				Kind:      kind,
				Keep:      keep.ID,
				KeepTitle: keep.Title,
			}
			for i, item := range cluster {
				if i == keepIdx {
					continue
				}
				score, _ := NearDuplicateMemory(keep, item)
				group.Superseded = append(group.Superseded, item.ID)
				group.Scores = append(group.Scores, score)
			}
			report.Groups = append(report.Groups, group)
			report.Superseded += len(group.Superseded)

			if apply {
				for _, id := range group.Superseded {
					s.store.MarkSuperseded(id, keep.ID)
				}
				s.cfg.Logger.Info().
					Str("partition", resolved).
					Str("kind", kind).
					Str("keep", keep.ID).
					Int("superseded", len(group.Superseded)).
					Msg("memory consolidation superseded near-duplicate cells")
			}
		}
	}

	return report
}
