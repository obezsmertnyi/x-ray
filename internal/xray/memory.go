package xray

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/rs/zerolog"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mcell "github.com/triageagent-dev/x-ray/v2/api/v1"
	"github.com/triageagent-dev/x-ray/v2/internal/observability"
	"github.com/triageagent-dev/x-ray/v2/internal/vectorstore"
)

// Upsert size caps — bound an in-process item so one oversized write cannot
// exhaust pod memory. The MCP server bypasses the HTTP body limit, so these
// are enforced at the store boundary too.
const (
	maxTitleChars       = 1 << 10 // 1 KiB
	maxContentChars     = 1 << 18 // 256 KiB
	maxImageBase64Chars = 1 << 21 // 2 MiB of base64 (~1.5 MiB raster)
)

// SearchMetrics tracks search operation counters since process start.
type SearchMetrics struct {
	TotalSearches     int64 `json:"totalSearches"`
	KeywordHits       int64 `json:"keywordHits"`
	KWVoiceHits       int64 `json:"kwVoiceHits"`
	KWVoiceMisses     int64 `json:"kwVoiceMisses"`
	KWTrigramHits     int64 `json:"kwTrigramHits"`
	SemanticCalls     int64 `json:"semanticCalls"`
	SemanticFallbacks int64 `json:"semanticFallbacks"`
}

type searchCounters struct {
	total         atomic.Int64
	kwHits        atomic.Int64
	kwVoiceHits   atomic.Int64
	kwVoiceMisses atomic.Int64
	kwTrigramHits atomic.Int64
	semCalls      atomic.Int64
	semFallbacks  atomic.Int64
	voiceTotal    atomic.Int64
	voiceActive   atomic.Int64
}

// MemoryItem is a single awareness memory cell stored in the server.
type MemoryItem struct {
	ID         string   `json:"id"`
	Kind       string   `json:"kind"`
	Scope      string   `json:"scope"`
	Subject    string   `json:"subject,omitempty"`
	Title      string   `json:"title"`
	Content    string   `json:"content"`
	Confidence string   `json:"confidence,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	Status     string   `json:"status"`
	// EventDate is only set for kind=event items. RFC3339.
	EventDate string `json:"eventDate,omitempty"`
	// Data holds arbitrary named blobs, e.g. Data["image"] for thumbnails.
	Data map[string]mcell.DataEntry `json:"data,omitempty"`
	// ImageURL is set server-side to /v1/memory/{id}/image when Data["image"] is present.
	ImageURL string `json:"imageUrl,omitempty"`
	// StaleAfter is the item's expiry deadline (decay by default). nil means no
	// deadline — the item never auto-expires via the retention sweep
	// (StartRetentionSweep). Stamped from a compiled per-kind default on CREATE
	// only (see defaultTTLByKind); an update never restamps or clears it
	// implicitly — renewal is an explicit StaleAfter set by the writer. Mirrors
	// MemoryCellSpec.StaleAfter (cellToItem / CellSyncer.Persist thread it both
	// directions).
	StaleAfter *time.Time `json:"staleAfter,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
	// Links mirrors MemoryCellSpec.Links (ThoughtLink — an existing CR field,
	// not new schema) for CR-backed items, so Search's optional 1-hop
	// expansion (SetLinkExpansion, docs/todo/graphiti-comparison-refinement.md
	// item #4) can walk it without a second read. Populated by cellToItem;
	// ephemeral (s.items-only) items carry no Links since nothing currently
	// writes them outside the CR path.
	Links []mcell.ThoughtLink `json:"links,omitempty"`
	// Origin is the cell's provenance (ADR-053): observed, derived, human or
	// verified. A cell stored without one reads as derived.
	Origin string `json:"origin,omitempty"`
	// EvidenceRefs names the cells or events this item was drawn from. Refute
	// follows them: a derived cell that cites a refuted one is retired with it.
	EvidenceRefs []string `json:"evidenceRefs,omitempty"`
	// Supersedes lists the ids of cells this one absorbed (ConsolidateMemory).
	Supersedes []string `json:"supersedes,omitempty"`
	// Via is set only on an item appended by Search's ThoughtLink expansion —
	// the ID of the higher-ranked result whose link surfaced it. Empty for
	// every directly matched result. Render/annotation-only: never read back
	// by Upsert, so a caller cannot set or spoof it.
	Via string `json:"via,omitempty"`
	// Collapsed is set only on a Search result that stands for near-duplicates
	// folded into it (see collapseNearDuplicates). Render-only, like Via.
	Collapsed int `json:"collapsed,omitempty"`
}

// toolCallStat tracks invocation count and cumulative duration for one tool.
type toolCallStat struct {
	calls   int64
	totalNs int64
}

// MemoryStore is an HNSW-backed semantic memory store.
// When an Embedder is provided, search_memory uses vector similarity.
// Without an embedder it falls back to keyword matching (for dev/testing).
//
// CR-backed items are read directly from the controller-runtime cache (reader/namespace).
// Ephemeral items written via Upsert (agent_note, session writes) live only in items map.
type MemoryStore struct {
	mu        sync.RWMutex
	items     map[string]*MemoryItem       // ephemeral: Upsert/Delete writes, not yet in a CR
	cells     *vectorstore.MemoryCellStore // nil if no embedder
	embedder  *vectorstore.Embedder        // nil in keyword mode
	logger    zerolog.Logger
	hub       *Hub
	counters  searchCounters
	reader    client.Reader // non-nil when controller-runtime manager is available
	writer    client.Client // non-nil when controller-runtime manager is available
	namespace string
	kwIdx     *trigramIndex

	toolMu    sync.Mutex
	toolStats map[string]*toolCallStat

	usageMu sync.Mutex
	usage   map[string]*userDaily // userHash → current-UTC-day token spend

	// statsDirty is set by markStatsDirty whenever a counter, session, or usage
	// row changes, and cleared by the background flusher after it writes the CR.
	// This decouples the hot request path (search, voice, login) from the etcd
	// write: callers flip a flag instead of doing a Get+Status().Update each, so
	// N concurrent searches collapse to at most one CR write per flush tick
	// instead of N. See ADR-047 (debounced stats flush).
	statsDirty atomic.Bool

	// episodicTTL is the max age of an episodic memory before it is forgotten
	// (ADR-020). 0 disables expiry. Read paths (ListRecent) skip items older than
	// this; the background reaper deletes their CRs. Set once at startup, read-only
	// thereafter, so no lock is needed.
	episodicTTL time.Duration

	// linkExpand gates Search's optional ThoughtLink 1-hop expansion (see
	// expandViaLinks). Default false (zero value): production behavior is
	// byte-identical to pre-expansion Search until SetLinkExpansion(true) is
	// called. atomic.Bool since Search runs on the hot read path with no
	// other lock held at the point it's checked.
	linkExpand atomic.Bool

	// sourceRel, when non-empty, turns on Search's source-collapse step
	// (SetSourceCollapse): a hit carrying an outbound ThoughtLink of this
	// relation is replaced by the link's target — the retrieval unit is the
	// small derived cell (an atomic fact), the reading unit is the source it
	// was derived from (an episode). This is the "rank on facts, read the
	// episode" split EverMemOS builds its MemCell around and the dual-storage
	// answer to the single-hop regression our facts-only arm measured
	// (docs/todo/graphiti-comparison-refinement.md item #2). atomic.Value of
	// string; "" means off, which is the default and byte-identical to before.
	sourceRel atomic.Value

	// fanout overrides the default candidateFanout over-fetch factor (see
	// SetCandidateFanout). 0 means "unset" — Search falls back to the
	// candidateFanout constant.
	fanout atomic.Int32

	// alwaysFuse, when true, disables Search's lexical confident short-circuit
	// (see SetLexicalShortCircuit): a full-coverage top lexical hit no longer
	// returns the lexical list on its own — Search always fuses lexical with
	// semantic by RRF when a semantic store exists. Zero value (false) is
	// today's default behaviour, byte-identical to before this field existed.
	alwaysFuse atomic.Bool

	// noShaping disables the near-duplicate collapse and state-aware re-rank
	// (SetRecallShaping); noWriteDedup disables merging a restated fact into
	// the cell it repeats (SetWriteDedup). Zero values leave both on.
	noShaping    atomic.Bool
	noWriteDedup atomic.Bool

	// seen is the last shared state of each cell this pod published or applied
	// (markSeen, applyRemote); guarded by mu. See memory_remote.go.
	seen map[string]*MemoryItem
}

// candidateFanout is how many times `limit` Search pulls from each channel
// when a second-stage step (source collapse) will shrink the pool
// again. Three is EverMemOS's own ratio (top-30 pooled → 10 read).
const candidateFanout = 3

// SetSourceCollapse turns on Search's source-collapse step for links of the
// given relation (see MemoryStore.sourceRel); "" turns it off. Safe to call at
// any time; takes effect on the next Search call.
func (s *MemoryStore) SetSourceCollapse(relation string) {
	s.sourceRel.Store(relation)
}

func (s *MemoryStore) sourceCollapseRel() string {
	v, _ := s.sourceRel.Load().(string)
	return v
}

// SetCandidateFanout overrides the over-fetch factor Search multiplies limit
// by when a second-stage step (source collapse) is going to shrink
// the candidate pool again — see candidateFanout. It exists because source
// collapse changes the pool's unit: Search's channels rank *facts*, but a
// caller wants *sources* (episodes) back, and one source can dominate the
// pool with many derived facts. On LoCoMo, a 10-turn episode yields roughly
// 26 fact candidates; a 3×limit pool of facts (the candidateFanout default)
// collapses to only ~7 distinct episodes for limit=20 — nowhere near enough
// to fill the request. A wider pool (10x) is needed so enough distinct
// sources survive the collapse to fill limit. This is cheap to buy: the
// over-fetch cost is paid entirely by the in-process HNSW/keyword search, not
// a network round-trip. Values below 1 are treated as 1 (Search never
// over-fetches less than limit itself). Safe to call at any time; takes
// effect on the next Search call.
func (s *MemoryStore) SetCandidateFanout(n int) {
	if n < 1 {
		n = 1
	}
	s.fanout.Store(int32(n))
}

// SetLexicalShortCircuit turns Search's lexical confident short-circuit on or
// off (see MemoryStore.alwaysFuse). The library default (an unconfigured
// MemoryStore) is on=true: a full-coverage top lexical hit returns the
// lexical list without ever consulting the semantic channel. on=false makes
// Search always fuse lexical with semantic by RRF whenever a semantic store
// is configured — the lexical-only return still happens when there is no
// semantic store at all (s.cells == nil), since there is nothing to fuse
// with. cmd/xray, the xray binary, calls SetLexicalShortCircuit(false) at
// startup unless --search-lexical-shortcircuit is passed, so the binary's
// product default differs from this library default; embedders of
// internal/xray that never call this setter keep getting on=true.
//
// Why this needs to be switchable: the short-circuit is the right call for
// short named-entity voice queries ("Billing RAG project") — one confident
// lexical hit, no reason to pay for a Vertex AI round-trip. It is the wrong
// call for long natural-language questions asked over long cells: a 10-turn
// LoCoMo episode shares tokens with almost any question about it, so the top
// lexical hit reads as "confident" by coverage even when it isn't the actual
// answer. Measured on the LoCoMo eval set, that false confidence returned a
// single lexical hit — and skipped semantic retrieval entirely — for 41% of
// questions. SetLexicalShortCircuit(false) is the escape hatch for that
// regime. Safe to call at any time; takes effect on the next Search call.
func (s *MemoryStore) SetLexicalShortCircuit(on bool) {
	s.alwaysFuse.Store(!on)
}

// candidateFanoutFactor returns the store's configured over-fetch factor, or
// the candidateFanout default when none has been set via SetCandidateFanout.
func (s *MemoryStore) candidateFanoutFactor() int {
	if n := s.fanout.Load(); n != 0 {
		return int(n)
	}
	return candidateFanout
}

// SetLinks replaces the ThoughtLinks of an ephemeral (s.items) item. CR-backed
// items get their links from the cell spec via cellToItem; this is the write
// path for links on items that were never persisted — an ingest pipeline
// deriving atomic-fact cells from an episode cell links each fact back to
// its source with it. Returns false when id is not an ephemeral item.
func (s *MemoryStore) SetLinks(id string, links []mcell.ThoughtLink) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[id]
	if !ok {
		return false
	}
	it.Links = append([]mcell.ThoughtLink(nil), links...)
	return true
}

// SetLinkExpansion turns Search's optional ThoughtLink 1-hop expansion on or
// off (default off — see docs/todo/graphiti-comparison-refinement.md item
// #4). Safe to call at any time; takes effect on the next Search call.
func (s *MemoryStore) SetLinkExpansion(on bool) {
	s.linkExpand.Store(on)
}

// SetEpisodicTTL sets the max age before an episodic memory is forgotten. 0
// disables expiry. Call once at startup before serving.
func (s *MemoryStore) SetEpisodicTTL(ttl time.Duration) { s.episodicTTL = ttl }

// userDaily tracks one authenticated user's Gemini Live token spend for a UTC day.
type userDaily struct {
	day    string // UTC calendar day, "YYYY-MM-DD"
	tokens int64
}

// utcDay returns the current UTC calendar day as "YYYY-MM-DD".
func utcDay() string { return time.Now().UTC().Format("2006-01-02") }

// NewMemoryStore creates a keyword-only MemoryStore (no embedding endpoint).
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		items:     make(map[string]*MemoryItem),
		logger:    zerolog.Nop(),
		hub:       &Hub{clients: make(map[chan []byte]string), hitCounts: make(map[string]*atomic.Int64)},
		kwIdx:     newTrigramIndex(),
		seen:      make(map[string]*MemoryItem),
		toolStats: make(map[string]*toolCallStat),
		usage:     make(map[string]*userDaily),
	}
}

// NewMemoryStoreWithEmbedder creates an HNSW-backed MemoryStore.
func NewMemoryStoreWithEmbedder(embedder *vectorstore.Embedder, logger zerolog.Logger) *MemoryStore {
	cells := vectorstore.NewMemoryCellStore(embedder, logger)
	return &MemoryStore{
		items:     make(map[string]*MemoryItem),
		cells:     cells,
		embedder:  embedder,
		logger:    logger,
		hub:       &Hub{clients: make(map[chan []byte]string), hitCounts: make(map[string]*atomic.Int64)},
		kwIdx:     newTrigramIndex(),
		seen:      make(map[string]*MemoryItem),
		toolStats: make(map[string]*toolCallStat),
		usage:     make(map[string]*userDaily),
	}
}

// SetSemanticScoreGap overrides the semantic relevance cutoff (absolute cosine
// gap below the top hit) on the underlying cell store. No-op in keyword-only mode.
func (s *MemoryStore) SetSemanticScoreGap(gap float32) {
	if s.cells != nil {
		s.cells.SetScoreGap(gap)
	}
}

// SetCacheReader wires the controller-runtime cache reader so List/KeywordSearch
// queries go to the cache instead of the in-process items map for CR-backed items.
// Call this after the manager cache has synced.
func (s *MemoryStore) SetCacheReader(r client.Reader, namespace string) {
	s.mu.Lock()
	s.reader = r
	s.namespace = namespace
	s.mu.Unlock()
	s.buildTrigramIndex(r, namespace)
}

// buildTrigramIndex bulk-populates the trigram index from all active CRs.
func (s *MemoryStore) buildTrigramIndex(r client.Reader, namespace string) {
	var list mcell.MemoryCellList
	if err := r.List(context.Background(), &list, client.InNamespace(namespace)); err != nil {
		s.logger.Warn().Err(err).Msg("trigram index: cache list failed")
		return
	}
	for i := range list.Items {
		cell := &list.Items[i]
		if !isActive(cell.Spec.Status) || isPrivateItem(cell.Spec.Kind, cell.Spec.Scope) {
			continue
		}
		s.kwIdx.index(cell.Spec.ID, cell.Spec.Title, cell.Spec.Content)
	}
	s.logger.Debug().Int("cells", len(list.Items)).Msg("trigram index built")
}

// SetWriter wires the controller-runtime client so flushStats can update CR status.
func (s *MemoryStore) SetWriter(c client.Client) {
	s.mu.Lock()
	s.writer = c
	s.mu.Unlock()
}

// rrfK is the Reciprocal Rank Fusion constant. Larger values flatten the
// contribution of rank position; 60 is the value from the original RRF paper.
const rrfK = 60

// shouldShortCircuit is the pure decision Search's lexical fast path makes:
// return lexical results on their own, without consulting semantic, when
// there is no semantic store to fuse with, or when the top lexical hit is
// confident (full-coverage) AND the short-circuit hasn't been disabled via
// SetLexicalShortCircuit(false) (alwaysFuse). Split out from Search so the
// decision itself is unit-testable without a semantic store — see
// MemoryStore.alwaysFuse and SetLexicalShortCircuit for why this needs to be
// switchable at all.
func shouldShortCircuit(hasCells, lexConfident, alwaysFuse bool) bool {
	return !hasCells || (lexConfident && !alwaysFuse)
}

// Search returns items matching the query (ADR-030). It runs lexical search
// first; when a semantic store is configured and the top lexical hit is not a
// confident (full-coverage) match, it fuses semantic results in by Reciprocal
// Rank Fusion. Without an embedder it returns lexical results directly.
func (s *MemoryStore) Search(ctx context.Context, query, kind, subject string, limit int) []*MemoryItem {
	if limit <= 0 {
		limit = 10
	}
	if query == "" {
		return nil
	}
	items := s.search(ctx, query, kind, subject, limit)
	hit := s.exactIDMatch(query, kind, subject)
	if hit == nil {
		return items
	}
	// A query that is a cell's id (pasted from a listing or another result) goes
	// first: the id is exactly what the caller meant.
	out := make([]*MemoryItem, 0, len(items)+1)
	out = append(out, hit)
	for _, it := range items {
		if it.ID != hit.ID && len(out) < limit {
			out = append(out, it)
		}
	}
	return out
}

// exactIDMatch returns the active cell whose id is the query, when the caller
// may see it (kind filter, subject scope, privacy), else nil. A query with
// whitespace in it is prose, not an id.
func (s *MemoryStore) exactIDMatch(query, kind, subject string) *MemoryItem {
	id := strings.TrimSpace(query)
	if id == "" || len(id) > 256 || strings.ContainsAny(id, " \t\r\n") {
		return nil
	}
	it := s.GetItem(id)
	if it == nil || !isActive(it.Status) || it.Kind == "system_configuration" {
		return nil
	}
	if kind != "" && it.Kind != kind {
		return nil
	}
	if !itemVisibleForSubject(it, subject) {
		return nil
	}
	return it
}

// search is Search's ranked path: lexical, then semantic fused in when the
// lexical answer is not confident.
func (s *MemoryStore) search(ctx context.Context, query, kind, subject string, limit int) []*MemoryItem {
	s.counters.total.Add(1)
	s.markStatsDirty()

	// Time the whole search; mode reflects which path served it (lexical fast path
	// vs semantic-fused). The deferred record reads mode by pointer so it captures
	// whichever return ran.
	start := time.Now()
	mode := "lexical"
	defer func() { observability.RecordMemorySearchDuration(ctx, mode, time.Since(start).Seconds()) }()

	// A second-stage step (source collapse) shrinks the pool again, so
	// each channel over-fetches for it; otherwise the pool is exactly limit.
	collapseRel := s.sourceCollapseRel()
	want := s.poolSize(limit)

	// Lexical first — deterministic, no network, dominant for named-entity queries.
	lexItems, lexConfident := s.keywordSearchScored(query, kind, subject, want)
	s.counters.kwHits.Add(1)

	// No semantic store, or lexical already has a confident (full-coverage) hit
	// and the short-circuit is enabled (SetLexicalShortCircuit, on by default)
	// → return lexical (the fast path: no Vertex AI round-trip, no looser
	// recall). See shouldShortCircuit and
	// SetLexicalShortCircuit for why this decision needs to be switchable.
	if shouldShortCircuit(s.cells != nil, lexConfident, s.alwaysFuse.Load()) {
		s.TrackKWVoiceHit()
		pool := s.shape(s.collapseToSources(lexItems, collapseRel, subject, want))
		if len(pool) > limit {
			pool = pool[:limit]
		}
		return s.expandViaLinks(pool, subject, limit)
	}

	// Low-confidence lexical: fuse with semantic by Reciprocal Rank Fusion.
	mode = "fused"
	s.TrackKWVoiceMiss()
	semItems := s.semanticSearch(ctx, query, kind, subject, want)
	pool := lexItems
	if len(semItems) > 0 {
		pool = fuseRRF(lexItems, semItems, want)
	}
	pool = s.collapseToSources(pool, collapseRel, subject, want)
	pool = s.shape(pool)
	if len(pool) > limit {
		pool = pool[:limit]
	}
	return s.expandViaLinks(pool, subject, limit)
}

// collapseToSources implements the source-collapse step (SetSourceCollapse):
// walking items best-first, an item with an outbound link of relation rel is
// replaced by that link's target when the target is active and visible to
// the caller; the first (best-ranked) item to reach a target claims its
// position and later items collapsing to the same target are dropped, so an
// episode that several of its facts matched ranks where its best fact did.
// Items without such a link pass through unchanged, deduplicated against
// targets already claimed. Returns at most limit items. A no-op (same slice)
// when rel is "".
func (s *MemoryStore) collapseToSources(items []*MemoryItem, rel, subject string, limit int) []*MemoryItem {
	if rel == "" || len(items) == 0 {
		return items
	}
	seen := make(map[string]struct{}, len(items))
	out := make([]*MemoryItem, 0, len(items))
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
		if target == "" {
			if _, dup := seen[it.ID]; dup {
				continue
			}
			seen[it.ID] = struct{}{}
			out = append(out, it)
			continue
		}
		if _, dup := seen[target]; dup {
			continue
		}
		src := s.GetItem(target)
		if src == nil || !isActive(src.Status) || !itemVisibleForSubject(src, subject) {
			// Dangling or hidden source: keep the fact itself rather than lose
			// the hit.
			if _, dup := seen[it.ID]; dup {
				continue
			}
			seen[it.ID] = struct{}{}
			out = append(out, it)
			continue
		}
		seen[target] = struct{}{}
		clone := *src
		clone.Via = it.ID
		out = append(out, &clone)
	}
	return out
}

// expandViaLinks optionally expands a fused/lexical top-k result set one hop
// through ThoughtLinks, re-fusing the neighbors in after the direct hits
// (docs/todo/graphiti-comparison-refinement.md item #4). A no-op — returning
// base unchanged, same slice/pointers — unless SetLinkExpansion(true) has
// been called, so default (and every existing caller's) behavior is
// byte-identical to before this method existed.
//
// Outbound links only: a reverse lookup (which items link TO a result) would
// need a scan of the whole store per Search call with no supporting index,
// which isn't cheap in this codebase (no reverse-link index exists) — see the
// item's "otherwise outbound only" fallback.
//
// Neighbors are ranked by the position of the item that links to them (a
// neighbor of result #1 outranks a neighbor of result #5): collected in
// base's rank order, so the first (best-ranked) linker to reach a given
// target wins the dedup. At most limit/2 neighbors are added, the total
// never exceeds limit, and each added item's Via is stamped with the ID of
// the result that linked it in — the reader-facing marker that distinguishes
// an expanded item from a direct hit. Skips: targets already in the result
// set, non-active/superseded targets, and targets outside the caller's
// subject scope (the identical visibility rule the base search itself
// already applies — see itemVisibleForSubject).
func (s *MemoryStore) expandViaLinks(base []*MemoryItem, subject string, limit int) []*MemoryItem {
	if !s.linkExpand.Load() || len(base) == 0 {
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
	for _, it := range base {
		seen[it.ID] = struct{}{}
	}

	type linkTarget struct {
		toID     string
		fromRank int
	}
	var targets []linkTarget
	for rank, it := range base {
		for _, link := range it.Links {
			if link.ToID == "" || link.ToID == it.ID {
				continue
			}
			if _, ok := seen[link.ToID]; ok {
				continue // already a direct hit, or already queued from a better-ranked linker
			}
			seen[link.ToID] = struct{}{}
			targets = append(targets, linkTarget{toID: link.ToID, fromRank: rank})
		}
	}
	if len(targets) == 0 {
		return base
	}
	// Collected in base's rank order already (outer loop is rank-ascending),
	// so this is a no-op stable sort — kept explicit so the ordering
	// invariant survives a future refactor of the collection loop above.
	sort.SliceStable(targets, func(i, j int) bool { return targets[i].fromRank < targets[j].fromRank })

	out := make([]*MemoryItem, len(base), len(base)+maxExtra)
	copy(out, base)
	added := 0
	for _, t := range targets {
		if added >= maxExtra {
			break
		}
		it := s.GetItem(t.toID)
		if it == nil || !isActive(it.Status) {
			continue
		}
		if !itemVisibleForSubject(it, subject) {
			continue
		}
		// GetItem may return a pointer aliasing live store state (the
		// ephemeral s.items map case) — clone before stamping Via so the
		// annotation never leaks into the stored item.
		clone := *it
		clone.Via = base[t.fromRank].ID
		out = append(out, &clone)
		added++
	}
	return out
}

// itemVisibleForSubject reports whether it is visible to a caller who
// searched with the given subject — the same two-part rule
// keywordSearchMap/keywordSearchCache already apply per candidate: a private
// item requires a non-empty subject, and once a subject is given every item
// (public or private) must belong to it. Shared here so expandViaLinks
// applies identical visibility to an expanded neighbor as Search already
// applies to a direct hit, without restating the rule from scratch.
func itemVisibleForSubject(it *MemoryItem, subject string) bool {
	if isPrivateItem(it.Kind, it.Scope) && subject == "" {
		return false
	}
	if subject != "" && !strings.EqualFold(it.Subject, subject) {
		return false
	}
	return true
}

// fuseRRF combines two rankings by Reciprocal Rank Fusion: each item's fused
// score is the sum of 1/(rrfK+rank) across the lists it appears in. Ranks are
// 0-based in input order (both inputs arrive already sorted best-first).
func fuseRRF(lex, sem []*MemoryItem, limit int) []*MemoryItem {
	fused := make(map[string]float64)
	byID := make(map[string]*MemoryItem)
	add := func(items []*MemoryItem) {
		for rank, it := range items {
			fused[it.ID] += 1.0 / float64(rrfK+rank)
			byID[it.ID] = it
		}
	}
	add(lex)
	add(sem)

	ids := make([]string, 0, len(fused))
	for id := range fused {
		ids = append(ids, id)
	}
	sort.SliceStable(ids, func(i, j int) bool {
		if fused[ids[i]] != fused[ids[j]] {
			return fused[ids[i]] > fused[ids[j]]
		}
		return ids[i] < ids[j]
	})

	out := make([]*MemoryItem, 0, limit)
	for _, id := range ids {
		if len(out) >= limit {
			break
		}
		out = append(out, byID[id])
	}
	return out
}

func (s *MemoryStore) semanticSearch(ctx context.Context, query, kindFilter, subject string, topK int) []*MemoryItem {
	s.counters.semCalls.Add(1)
	ctx = vectorstore.WithTimingSink(ctx, func(t vectorstore.SearchTiming) {
		observability.RecordMemorySearchPhase(ctx, "embed", t.Embed.Seconds())
		observability.RecordMemorySearchPhase(ctx, "index", t.Index.Seconds())
	})
	var results []vectorstore.CellSearchResult
	var err error
	if subject != "" {
		results, err = s.cells.SearchSubject(ctx, query, subject, topK, kindFilter)
	} else {
		results, err = s.cells.Search(ctx, query, topK, kindFilter)
	}
	if err != nil {
		s.logger.Warn().Err(err).Msg("semantic search failed — falling back to keyword")
		s.counters.semFallbacks.Add(1)
		return s.KeywordSearch(query, kindFilter, subject, topK)
	}

	hydrate := func(rs []vectorstore.CellSearchResult) []*MemoryItem {
		var out []*MemoryItem
		for _, r := range rs {
			it := s.getByID(ctx, r.Entry.ID)
			if it == nil || !isActive(it.Status) {
				continue
			}
			out = append(out, it)
			if len(out) >= topK {
				break
			}
		}
		return out
	}
	out := hydrate(results)
	if len(out) > 0 || len(results) == 0 {
		return out
	}
	// The whole top-K window was rejected on hydration: every hit is a cell that
	// was deleted or retired elsewhere and is still in this pod's index. Rank
	// everything exactly so the cells that do qualify are not starved by it.
	all, err := s.cells.SearchExhaustive(ctx, query, subject, kindFilter)
	if err != nil {
		return nil
	}
	return hydrate(all)
}

// normalizeQuery lowercases, replaces separators with spaces, and splits
// CamelCase/PascalCase boundaries so "BillingREC" → "billing rec".
func normalizeQuery(q string) string {
	// Insert space before uppercase letters that follow a lowercase letter (CamelCase split).
	var sb strings.Builder
	runes := []rune(q)
	for i, r := range runes {
		if i > 0 && r >= 'A' && r <= 'Z' {
			prev := runes[i-1]
			if prev >= 'a' && prev <= 'z' {
				sb.WriteByte(' ')
			}
		}
		sb.WriteRune(r)
	}
	q = sb.String()
	q = strings.ToLower(q)
	q = strings.ReplaceAll(q, "_", " ")
	q = strings.ReplaceAll(q, "-", " ")
	return q
}

// categoryStopwords are tokens that name a memory's UI category rather than
// identify the item. The X-Ray pages are Projects, Team, News, Blog and Events,
// and the voice model appends these category words to queries as hints
// ("LLM as judge blog", "Billing RAG project"). As search terms they neither
// narrow nor distinguish — "blog" appears in many items' prose, "project" tags
// every Projects item — so they are dropped from queries. "LLM as judge blog"
// matches on "llm as judge", "Billing RAG project" on "billing rag". They are only
// stripped when other tokens remain, so a bare "blog" query still tokenises (and
// listing a category is get_memory_items' job, not search_memory's).
var categoryStopwords = map[string]struct{}{
	"project":     {}, // Projects (system_identity + project tag)
	"contributor": {}, // Team (contributor tag)
	"blog":        {}, // Blog (stable_fact)
	"news":        {}, // News (recent_change)
	"event":       {}, // Events (event)
	"events":      {},
}

// kwTokens splits a normalized query into non-empty tokens, dropping category
// stopwords when at least one non-stopword token remains.
func kwTokens(q string) []string {
	parts := strings.Fields(q)
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	// Drop category stopwords, but only if doing so leaves at least one token.
	kept := out[:0]
	for _, t := range out {
		if _, stop := categoryStopwords[t]; !stop {
			kept = append(kept, t)
		}
	}
	if len(kept) > 0 {
		return kept
	}
	return out
}

// editDistance returns the Levenshtein distance between a and b, measured in
// runes. Must operate on runes, not bytes: a byte-indexed DP gives garbage
// distances for multibyte words (e.g. "déjà") because range-over-string yields
// byte offsets while the slices are sized by byte length — which produced
// spurious 0-distance matches.
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

func min3(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}

// matchesKWToken returns true when a single token matches the haystack —
// exact substring first, then a length-scaled fuzzy word match.
func matchesKWToken(h, t string) bool {
	if strings.Contains(h, t) {
		return true
	}
	return fuzzyWordMatch(h, t)
}

// Lexical scoring weights (ADR-030). lexScore folds the whole-query title
// bonuses, graded per-token match quality, and a small trigram tie-breaker into
// one ranking score. Token coverage (matchesKWToken) remains the *inclusion*
// gate — see softMatchCoverage.
//
// Match quality is graded so that an exact word match beats a fuzzy one, and a
// title match beats a content match. This matters for voice mishears: "rac" is a
// 1-edit fuzzy match to "rag" (Billing RAG) but shares zero trigrams, so trigram
// overlap alone wrongly favours a longer item whose body contains an incidental
// high-overlap word. Match quality must dominate the trigram fraction.
const (
	wExactTitle   = 1000 // title equals the whole normalized query
	wTitleContain = 600  // title contains the whole query as a substring
	wTokExactTle  = 100  // per-token: exact substring in title
	wTokFuzzyTle  = 60   // per-token: 1-edit fuzzy match in title
	wTokExactCnt  = 40   // per-token: exact substring in content
	wTokFuzzyCnt  = 25   // per-token: 1-edit fuzzy match in content
	wTrigram      = 20   // multiplied by mean trigram-overlap fraction (0..1) — tie-breaker only
	wMissPenalty  = 200  // per missing token — keeps full-coverage above partial
)

// tokenMatchQuality returns the best per-token score against title/content,
// graded exact-title > fuzzy-title > exact-content > fuzzy-content > none. nt/nc
// are the already-lowercased title/content. Fuzzy uses fuzzyWordMatch (length-
// scaled edit tolerance), matching matchesKWToken.
func tokenMatchQuality(nt, nc, t string) int {
	if strings.Contains(nt, t) {
		return wTokExactTle
	}
	if fuzzyWordMatch(nt, t) {
		return wTokFuzzyTle
	}
	if strings.Contains(nc, t) {
		return wTokExactCnt
	}
	if fuzzyWordMatch(nc, t) {
		return wTokFuzzyCnt
	}
	return 0
}

// fuzzyTolerance is the allowed Levenshtein distance for a fuzzy word match,
// scaled by token length. 1-2 char tokens demand exactness (a 1-edit ball around
// them is far too broad); tokens of 3+ chars allow 1 edit. This catches both
// short mishears ("rek"→"rec", "rac"→"rag") and long ones ("lunchpad"→
// "launchpad") — the old hard ≤6-char cap blocked the latter entirely. Spurious
// near-collisions (rac/rag/rec) are sorted out by match-quality grading and the
// coverage gate, not by disabling fuzzy.
func fuzzyTolerance(n int) int {
	if n <= 2 {
		return 0
	}
	return 1
}

// fuzzyWordMatch reports whether t is within its length-scaled edit tolerance of
// any single word in h. A tolerance of 0 means no fuzzy match (exact only).
//
// It also tests t against adjacent word pairs joined without a space, because
// normalizeQuery splits CamelCase ("LaunchPad" → "launch pad"): a mishear
// token like "lunchpad" must still match the original single word. The join
// uses the token's tolerance sized for the *joined* length.
func fuzzyWordMatch(h, t string) bool {
	tn := utf8.RuneCountInString(t)
	tol := fuzzyTolerance(tn)
	if tol == 0 {
		return false
	}
	words := strings.Fields(h)
	lens := make([]int, len(words))
	for i, w := range words {
		lens[i] = utf8.RuneCountInString(w)
	}
	for i, word := range words {
		// Levenshtein distance is at least the length difference, so a word
		// whose length gap already exceeds the tolerance can never match: skip
		// the DP, which is the hot path of the whole lexical scan.
		if absDiff(lens[i], tn) <= tol && editDistanceWithin(t, word, tol) {
			return true
		}
		// Try joining with the next word to undo a CamelCase/space split.
		if i+1 < len(words) {
			jn := lens[i] + lens[i+1]
			jtol := fuzzyTolerance(jn)
			if absDiff(jn, tn) <= jtol && editDistanceWithin(t, word+words[i+1], jtol) {
				return true
			}
		}
	}
	return false
}

// softMatchCoverage reports whether the item clears the token-coverage gate and
// how many tokens it matched. Coverage rule (preserves prior precision):
//   - 1-2 token queries: every token must match (strict AND)
//   - 3+ token queries: one token may be absent ("Billing RAG project" → "Billing RAG")
//
// A token matches when it appears in title OR content via exact substring or a
// 1-edit fuzzy match (matchesKWToken).
func softMatchCoverage(title, content string, toks []string) (matched int, ok bool) {
	if len(toks) == 0 {
		return 0, true
	}
	nt := strings.ToLower(title)
	nc := strings.ToLower(content)
	for _, t := range toks {
		if matchesKWToken(nt, t) || matchesKWToken(nc, t) {
			matched++
		}
	}
	need := len(toks)
	if len(toks) >= 3 {
		need = len(toks) - 1
	}
	return matched, matched >= need
}

// lexScore is the unified lexical ranking score of an item for a tokenised
// query. weights carries each query token's distinctiveness (tokenWeights); nil
// weighs every token equally. trigramFrac is the item's mean per-token trigram
// overlap (0..1) from the index (pass 0 when there is no index signal).
//
// The per-token match quality is scaled by the token's weight relative to the
// query's mean weight and the sum divided back by that mean, so the score stays
// on its original scale for a query of near-equal tokens while a rare token's
// match outweighs a filler token's. The miss penalty scales by the weighted
// fraction of the query left unmatched, so missing a filler word costs almost
// nothing and missing the entity costs nearly the full penalty. The exact-phrase
// title bonus and the trigram tie-breaker are unweighted.
func lexScore(title, content, q string, toks []string, weights map[string]float64, trigramFrac float64) int {
	nt := normalizeQuery(title)
	nc := normalizeQuery(content)

	score := 0
	if q != "" && nt == q {
		score += wExactTitle
	} else if q != "" && strings.Contains(nt, q) {
		score += wTitleContain
	}
	var matchedWeight, totalWeight float64
	for _, t := range toks {
		w := tokenWeight(weights, t)
		totalWeight += w
		tq := tokenMatchQuality(nt, nc, t)
		if tq > 0 {
			matchedWeight += w
		}
		score += int(float64(tq) * w)
	}
	if len(toks) > 0 && totalWeight > 0 {
		score = int(float64(score) / (totalWeight / float64(len(toks))))
	}
	score += int(trigramFrac * wTrigram)
	if len(toks) > 0 && totalWeight > 0 && matchedWeight < totalWeight {
		score -= int(((totalWeight - matchedWeight) / totalWeight) * float64(len(toks)) * wMissPenalty)
	}
	return score
}

// KeywordSearch scans items by substring match on title+content.
// When the cache reader is available, queries the controller-runtime cache for
// CR-backed items (using field indexes for kind/subject); ephemeral items from
// the in-process map are merged in.
func (s *MemoryStore) KeywordSearch(query, kind, subject string, limit int) []*MemoryItem {
	return s.keywordSearch(query, kind, subject, limit, 0)
}

// KeywordSearchPerKind is KeywordSearch with per-kind fairness: at most perKind
// results per kind, so a dominant kind can't monopolise the flat limit and
// starve smaller categories. Relevance ordering within each kind is preserved
// and limit still caps the total. Used by the graph search (/v1/memory?q=),
// which spans all kinds over a skewed store.
func (s *MemoryStore) KeywordSearchPerKind(query, kind, subject string, limit, perKind int) []*MemoryItem {
	return s.keywordSearch(query, kind, subject, limit, perKind)
}

func (s *MemoryStore) keywordSearch(query, kind, subject string, limit, perKind int) []*MemoryItem {
	ctx := context.Background()
	q := normalizeQuery(query)

	s.mu.RLock()
	r := s.reader
	ns := s.namespace
	s.mu.RUnlock()

	if r != nil {
		return s.keywordSearchCache(ctx, r, ns, q, kind, subject, limit, perKind)
	}
	return s.keywordSearchMap(q, kind, subject, limit, perKind)
}

// keywordSearchScored runs KeywordSearch and reports whether the top hit is a
// *confident* lexical answer. Confidence is coverage-based, not score-based: a
// multi-token name query like "Billing RAG project" matches "Billing RAG" on 2 of
// 3 tokens and is confident, even though the per-token miss penalty drags its raw
// score near zero. Used by Search to decide whether the semantic round-trip (and
// its looser recall) is worth fusing in.
func (s *MemoryStore) keywordSearchScored(query, kind, subject string, limit int) (items []*MemoryItem, confident bool) {
	items = s.KeywordSearch(query, kind, subject, limit)
	if len(items) == 0 {
		return items, false
	}
	toks := kwTokens(normalizeQuery(query))
	if len(toks) == 0 {
		return items, true
	}
	matched, _ := softMatchCoverage(items[0].Title, itemHaystack(items[0]), toks)
	// Confident when the top hit covers every token, or all-but-one for queries
	// of 3+ tokens (the same soft-AND tolerance used for inclusion).
	need := len(toks)
	if len(toks) >= 3 {
		need = len(toks) - 1
	}
	return items, matched >= need
}

// appendFieldSelectors merges kind and subject into a single MatchingFields option.
// controller-runtime only honours one MatchingFields per List call — passing two
// separate options causes the second to silently override the first.
func appendFieldSelectors(opts []client.ListOption, kind, subject string) []client.ListOption {
	fields := client.MatchingFields{}
	if kind != "" {
		fields["spec.kind"] = kind
	}
	if subject != "" {
		fields["spec.subject"] = subject
	}
	if len(fields) > 0 {
		opts = append(opts, fields)
	}
	return opts
}

// keywordSearchCache uses the controller-runtime cache with field indexes.
func (s *MemoryStore) keywordSearchCache(ctx context.Context, r client.Reader, ns, q, kind, subject string, limit, perKind int) []*MemoryItem {
	toks := kwTokens(q)

	// Build trigram candidate set when a query is present.
	var trigramCandidates map[string]struct{}
	if q != "" {
		ids := s.kwIdx.search(q)
		if len(ids) > 0 {
			trigramCandidates = make(map[string]struct{}, len(ids))
			for _, id := range ids {
				trigramCandidates[id] = struct{}{}
			}
			s.logger.Debug().Str("query", q).Int("candidates", len(ids)).Msg("trigram candidates")
		}
	}

	listOpts := []client.ListOption{client.InNamespace(ns)}
	listOpts = appendFieldSelectors(listOpts, kind, subject)

	var cellList mcell.MemoryCellList
	if err := r.List(ctx, &cellList, listOpts...); err != nil {
		s.logger.Warn().Err(err).Msg("cache list failed — falling back to map scan")
		return s.keywordSearchMap(q, kind, subject, limit, perKind)
	}

	// Single scored pass: score every candidate via lexScore (ADR-030), keep
	// those above the floor, then rank. No boolean AND gate — partial-coverage
	// items surface but rank below full matches.
	seen := make(map[string]struct{})
	var cands []*MemoryItem
	for i := range cellList.Items {
		cell := &cellList.Items[i]
		if !isActive(cell.Spec.Status) {
			continue
		}
		if isPrivateItem(cell.Spec.Kind, cell.Spec.Scope) && subject == "" {
			continue
		}
		seen[cell.Spec.ID] = struct{}{}
		cands = append(cands, cellToItem(cell))
	}

	// Merge ephemeral items not yet in CRs (agent_note, in-flight Upserts).
	s.mu.RLock()
	for _, it := range s.items {
		if _, ok := seen[it.ID]; ok {
			continue
		}
		if !isActive(it.Status) {
			continue
		}
		if isPrivateItem(it.Kind, it.Scope) && subject == "" {
			continue
		}
		if kind != "" && it.Kind != kind {
			continue
		}
		if subject != "" && !strings.EqualFold(it.Subject, subject) {
			continue
		}
		cands = append(cands, it)
	}
	s.mu.RUnlock()

	return s.rankAndLimit(q, toks, cands, trigramCandidates, limit, perKind)
}

// rankAndLimit scores candidates with lexScore, drops those below the floor,
// sorts by score (recency/ID tie-break), and truncates to limit. When perKind
// > 0, it additionally caps results per kind so a dominant kind can't starve
// smaller categories. trigramCands (may be nil) carries the index pre-filter
// membership used to track the trigram hit counter. Shared by keywordSearchCache
// and keywordSearchMap.
func (s *MemoryStore) rankAndLimit(q string, toks []string, cands []*MemoryItem, trigramCands map[string]struct{}, limit, perKind int) []*MemoryItem {
	type scored struct {
		it      *MemoryItem
		score   int
		matched int
		// viaFallback marks an item admitted only by the weighted-mass fallback:
		// it failed the count-based gate (its matched count can be as low as one
		// rare entity token) and cleared a different bar, so the coverage cutoff
		// below, which enforces the count bar, must not apply to it.
		viaFallback bool
	}
	// Weights are computed once per query against the whole candidate set.
	weights := tokenWeights(toks, docFreqs(cands, toks), len(cands))
	var hits []scored
	trigramContributed := false
	for _, it := range cands {
		if len(toks) > 0 {
			hay := itemHaystack(it)
			matched, ok := softMatchCoverage(it.Title, hay, toks)
			viaFallback := false
			if !ok {
				// The count gate says too many tokens missing; if the tokens this
				// item did match carry enough of the query's weight, admit it.
				// Only for a query of three or more tokens: a one- or two-token
				// query is a name, and "edge cache" must not return "edge proxy"
				// because the corpus shares one word.
				if len(toks) < 3 || weightedMatchFraction(strings.ToLower(it.Title), strings.ToLower(hay), toks, weights) < wCoverageMassThreshold {
					continue
				}
				viaFallback = true
			}
			frac := s.kwIdx.itemScore(it.ID, toks)
			sc := lexScore(it.Title, hay, q, toks, weights, frac)
			if _, ok := trigramCands[it.ID]; ok {
				trigramContributed = true
			}
			hits = append(hits, scored{it, sc, matched, viaFallback})
		} else {
			hits = append(hits, scored{it, 0, 0, false})
		}
	}

	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		if !hits[i].it.UpdatedAt.Equal(hits[j].it.UpdatedAt) {
			return hits[i].it.UpdatedAt.After(hits[j].it.UpdatedAt)
		}
		return hits[i].it.ID < hits[j].it.ID
	})

	// Coverage cutoff: drop tail items that cover strictly fewer query tokens than
	// the best strict-gate hit. A single shared token like "billing" otherwise lets
	// a long tail clear the coverage gate; pruning by token coverage (not score) is
	// robust to the large whole-query title bonus that only the single best item
	// earns. A fallback-admitted item is neither the reference nor subject to it;
	// with no strict hit the step does nothing.
	if len(hits) > 1 {
		topCov := 0
		for _, h := range hits {
			if !h.viaFallback && h.matched > topCov {
				topCov = h.matched
			}
		}
		kept := hits[:0]
		for _, h := range hits {
			if h.viaFallback || h.matched >= topCov {
				kept = append(kept, h)
			}
		}
		hits = kept
	}

	// Truncate. hits are already sorted by relevance. When perKind > 0, cap the
	// number of results PER kind so a dominant kind can't monopolise the flat
	// limit and starve smaller categories (e.g. a query matching 97 events and
	// 36 concerns must still surface concerns, not just the top-50 events). The
	// overall limit still applies as a ceiling; in-kind ranking is preserved.
	out := make([]*MemoryItem, 0, limit)
	var perKindCount map[string]int
	if perKind > 0 {
		perKindCount = make(map[string]int, 16)
	}
	for _, h := range hits {
		if len(out) >= limit {
			break
		}
		if perKind > 0 {
			if perKindCount[h.it.Kind] >= perKind {
				continue
			}
			perKindCount[h.it.Kind]++
		}
		out = append(out, h.it)
	}
	if trigramContributed {
		s.counters.kwTrigramHits.Add(1)
	}
	// Always-on keyword-path trace: fires for every search regardless of whether
	// the trigram pre-filter contributed, so queries served purely by lexScore
	// substring matching (trigram_candidates=0) are no longer invisible.
	s.logger.Debug().
		Str("query", q).
		Int("trigram_candidates", len(trigramCands)).
		Bool("trigram_contributed", trigramContributed).
		Int("scored", len(cands)).
		Int("results", len(out)).
		Msg("keyword search")
	return out
}

// keywordSearchMap is the fallback O(n) scan used when no cache reader is wired.
// Uses the trigram index as a pre-filter when available.
// Trigram-matched items are ranked first so the best match is always within the limit.
func (s *MemoryStore) keywordSearchMap(q, kind, subject string, limit, perKind int) []*MemoryItem {
	toks := kwTokens(q)

	var trigramCandidates map[string]struct{}
	if q != "" {
		ids := s.kwIdx.search(q)
		if len(ids) > 0 {
			trigramCandidates = make(map[string]struct{}, len(ids))
			for _, id := range ids {
				trigramCandidates[id] = struct{}{}
			}
		}
	}

	s.mu.RLock()
	var cands []*MemoryItem
	for _, it := range s.items {
		if !isActive(it.Status) {
			continue
		}
		if isPrivateItem(it.Kind, it.Scope) && subject == "" {
			continue
		}
		if kind != "" && it.Kind != kind {
			continue
		}
		if subject != "" && !strings.EqualFold(it.Subject, subject) {
			continue
		}
		cands = append(cands, it)
	}
	s.mu.RUnlock()

	return s.rankAndLimit(q, toks, cands, trigramCandidates, limit, perKind)
}

type UpsertResult struct {
	ID     string
	Status string
	Item   *MemoryItem
}

// defaultTTLByKind stamps a per-kind default retention deadline on a newly
// CREATED item that carries no explicit StaleAfter — "decay by default". Only the kinds listed
// here ever get an implicit deadline; every other kind (stable_fact,
// operational_pattern, watch_item, system_configuration, system_identity,
// policy, mental_map/map-carrying cells, episodic, …) is left with a nil
// StaleAfter and never auto-expires. These are launch defaults pending
// per-deployment config — there is no override mechanism yet.
var defaultTTLByKind = map[string]time.Duration{
	"event":         30 * 24 * time.Hour,
	"recent_change": 90 * 24 * time.Hour,
	"note":          90 * 24 * time.Hour,
	// A hypothesis is a guess and an unknown a gap: both go stale unless
	// something renews them.
	"hypothesis": 24 * time.Hour,
	"unknown":    7 * 24 * time.Hour,
}

// UpsertOption sets an optional field of an Upsert.
type UpsertOption func(*upsertOpts)

type upsertOpts struct {
	evidenceRefs []string
	supersedes   []string
	noDedup      bool
}

// withSupersedes lists the cells the item replaces (Supersede).
func withSupersedes(ids []string) UpsertOption {
	return func(o *upsertOpts) { o.supersedes = ids }
}

// WithEvidenceRefs records the cells or events the item was drawn from. Refute
// follows them.
func WithEvidenceRefs(refs []string) UpsertOption {
	return func(o *upsertOpts) { o.evidenceRefs = refs }
}

// Upsert creates or updates a memory item matched by ID (if provided) or auto-generates one.
// The write's provenance comes from ctx (withWriteOrigin): an identified person
// is human, anything else derived. A restated stable_fact, operational_pattern,
// watch_item or hypothesis lands on the cell it repeats (status "merged").
func (s *MemoryStore) Upsert(ctx context.Context, id, kind, scope, title, content, subject, confidence, eventDate string, data map[string]mcell.DataEntry, tags []string, opts ...UpsertOption) (UpsertResult, string) {
	return s.upsertWithStaleAfter(ctx, id, kind, scope, title, content, subject, confidence, eventDate, data, tags, nil, opts...)
}

// upsertWithStaleAfter is Upsert's implementation, plus an explicit staleAfter
// deadline for callers that need to set expiry themselves (renewal) — Upsert
// passes nil, meaning "no explicit deadline given". See MemoryItem.StaleAfter
// and defaultTTLByKind for the decay-by-default contract this implements:
//   - CREATE, staleAfter given: respected as-is, no default applied.
//   - CREATE, staleAfter nil, kind has a default: stamp now+default.
//   - CREATE, staleAfter nil, kind has no default: left nil (never expires).
//   - UPDATE, staleAfter given: the item's deadline is renewed to it.
//   - UPDATE, staleAfter nil: the item's existing deadline is left untouched —
//     an update never implicitly restamps, extends, or clears it.
func (s *MemoryStore) upsertWithStaleAfter(ctx context.Context, id, kind, scope, title, content, subject, confidence, eventDate string, data map[string]mcell.DataEntry, tags []string, staleAfter *time.Time, opts ...UpsertOption) (UpsertResult, string) {
	if kind == "" || scope == "" || content == "" {
		return UpsertResult{}, "kind, scope, and content are required"
	}
	var o upsertOpts
	for _, opt := range opts {
		opt(&o)
	}

	// Bound item size — the store is an in-process map with no global cap, and
	// the MCP server bypasses the HTTP body limit (separate port). These caps
	// stop a single oversized upsert from exhausting pod memory.
	if len(title) > maxTitleChars {
		return UpsertResult{}, fmt.Sprintf("title exceeds %d characters", maxTitleChars)
	}
	if len(content) > maxContentChars {
		return UpsertResult{}, fmt.Sprintf("content exceeds %d characters", maxContentChars)
	}
	if img, ok := data["image"]; ok && len(img.Bytes) > maxImageBase64Chars {
		return UpsertResult{}, fmt.Sprintf("image exceeds %d bytes (base64)", maxImageBase64Chars)
	}

	// system_configuration is a singleton per subject — reject attempts to create
	// a second one, which would poison loadSessionConfig via random map iteration.
	if kind == "system_configuration" {
		return UpsertResult{}, "system_configuration items are managed via CR; use kubectl to update"
	}

	now := time.Now()

	// An update targeting an existing ID may hit a CR-backed item that this pod
	// has never written to s.items (e.g. a cell seeded by an earlier pod). Resolve
	// existence against the persisted store too, so the reported status and the
	// preserved CreatedAt/StaleAfter match storage rather than this pod's
	// ephemeral writes. getByID takes s.mu.RLock internally, so it must run
	// before we hold the write lock below (RWMutex is not reentrant). ADR-047.
	var existingCreatedAt time.Time
	var existingStaleAfter *time.Time
	var existingOrigin string
	var existingTags []string
	var existingEvidence, existingSupersedes []string
	var existingLinks []mcell.ThoughtLink
	prior := false
	merged := false
	if id != "" {
		if persisted := s.getByID(ctx, id); persisted != nil {
			prior = true
			existingCreatedAt = persisted.CreatedAt
			existingStaleAfter = persisted.StaleAfter
			existingOrigin = persisted.Origin
		}
	}

	incomingOrigin := writeOriginFrom(ctx)
	if incomingOrigin == "" {
		incomingOrigin = OriginDerived
	}

	// Write-side dedup: a restated fact under a new id lands on the cell it
	// repeats. An explicit update by id is never a dedup candidate.
	if !prior && !o.noDedup && dedupKinds[kind] && !isPrivateItem(kind, scope) && !s.noWriteDedup.Load() {
		probe := &MemoryItem{ID: id, Kind: kind, Scope: scope, Subject: subject, Title: title, Content: content, Origin: incomingOrigin}
		if match, _, ok := findNearDuplicate(s.FilterItems(subject, kind), probe); ok {
			id = match.ID
			prior, merged = true, true
			existingCreatedAt = match.CreatedAt
			existingStaleAfter = match.StaleAfter
			existingOrigin = match.Origin
			existingTags = match.Tags
			existingEvidence, existingSupersedes, existingLinks = match.EvidenceRefs, match.Supersedes, match.Links
		}
	}

	// A restatement re-affirms the cell it landed on, which renews a decaying
	// kind's deadline: a hypothesis that keeps being said stays.
	if merged && staleAfter == nil {
		if ttl, ok := defaultTTLByKind[kind]; ok {
			renewed := now.Add(ttl)
			staleAfter = &renewed
		}
	}

	s.mu.Lock()
	if id == "" {
		id = newID()
	}
	status := "created"
	it, exists := s.items[id]
	if exists || prior {
		status = "updated"
	}
	if merged {
		status = "merged"
	}
	if exists {
		existingOrigin = it.Origin
	}
	origin, originErr := resolveOrigin(existingOrigin, exists || prior, incomingOrigin)
	if originErr != "" {
		s.mu.Unlock()
		return UpsertResult{}, originErr
	}
	if exists {
		it.Kind = kind
		it.Scope = scope
		it.Title = title
		it.Content = content
		it.Subject = subject
		it.Confidence = confidence
		it.EventDate = eventDate
		it.Origin = origin
		if data != nil {
			it.Data = data
		}
		if tags != nil {
			it.Tags = tags
		}
		if merged {
			it.Tags = mergeTags(existingTags, tags)
		}
		if o.evidenceRefs != nil {
			it.EvidenceRefs = o.evidenceRefs
		}
		if o.supersedes != nil {
			it.Supersedes = mergeTags(it.Supersedes, o.supersedes)
		}
		it.UpdatedAt = now
		// Renewal is explicit only — an update never implicitly restamps,
		// extends, or clears StaleAfter; it.StaleAfter is left as-is unless the
		// caller passed a deadline of its own.
		if staleAfter != nil {
			it.StaleAfter = staleAfter
		}
	} else {
		createdAt := now
		if prior && !existingCreatedAt.IsZero() {
			createdAt = existingCreatedAt
		}
		if merged {
			tags = mergeTags(existingTags, tags)
		}
		it = &MemoryItem{
			ID:         id,
			Kind:       kind,
			Scope:      scope,
			Title:      title,
			Content:    content,
			Subject:    subject,
			Confidence: confidence,
			EventDate:  eventDate,
			Data:       data,
			Tags:       tags,
			Status:     "active",
			Origin:     origin,
			CreatedAt:  createdAt,
			UpdatedAt:  now,

			EvidenceRefs: o.evidenceRefs,
			Supersedes:   existingSupersedes,
			Links:        existingLinks,
		}
		if o.evidenceRefs == nil {
			it.EvidenceRefs = existingEvidence
		}
		if o.supersedes != nil {
			it.Supersedes = mergeTags(existingSupersedes, o.supersedes)
		}
		switch {
		case staleAfter != nil:
			// Explicit deadline always wins over any default.
			it.StaleAfter = staleAfter
		case prior:
			// Not a true create — a CR this pod hadn't cached locally. Preserve
			// its existing deadline rather than silently dropping it; stamping a
			// fresh default here would be an implicit restamp of an "update".
			it.StaleAfter = existingStaleAfter
		default:
			// True create with no explicit deadline: decay by default.
			if ttl, ok := defaultTTLByKind[kind]; ok {
				deadline := now.Add(ttl)
				it.StaleAfter = &deadline
			}
		}
		s.items[id] = it
	}
	s.mu.Unlock()

	if s.cells != nil && !isPrivateItem(kind, scope) {
		embedCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		start := time.Now()
		s.logger.Debug().Str("id", id).Str("title", title).Msg("embed start")
		_, err := s.cells.Index(embedCtx, itemToDoc(it))
		dur := time.Since(start)
		if err != nil {
			s.logger.Warn().Err(err).Str("id", id).Dur("dur", dur).Msg("embed failed")
		} else {
			s.logger.Debug().Str("id", id).Dur("dur", dur).Msg("embed ok")
			s.mu.Lock()
			it.Status = "indexed"
			s.mu.Unlock()
		}
	}

	// Private cells (scope=private, e.g. personal notes) and system_configuration
	// are never indexed for shared search nor broadcast on the shared SSE stream
	// (every authenticated client subscribes to it). isPrivateItem (ADR-037).
	//
	// Personal notes still need to reach their owner's UI live, so they are
	// published on the owner-scoped channel (keyed by the note's subject, which is
	// the userHash). system_configuration has no owner and is never surfaced, so it
	// is not published at all.
	switch {
	case !isPrivateItem(kind, scope):
		s.kwIdx.index(id, title, content)
		s.markSeen(it)
		s.hub.PublishChange(MemoryChangeEvent{Op: "upsert", Item: it})
	case isPrivateScope(scope) && subject != "":
		s.hub.PublishChangeTo(subject, MemoryChangeEvent{Op: "upsert", Item: it})
	}

	return UpsertResult{ID: id, Status: status, Item: it}, ""
}

// GetItem returns a memory item by ID, or nil if not found.
// Checks the cache first (for CR-backed items), then the ephemeral map.
func (s *MemoryStore) GetItem(id string) *MemoryItem {
	s.mu.RLock()
	r := s.reader
	ns := s.namespace
	s.mu.RUnlock()

	if r != nil {
		var cellList mcell.MemoryCellList
		if err := r.List(context.Background(), &cellList,
			client.InNamespace(ns),
			client.MatchingFields{"spec.id": id},
		); err == nil && len(cellList.Items) > 0 {
			return cellToItem(&cellList.Items[0])
		}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.items[id]
}

// Delete removes a memory item by ID. Returns false if not found in the ephemeral map.
// Always publishes a delete SSE event so CR-backed items (e.g. agent_note) are removed
// from the graph even when they are not present in s.items.
func (s *MemoryStore) Delete(id string) bool {
	s.mu.Lock()
	it, ok := s.items[id]
	var owner string // userHash of a private item, so its delete is owner-scoped
	delete(s.seen, id)
	if ok {
		if isPrivateScope(it.Scope) {
			owner = it.Subject
		}
		delete(s.items, id)
	}
	s.mu.Unlock()
	if ok {
		s.kwIdx.remove(id)
	}
	// The vector goes too, whether or not the item was held here: a CR-backed
	// item is not in s.items, and a vector left behind keeps taking top-k slots.
	if s.cells != nil {
		_ = s.cells.Delete(context.Background(), id)
	}
	// A private item's delete goes only to its owner — broadcasting even a bare ID
	// would tell every client a note existed and was removed. Shared items (and
	// CR-backed items not in s.items, which carry no content here) broadcast to all.
	if owner != "" {
		s.hub.PublishChangeTo(owner, MemoryChangeEvent{Op: "delete", ID: id})
	} else {
		s.hub.PublishChange(MemoryChangeEvent{Op: "delete", ID: id})
	}
	return ok
}

func itemToDoc(it *MemoryItem) vectorstore.MemoryCellIndexEntry {
	return vectorstore.MemoryCellIndexEntry{
		ID:         it.ID,
		Kind:       it.Kind,
		Scope:      it.Scope,
		Subject:    it.Subject,
		Title:      it.Title,
		Content:    it.Content,
		Confidence: it.Confidence,
		Tags:       it.Tags,
		Status:     it.Status,
		EventDate:  it.EventDate,
	}
}

// Get returns a single active memory item by ID, or nil if not found.
func (s *MemoryStore) Get(id string) *MemoryItem {
	it := s.GetItem(id)
	if it == nil || !isActive(it.Status) {
		return nil
	}
	return it
}

// List returns active items filtered by kind/scope/subject up to limit.
// Uses the cache when available; falls back to items map.
func (s *MemoryStore) List(kind, scope, subject string, limit int) []*MemoryItem {
	s.mu.RLock()
	r := s.reader
	ns := s.namespace
	s.mu.RUnlock()

	if r != nil {
		return s.listCache(context.Background(), r, ns, kind, scope, subject, limit)
	}
	return s.listMap(kind, scope, subject, limit)
}

// listRecentCap bounds the candidate set ListRecent fetches before sorting. The
// underlying List has no time ordering (map iteration / k8s list order), so we
// must fetch a generous superset, sort newest-first, then cap — capping at the
// source would drop the actually-newest items. Per-user episodic memory is far
// below this; if it is ever exceeded, the oldest beyond the cap are simply not
// considered, which is acceptable for a "most recent" query.
const listRecentCap = 500

// ListRecent returns active items matching kind/scope/subject sorted newest-first
// by CreatedAt (UpdatedAt breaks ties), capped to limit. Unlike List, the result
// is time-ordered — this is the recall path for episodic ("what happened last")
// memory, where "the latest creation time" is the selection criterion. ADR-020.
func (s *MemoryStore) ListRecent(kind, scope, subject string, limit int) []*MemoryItem {
	if limit <= 0 {
		return nil
	}
	all := s.List(kind, scope, subject, listRecentCap)
	// Forget expired episodic memories at read time (ADR-020): a stale memory must
	// not surface even before the reaper deletes its CR. Other kinds never expire.
	if kind == "episodic" {
		kept := all[:0]
		for _, it := range all {
			if !s.episodicExpired(it) {
				kept = append(kept, it)
			}
		}
		all = kept
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].CreatedAt.After(all[j].CreatedAt)
		}
		return all[i].UpdatedAt.After(all[j].UpdatedAt)
	})
	if len(all) > limit {
		all = all[:limit]
	}
	return all
}

// episodicExpired reports whether an episodic memory is older than episodicTTL.
// TTL<=0 disables expiry (nothing is ever expired). Age is measured from
// CreatedAt so a re-read (UpdatedAt bump) does not extend a memory's life.
func (s *MemoryStore) episodicExpired(it *MemoryItem) bool {
	if s.episodicTTL <= 0 || it.Kind != "episodic" {
		return false
	}
	return time.Since(it.CreatedAt) > s.episodicTTL
}

// StartEpisodicReaper runs a background sweep that deletes episodic memories
// older than episodicTTL (the "forget" mechanism, ADR-020). It mirrors
// StartStatsFlusher: one goroutine, a ticker, exits on ctx cancel. A no-op when
// TTL<=0 or every<=0. syncer removes the backing CR (nil → in-memory only).
func (s *MemoryStore) StartEpisodicReaper(ctx context.Context, syncer *CellSyncer, every time.Duration) {
	if s.episodicTTL <= 0 || every <= 0 {
		return
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.reapExpiredEpisodic(ctx, syncer)
			}
		}
	}()
}

// reapExpiredEpisodic deletes every episodic memory past its TTL. It lists
// episodic items via the reader when available (sees all CRs across pods,
// regardless of owner — this is an internal sweep, not a user query) and falls
// back to the in-process map. Each expired item is removed from memory and, when
// a syncer is present, from its CR.
func (s *MemoryStore) reapExpiredEpisodic(ctx context.Context, syncer *CellSyncer) {
	s.mu.RLock()
	r := s.reader
	ns := s.namespace
	s.mu.RUnlock()

	var expired []*MemoryItem
	if r != nil {
		var cellList mcell.MemoryCellList
		opts := appendFieldSelectors([]client.ListOption{client.InNamespace(ns)}, "episodic", "")
		if err := r.List(ctx, &cellList, opts...); err != nil {
			s.logger.Warn().Err(err).Msg("episodic reaper: list failed")
			return
		}
		for i := range cellList.Items {
			it := cellToItem(&cellList.Items[i])
			if s.episodicExpired(it) {
				expired = append(expired, it)
			}
		}
	} else {
		s.mu.RLock()
		for _, it := range s.items {
			if s.episodicExpired(it) {
				expired = append(expired, it)
			}
		}
		s.mu.RUnlock()
	}

	for _, it := range expired {
		s.Delete(it.ID)
		if syncer != nil {
			if err := syncer.Remove(ctx, it.ID, ns); err != nil {
				s.logger.Warn().Err(err).Str("id", it.ID).Msg("episodic reaper: CR remove failed")
			}
		}
	}
	if len(expired) > 0 {
		s.logger.Info().Int("count", len(expired)).Dur("ttl", s.episodicTTL).Msg("episodic reaper: forgot expired memories")
	}
}

// retentionExemptKinds are config/durable kinds the retention sweep never
// touches, regardless of StaleAfter: system_configuration and system_identity
// are structural (deployment/instance identity, not decaying knowledge),
// policy carries operator-tuned behavioral parameters, and episodic is owned
// by the separate episodic reaper (StartEpisodicReaper, which keys off
// CreatedAt+episodicTTL, not StaleAfter) — sweeping it here too would
// double-handle the same kind under two different expiry mechanisms.
var retentionExemptKinds = map[string]struct{}{
	"system_configuration": {},
	"system_identity":      {},
	"policy":               {},
	"episodic":             {},
}

// isRetentionExempt reports whether an item must never be removed by the
// retention sweep regardless of its StaleAfter deadline: a durable/config kind
// (retentionExemptKinds), or a map-carrying cell (a mental-map graph) —
// identified the same way CellSyncer.syncMapCell identifies one, by the
// presence of the "graph" Data blob (plus the kind=="mental_map" convention
// those cells are created under, belt and suspenders).
func isRetentionExempt(it *MemoryItem) bool {
	if _, ok := retentionExemptKinds[it.Kind]; ok {
		return true
	}
	if it.Kind == "mental_map" {
		return true
	}
	if _, ok := it.Data[graphDataKey]; ok {
		return true
	}
	return false
}

// isRetentionExpired reports whether an item's StaleAfter deadline has passed
// and it is not exempt from the retention sweep. An item with no StaleAfter
// set never expires.
func isRetentionExpired(it *MemoryItem, now time.Time) bool {
	if it.StaleAfter == nil || !it.StaleAfter.Before(now) {
		return false
	}
	return !isRetentionExempt(it)
}

// StartRetentionSweep runs a background sweep that hard-deletes memory items
// past their StaleAfter deadline — decay by default, the general counterpart
// to StartEpisodicReaper's episodic-only TTL. Mirrors StartEpisodicReaper's structure: one goroutine, a
// ticker, exits on ctx cancel. A no-op when every<=0.
func (s *MemoryStore) StartRetentionSweep(ctx context.Context, syncer *CellSyncer, every time.Duration) {
	if every <= 0 {
		return
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.sweepExpiredRetention(ctx, syncer)
			}
		}
	}()
}

// sweepExpiredRetention hard-deletes every item whose StaleAfter deadline is
// in the past, excluding exempt kinds (isRetentionExempt) and items with no
// deadline set. It lists ALL items via the reader when available (this is an
// internal sweep, not a user query — ignores scope/subject filtering just like
// reapExpiredEpisodic) and falls back to the in-process map. Each expired item
// goes through the same delete path a user delete uses — MemoryStore.Delete
// (which also fires the SSE change event) plus the CR delete via the syncer —
// so downstream behavior is uniform regardless of who initiated the delete.
func (s *MemoryStore) sweepExpiredRetention(ctx context.Context, syncer *CellSyncer) {
	s.mu.RLock()
	r := s.reader
	ns := s.namespace
	s.mu.RUnlock()

	now := time.Now()
	var expired []*MemoryItem
	if r != nil {
		var cellList mcell.MemoryCellList
		if err := r.List(ctx, &cellList, client.InNamespace(ns)); err != nil {
			s.logger.Warn().Err(err).Msg("retention sweep: list failed")
			return
		}
		for i := range cellList.Items {
			it := cellToItem(&cellList.Items[i])
			if isRetentionExpired(it, now) {
				expired = append(expired, it)
			}
		}
	} else {
		s.mu.RLock()
		for _, it := range s.items {
			if isRetentionExpired(it, now) {
				expired = append(expired, it)
			}
		}
		s.mu.RUnlock()
	}

	if len(expired) == 0 {
		return
	}

	byKind := make(map[string]int, len(expired))
	for _, it := range expired {
		s.Delete(it.ID)
		if syncer != nil {
			if err := syncer.Remove(ctx, it.ID, ns); err != nil {
				s.logger.Warn().Err(err).Str("id", it.ID).Msg("retention sweep: CR remove failed")
			}
		}
		byKind[it.Kind]++
	}
	s.logger.Info().Int("count", len(expired)).Interface("byKind", byKind).Msg("retention sweep: expired items removed")
}

func (s *MemoryStore) listCache(ctx context.Context, r client.Reader, ns, kind, scope, subject string, limit int) []*MemoryItem {
	listOpts := []client.ListOption{client.InNamespace(ns)}
	listOpts = appendFieldSelectors(listOpts, kind, subject)

	var cellList mcell.MemoryCellList
	if err := r.List(ctx, &cellList, listOpts...); err != nil {
		s.logger.Warn().Err(err).Msg("cache list failed — falling back to map")
		return s.listMap(kind, scope, subject, limit)
	}

	seen := make(map[string]struct{})
	var out []*MemoryItem
	for i := range cellList.Items {
		cell := &cellList.Items[i]
		if !isActive(cell.Spec.Status) {
			continue
		}
		if isPrivateItem(cell.Spec.Kind, cell.Spec.Scope) && subject == "" {
			continue
		}
		if scope != "" && cell.Spec.Scope != scope {
			continue
		}
		seen[cell.Spec.ID] = struct{}{}
		out = append(out, cellToItem(cell))
		if len(out) >= limit {
			return out
		}
	}

	// Merge ephemeral items.
	s.mu.RLock()
	for _, it := range s.items {
		if len(out) >= limit {
			break
		}
		if _, ok := seen[it.ID]; ok {
			continue
		}
		if !isActive(it.Status) {
			continue
		}
		if isPrivateItem(it.Kind, it.Scope) && subject == "" {
			continue
		}
		if kind != "" && it.Kind != kind {
			continue
		}
		if scope != "" && it.Scope != scope {
			continue
		}
		if subject != "" && it.Subject != subject {
			continue
		}
		out = append(out, it)
	}
	s.mu.RUnlock()
	return out
}

func (s *MemoryStore) listMap(kind, scope, subject string, limit int) []*MemoryItem {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*MemoryItem
	for _, it := range s.items {
		if !isActive(it.Status) {
			continue
		}
		if isPrivateItem(it.Kind, it.Scope) && subject == "" {
			continue
		}
		if kind != "" && it.Kind != kind {
			continue
		}
		if scope != "" && it.Scope != scope {
			continue
		}
		if subject != "" && it.Subject != subject {
			continue
		}
		out = append(out, it)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// MemoryItemSummary is a lightweight view of a MemoryItem — no content or image data.
type MemoryItemSummary struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Scope     string    `json:"scope"`
	Subject   string    `json:"subject,omitempty"`
	Title     string    `json:"title"`
	Tags      []string  `json:"tags,omitempty"`
	Status    string    `json:"status"`
	UpdatedAt time.Time `json:"updatedAt"`
	HitCount  int64     `json:"hitCount"`
}

// ListSummary returns lightweight summaries of active items filtered by kind/scope/subject.
// Total is the count of all matching active items (ignoring limit).
func (s *MemoryStore) ListSummary(kind, scope, subject string, limit int) (items []MemoryItemSummary, total int) {
	s.mu.RLock()
	r := s.reader
	ns := s.namespace
	s.mu.RUnlock()

	if r != nil {
		return s.listSummaryCache(context.Background(), r, ns, kind, scope, subject, limit)
	}
	return s.listSummaryMap(kind, scope, subject, limit)
}

// ListSummaryPerKind returns a bounded graph preview: up to perKind active
// items for EACH kind (not a global cap), so every category is represented on
// the graph even when one kind dwarfs the rest. total is the true count of all
// matching active items, independent of perKind — the UI shows it as the real
// store size while rendering only the preview. perKind <= 0 means no per-kind
// cap (return everything). Private/system_configuration cells are excluded (the
// shared graph is public-only); scope/subject filters still apply.
func (s *MemoryStore) ListSummaryPerKind(perKind int) (items []MemoryItemSummary, total int, totalsByKind map[string]int) {
	// Pull the full active, public set once (limit high enough to be effectively
	// unbounded), then bucket by kind and cap each bucket.
	all, _ := s.ListSummary("", "", "", 1<<30)
	all = slices.DeleteFunc(all, func(it MemoryItemSummary) bool {
		return isPrivateItem(it.Kind, it.Scope)
	})
	// True per-kind totals over the full (post-privacy-filter) set — the UI shows
	// these as the real category counts, independent of the per-kind render cap.
	totalsByKind = make(map[string]int, 16)
	for _, it := range all {
		totalsByKind[it.Kind]++
	}
	total = len(all)
	if perKind <= 0 {
		return all, total, totalsByKind
	}
	perKindCount := make(map[string]int, 16)
	for _, it := range all {
		if perKindCount[it.Kind] >= perKind {
			continue
		}
		perKindCount[it.Kind]++
		items = append(items, it)
	}
	return items, total, totalsByKind
}

func (s *MemoryStore) listSummaryCache(ctx context.Context, r client.Reader, ns, kind, scope, subject string, limit int) (items []MemoryItemSummary, total int) {
	listOpts := []client.ListOption{client.InNamespace(ns)}
	listOpts = appendFieldSelectors(listOpts, kind, subject)

	var cellList mcell.MemoryCellList
	if err := r.List(ctx, &cellList, listOpts...); err != nil {
		s.logger.Warn().Err(err).Msg("cache list failed — falling back to map")
		return s.listSummaryMap(kind, scope, subject, limit)
	}

	seen := make(map[string]struct{})
	for i := range cellList.Items {
		cell := &cellList.Items[i]
		if !isActive(cell.Spec.Status) {
			continue
		}
		if isPrivateItem(cell.Spec.Kind, cell.Spec.Scope) && subject == "" {
			continue
		}
		if scope != "" && cell.Spec.Scope != scope {
			continue
		}
		seen[cell.Spec.ID] = struct{}{}
		total++
		if len(items) < limit {
			items = append(items, MemoryItemSummary{
				ID:        cell.Spec.ID,
				Kind:      cell.Spec.Kind,
				Scope:     cell.Spec.Scope,
				Subject:   cell.Spec.Subject,
				Title:     cell.Spec.Title,
				Tags:      cell.Spec.Tags,
				Status:    cell.Spec.Status,
				UpdatedAt: cell.Spec.UpdatedAt.Time,
				HitCount:  s.hub.HitCount(cell.Spec.ID),
			})
		}
	}

	// Merge ephemeral items.
	s.mu.RLock()
	for _, it := range s.items {
		if _, ok := seen[it.ID]; ok {
			continue
		}
		if !isActive(it.Status) {
			continue
		}
		if isPrivateItem(it.Kind, it.Scope) && subject == "" {
			continue
		}
		if kind != "" && it.Kind != kind {
			continue
		}
		if scope != "" && it.Scope != scope {
			continue
		}
		if subject != "" && it.Subject != subject {
			continue
		}
		total++
		if len(items) < limit {
			items = append(items, MemoryItemSummary{
				ID:        it.ID,
				Kind:      it.Kind,
				Scope:     it.Scope,
				Subject:   it.Subject,
				Title:     it.Title,
				Tags:      it.Tags,
				Status:    it.Status,
				UpdatedAt: it.UpdatedAt,
				HitCount:  s.hub.HitCount(it.ID),
			})
		}
	}
	s.mu.RUnlock()
	return items, total
}

func (s *MemoryStore) listSummaryMap(kind, scope, subject string, limit int) (items []MemoryItemSummary, total int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, it := range s.items {
		if !isActive(it.Status) {
			continue
		}
		if isPrivateItem(it.Kind, it.Scope) && subject == "" {
			continue
		}
		if kind != "" && it.Kind != kind {
			continue
		}
		if scope != "" && it.Scope != scope {
			continue
		}
		if subject != "" && it.Subject != subject {
			continue
		}
		total++
		if len(items) < limit {
			items = append(items, MemoryItemSummary{
				ID:        it.ID,
				Kind:      it.Kind,
				Scope:     it.Scope,
				Subject:   it.Subject,
				Title:     it.Title,
				Tags:      it.Tags,
				Status:    it.Status,
				UpdatedAt: it.UpdatedAt,
				HitCount:  s.hub.HitCount(it.ID),
			})
		}
	}
	return items, total
}

// PublishHit broadcasts a memory search hit event to SSE subscribers. kinds
// must be parallel to itemIDs (same order, same length) — see
// MemoryHitEvent.Kinds for why: it's what lets the UI flash a hit's category
// region even when the hit item itself isn't one of the currently-rendered
// graph nodes.
func (s *MemoryStore) PublishHit(itemIDs []string, query, kind string, kinds []string) {
	s.hub.Publish(MemoryHitEvent{ItemIDs: itemIDs, Query: query, Kind: kind, Kinds: kinds})
}

// RestoreStats loads persisted counters from a RuntimeStats snapshot (e.g. read by ConfigWatcher at startup).
// voiceActive is intentionally not restored — it is session-local and always starts at 0.
func (s *MemoryStore) RestoreStats(st *mcell.RuntimeStats) {
	if st == nil {
		return
	}
	s.counters.total.Store(st.SearchTotal)
	s.counters.kwHits.Store(st.KeywordHits)
	s.counters.kwVoiceHits.Store(st.KWVoiceHits)
	s.counters.kwVoiceMisses.Store(st.KWVoiceMisses)
	s.counters.kwTrigramHits.Store(st.KWTrigramHits)
	s.counters.semCalls.Store(st.SemanticCalls)
	s.counters.semFallbacks.Store(st.SemanticFallbacks)
	s.counters.voiceTotal.Store(st.VoiceTotal)

	// Rehydrate per-user daily token spend for the current UTC day. This runs on
	// every system_configuration watch event, not just at startup — and flushStats
	// writes that CR, so each flush echoes back here. We must NOT clobber the live
	// in-memory map: a stale or self-echoed CR snapshot would otherwise lower a
	// counter the running session has already advanced (observed as the HUD
	// dropping mid-count then recovering). Merge by max so restore only ever fills
	// in or raises a user's count, never lowers it; a genuine restart starts from
	// an empty map and rehydrates fully.
	today := utcDay()
	s.usageMu.Lock()
	for _, ud := range st.UserDaily {
		if ud.UserHash == "" || ud.Day != today {
			continue
		}
		if u, ok := s.usage[ud.UserHash]; ok && u.day == today {
			if ud.Tokens > u.tokens {
				u.tokens = ud.Tokens
			}
			continue
		}
		s.usage[ud.UserHash] = &userDaily{day: ud.Day, tokens: ud.Tokens}
	}
	s.usageMu.Unlock()

	// Rehydrate persisted sessions so logins survive a restart and propagate across
	// replicas via this same watch event. restoreSessions merges in (never clobbers
	// a live entry) and prunes expired rows, mirroring the UserDaily merge above.
	if r := sessions.restoreSessions(st.Sessions); r.added > 0 {
		s.logger.Info().Int("sessions", r.added).Int("rows", len(st.Sessions)).Msg("restored persisted sessions from CR")
	} else if r.undecryptable > 0 {
		// Only a genuine decrypt failure is worth a warning — a key mismatch means
		// these sessions can never restore (e.g. rows written under a different
		// OAuth-secret-derived key). added==0 with only alreadyHeld rows is the
		// normal self-echo from our own flush and is silent.
		s.logger.Warn().Int("undecryptable", r.undecryptable).Int("rows", len(st.Sessions)).Msg("CR session rows could not be decrypted (key mismatch) — those logins will require re-auth")
	}

	// Rehydrate MCP OAuth client registrations (ADR-049) so a client registered
	// before a restart (e.g. Claude Code) stays known — otherwise /authorize rejects
	// its cached client_id and forces a re-register.
	if n := oauthClients.restore(st.OAuthClients); n > 0 {
		s.logger.Info().Int("clients", n).Msg("restored persisted MCP OAuth clients from CR")
	}
}

// AddUserTokens accumulates Gemini Live token spend for an authenticated user on
// the current UTC day. Empty userHash (unauthenticated/dev sessions) is ignored —
// there is no stable key to meter. Rolls the counter over when the UTC day changes.
func (s *MemoryStore) AddUserTokens(userHash string, n int64) {
	if userHash == "" || n <= 0 {
		return
	}
	today := utcDay()
	s.usageMu.Lock()
	u, ok := s.usage[userHash]
	if !ok {
		u = &userDaily{day: today}
		s.usage[userHash] = u
	}
	if u.day != today {
		u.day = today
		u.tokens = 0
	}
	u.tokens += n
	s.usageMu.Unlock()
}

// UserTokensToday returns the user's token spend for the current UTC day, or 0 if
// the stored counter is from a previous day (not yet rolled over) or the user is
// unknown.
func (s *MemoryStore) UserTokensToday(userHash string) int64 {
	if userHash == "" {
		return 0
	}
	today := utcDay()
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	u, ok := s.usage[userHash]
	if !ok || u.day != today {
		return 0
	}
	return u.tokens
}

// snapshotUserDaily returns the per-user token rows for the current UTC day for
// persistence to CR status, pruning rows from past days from the in-memory map so
// it stays bounded by active users. Returns nil when there is nothing to persist.
func (s *MemoryStore) snapshotUserDaily() []mcell.UserDailyTokens {
	today := utcDay()
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	var rows []mcell.UserDailyTokens
	for hash, u := range s.usage {
		if u.day != today {
			delete(s.usage, hash)
			continue
		}
		rows = append(rows, mcell.UserDailyTokens{UserHash: hash, Day: u.day, Tokens: u.tokens})
	}
	return rows
}

// TrackToolCall records a tool invocation and its execution duration.
func (s *MemoryStore) TrackToolCall(name string, d time.Duration) {
	s.toolMu.Lock()
	st, ok := s.toolStats[name]
	if !ok {
		st = &toolCallStat{}
		s.toolStats[name] = st
	}
	st.calls++
	st.totalNs += d.Nanoseconds()
	s.toolMu.Unlock()
}

// ToolMetrics returns per-tool call counts and average latency in milliseconds.
func (s *MemoryStore) ToolMetrics() map[string]any {
	s.toolMu.Lock()
	out := make(map[string]any, len(s.toolStats))
	for name, st := range s.toolStats {
		avgUs := float64(0)
		if st.calls > 0 {
			avgUs = float64(st.totalNs) / float64(st.calls) / 1e3
		}
		out[name] = map[string]any{
			"calls":  st.calls,
			"avg_us": avgUs,
		}
	}
	s.toolMu.Unlock()
	return out
}

// TrackKWTrigramHit records that the trigram index contributed at least one candidate to a search result.
func (s *MemoryStore) TrackKWTrigramHit() { s.counters.kwTrigramHits.Add(1) }

// TrackKWVoiceHit records that the keyword-first path in the voice search_memory tool
// returned enough results to skip semantic search.
func (s *MemoryStore) TrackKWVoiceHit() { s.counters.kwVoiceHits.Add(1) }

// TrackKWVoiceMiss records that keyword-first returned too few results and fell through
// to semantic search.
func (s *MemoryStore) TrackKWVoiceMiss() { s.counters.kwVoiceMisses.Add(1) }

// Metrics returns a snapshot of search operation counters.
func (s *MemoryStore) Metrics() SearchMetrics {
	return SearchMetrics{
		TotalSearches:     s.counters.total.Load(),
		KeywordHits:       s.counters.kwHits.Load(),
		KWVoiceHits:       s.counters.kwVoiceHits.Load(),
		KWVoiceMisses:     s.counters.kwVoiceMisses.Load(),
		KWTrigramHits:     s.counters.kwTrigramHits.Load(),
		SemanticCalls:     s.counters.semCalls.Load(),
		SemanticFallbacks: s.counters.semFallbacks.Load(),
	}
}

// VoiceMetrics returns a snapshot of voice session counters.
func (s *MemoryStore) VoiceMetrics() map[string]any {
	return map[string]any{
		"totalCalls":  s.counters.voiceTotal.Load(),
		"activeCalls": s.counters.voiceActive.Load(),
	}
}

// TrackVoiceCall records a new voice session start and marks stats dirty.
func (s *MemoryStore) TrackVoiceCall() {
	s.counters.voiceTotal.Add(1)
	s.counters.voiceActive.Add(1)
	s.markStatsDirty()
}

// TrackVoiceEnd records a voice session end and marks stats dirty.
func (s *MemoryStore) TrackVoiceEnd() {
	s.counters.voiceActive.Add(-1)
	s.markStatsDirty()
}

// markStatsDirty marks the persisted stats/session snapshot as stale without
// touching the apiserver. The background flusher (StartStatsFlusher) coalesces
// these into at most one CR write per tick. Cheap and non-blocking — safe to call
// on the hot path (every search, voice event, login/logout).
func (s *MemoryStore) markStatsDirty() { s.statsDirty.Store(true) }

// FlushStats marks stats dirty so the next flush tick persists them. Exported so
// the session store can request a flush on login/logout (wired via
// SetSessionPersistence). It no longer writes synchronously — see ADR-047
// (debounced stats flush). The change lands on the next StartStatsFlusher tick.
func (s *MemoryStore) FlushStats() { s.markStatsDirty() }

// StartStatsFlusher runs the single CR-status writer: every `every` it writes the
// current counters + sessions to the system_configuration CR, but only when
// something changed since the last write (markStatsDirty). This replaces the old
// per-request synchronous flush, so search/voice/login throughput is no longer
// gated on an etcd round-trip and N concurrent requests cause at most one write
// per tick. On ctx cancellation it does a final flush so the last state is durable
// across a graceful shutdown. Call once at startup after SetWriter. A zero or
// negative interval disables the loop (in-memory only).
func (s *MemoryStore) StartStatsFlusher(ctx context.Context, every time.Duration) {
	if every <= 0 {
		return
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				// Final flush so a login/voice/search in the last window survives.
				if s.statsDirty.Swap(false) {
					s.flushStats()
				}
				return
			case <-t.C:
				// Only write when something changed — idle pods make no etcd writes.
				if s.statsDirty.Swap(false) {
					s.flushStats()
				}
			}
		}
	}()
}

// flushStats writes runtime counters to the status of the system_configuration CR.
func (s *MemoryStore) flushStats() {
	s.mu.RLock()
	w := s.writer
	ns := s.namespace
	r := s.reader
	s.mu.RUnlock()
	if w == nil || r == nil {
		return
	}

	var embedCalls, embedCacheHits int64
	if s.embedder != nil {
		m := s.embedder.Metrics()
		embedCalls = m.EmbedCalls
		embedCacheHits = m.CacheHits
	}

	stats := &mcell.RuntimeStats{
		MemoryCells:       int64(s.Count()),
		VoiceTotal:        s.counters.voiceTotal.Load(),
		VoiceActive:       s.counters.voiceActive.Load(),
		SearchTotal:       s.counters.total.Load(),
		KeywordHits:       s.counters.kwHits.Load(),
		KWVoiceHits:       s.counters.kwVoiceHits.Load(),
		KWVoiceMisses:     s.counters.kwVoiceMisses.Load(),
		KWTrigramHits:     s.counters.kwTrigramHits.Load(),
		SemanticCalls:     s.counters.semCalls.Load(),
		SemanticFallbacks: s.counters.semFallbacks.Load(),
		EmbedCalls:        embedCalls,
		EmbedCacheHits:    embedCacheHits,
		UpdatedAt:         time.Now().UTC().Format(time.RFC3339),
		UserDaily:         s.snapshotUserDaily(),
		Sessions:          sessions.snapshotSessions(),
		OAuthClients:      oauthClients.snapshot(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cell := &mcell.MemoryCell{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: SystemConfigCRName}, cell); err != nil {
		return
	}
	cell = cell.DeepCopy()
	cell.Status.Stats = stats
	if err := w.Status().Update(ctx, cell); err != nil {
		s.logger.Warn().Err(err).Msg("flushStats: status update failed")
	}
}

// Count returns the number of active items visible to the store.
func (s *MemoryStore) Count() int {
	s.mu.RLock()
	r := s.reader
	ns := s.namespace
	s.mu.RUnlock()

	if r != nil {
		var cellList mcell.MemoryCellList
		if err := r.List(context.Background(), &cellList, client.InNamespace(ns)); err == nil {
			n := 0
			for i := range cellList.Items {
				if isActive(cellList.Items[i].Spec.Status) {
					n++
				}
			}
			return n
		}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, it := range s.items {
		if isActive(it.Status) {
			n++
		}
	}
	return n
}

// EmbedMetrics returns embedding cache/call counters, or zero if no embedder configured.
func (s *MemoryStore) EmbedMetrics() vectorstore.EmbedMetrics {
	if s.embedder == nil {
		return vectorstore.EmbedMetrics{}
	}
	return s.embedder.Metrics()
}

// memoryNeighborEdgesPerNeighbor is how many neighbour-to-neighbour links each
// neighbour may contribute to a Neighborhood.
const memoryNeighborEdgesPerNeighbor = 3

// MemoryNeighbor is one neighbour of a cell, without its content.
type MemoryNeighbor struct {
	ID     string   `json:"id"`
	Kind   string   `json:"kind"`
	Title  string   `json:"title"`
	Status string   `json:"status"`
	Tags   []string `json:"tags,omitempty"`
	Score  float32  `json:"score"`
}

// MemoryNeighborEdge links two neighbours that are tighter to each other than to
// the centre.
type MemoryNeighborEdge struct {
	Source string  `json:"source"`
	Target string  `json:"target"`
	Score  float32 `json:"score"`
}

// MemoryNeighborhood is a cell's semantic neighbourhood. Indexed is false when
// the cell has no embedding (keyword-only mode, a private cell, or not embedded
// yet), which is different from an indexed cell with no neighbours.
type MemoryNeighborhood struct {
	ID        string               `json:"id"`
	Indexed   bool                 `json:"indexed"`
	Neighbors []MemoryNeighbor     `json:"neighbors"`
	Edges     []MemoryNeighborEdge `json:"edges"`
}

// Neighborhood returns the semantic k-NN neighbourhood of the cell id from the
// embedding index (top is clamped to 1..50, default 20). Only shared, active
// neighbours appear: a private cell is never embedded, and the filter keeps one
// out should it ever be.
func (s *MemoryStore) Neighborhood(id string, top int) *MemoryNeighborhood {
	out := &MemoryNeighborhood{ID: id, Neighbors: []MemoryNeighbor{}, Edges: []MemoryNeighborEdge{}}
	if s.cells == nil {
		return out
	}
	if top <= 0 {
		top = 20
	}
	if top > 50 {
		top = 50
	}
	nh, ok := s.cells.Neighborhood(id, top, memoryNeighborEdgesPerNeighbor, func(c vectorstore.MemoryCellIndexEntry) bool {
		return isActive(c.Status) && !isPrivateItem(c.Kind, c.Scope)
	})
	if !ok {
		return out
	}
	out.Indexed = true
	for _, n := range nh.Neighbors {
		out.Neighbors = append(out.Neighbors, MemoryNeighbor{ID: n.Entry.ID, Kind: n.Entry.Kind, Title: n.Entry.Title, Status: n.Entry.Status, Tags: n.Entry.Tags, Score: n.Score})
	}
	for _, e := range nh.Edges {
		out.Edges = append(out.Edges, MemoryNeighborEdge{Source: e.A, Target: e.B, Score: e.Score})
	}
	return out
}

// AllItems returns all active memory items for the given subject.
// Used by get_knowledge_context to pre-load full context at session start.
func (s *MemoryStore) AllItems(subject string) []*MemoryItem {
	s.mu.RLock()
	r := s.reader
	ns := s.namespace
	s.mu.RUnlock()

	if r != nil {
		return s.allItemsCache(context.Background(), r, ns, subject)
	}
	return s.allItemsMap(subject)
}

// FilterItems returns active memory items filtered by subject and/or kind.
// Unlike AllItems, private notes are included when kind="note".
func (s *MemoryStore) FilterItems(subject, kind string) []*MemoryItem {
	s.mu.RLock()
	r := s.reader
	ns := s.namespace
	s.mu.RUnlock()

	if r != nil {
		return s.filterItemsCache(context.Background(), r, ns, subject, kind)
	}
	return s.filterItemsMap(subject, kind)
}

func (s *MemoryStore) allItemsCache(ctx context.Context, r client.Reader, ns, subject string) []*MemoryItem {
	listOpts := []client.ListOption{client.InNamespace(ns)}
	if subject != "" {
		listOpts = append(listOpts, client.MatchingFields{"spec.subject": subject})
	}
	var cellList mcell.MemoryCellList
	if err := r.List(ctx, &cellList, listOpts...); err != nil {
		s.logger.Warn().Err(err).Msg("cache list failed — falling back to map")
		return s.allItemsMap(subject)
	}
	seen := make(map[string]struct{})
	var out []*MemoryItem
	for i := range cellList.Items {
		cell := &cellList.Items[i]
		if !isActive(cell.Spec.Status) {
			continue
		}
		if isPrivateItem(cell.Spec.Kind, cell.Spec.Scope) {
			continue
		}
		seen[cell.Spec.ID] = struct{}{}
		out = append(out, cellToItem(cell))
	}
	s.mu.RLock()
	for _, it := range s.items {
		if _, ok := seen[it.ID]; ok {
			continue
		}
		if !isActive(it.Status) || isPrivateItem(it.Kind, it.Scope) {
			continue
		}
		if subject != "" && it.Subject != subject {
			continue
		}
		out = append(out, it)
	}
	s.mu.RUnlock()
	return out
}

func (s *MemoryStore) allItemsMap(subject string) []*MemoryItem {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*MemoryItem
	for _, it := range s.items {
		if !isActive(it.Status) {
			continue
		}
		if isPrivateItem(it.Kind, it.Scope) {
			continue
		}
		if subject != "" && it.Subject != subject {
			continue
		}
		out = append(out, it)
	}
	return out
}

func (s *MemoryStore) filterItemsCache(ctx context.Context, r client.Reader, ns, subject, kind string) []*MemoryItem {
	listOpts := []client.ListOption{client.InNamespace(ns)}
	if subject != "" {
		listOpts = append(listOpts, client.MatchingFields{"spec.subject": subject})
	}
	var cellList mcell.MemoryCellList
	if err := r.List(ctx, &cellList, listOpts...); err != nil {
		s.logger.Warn().Err(err).Msg("cache list failed — falling back to map")
		return s.filterItemsMap(subject, kind)
	}
	seen := make(map[string]struct{})
	var out []*MemoryItem
	for i := range cellList.Items {
		cell := &cellList.Items[i]
		if !isActive(cell.Spec.Status) || cell.Spec.Kind == "system_configuration" {
			continue
		}
		if kind != "" && cell.Spec.Kind != kind {
			continue
		}
		seen[cell.Spec.ID] = struct{}{}
		out = append(out, cellToItem(cell))
	}
	s.mu.RLock()
	for _, it := range s.items {
		if _, ok := seen[it.ID]; ok {
			continue
		}
		if !isActive(it.Status) || it.Kind == "system_configuration" {
			continue
		}
		if kind != "" && it.Kind != kind {
			continue
		}
		if subject != "" && it.Subject != subject {
			continue
		}
		out = append(out, it)
	}
	s.mu.RUnlock()
	return out
}

func (s *MemoryStore) filterItemsMap(subject, kind string) []*MemoryItem {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*MemoryItem
	for _, it := range s.items {
		if !isActive(it.Status) || it.Kind == "system_configuration" {
			continue
		}
		if kind != "" && it.Kind != kind {
			continue
		}
		if subject != "" && it.Subject != subject {
			continue
		}
		out = append(out, it)
	}
	return out
}

// KnowledgeIndex returns a compact list of titles available for the given subject.
func (s *MemoryStore) KnowledgeIndex(subject string) []string {
	items := s.AllItems(subject)
	seen := make(map[string]struct{}, len(items))
	var titles []string
	for _, it := range items {
		if _, ok := seen[it.Title]; !ok {
			seen[it.Title] = struct{}{}
			titles = append(titles, it.Title)
		}
	}
	return titles
}

// getByID returns a MemoryItem by ID, checking cache then the ephemeral map.
func (s *MemoryStore) getByID(ctx context.Context, id string) *MemoryItem {
	s.mu.RLock()
	r := s.reader
	ns := s.namespace
	s.mu.RUnlock()

	if r != nil {
		var cellList mcell.MemoryCellList
		if err := r.List(ctx, &cellList,
			client.InNamespace(ns),
			client.MatchingFields{"spec.id": id},
		); err == nil && len(cellList.Items) > 0 {
			return cellToItem(&cellList.Items[0])
		}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.items[id]
}

// metaTimeToTimePtr converts a MemoryCellSpec's optional metav1.Time pointer
// (StaleAfter) to the plain *time.Time MemoryItem carries. nil in, nil out.
func metaTimeToTimePtr(t *metav1.Time) *time.Time {
	if t == nil {
		return nil
	}
	tt := t.Time
	return &tt
}

// timePtrToMetaTime is the reverse of metaTimeToTimePtr, used when writing a
// MemoryItem's StaleAfter back to a MemoryCellSpec (CellSyncer.Persist). nil
// in, nil out.
func timePtrToMetaTime(t *time.Time) *metav1.Time {
	if t == nil {
		return nil
	}
	mt := metav1.NewTime(*t)
	return &mt
}

// cellToItem converts an AwarenessMemoryCell CR to a MemoryItem for API responses.
func cellToItem(cell *mcell.MemoryCell) *MemoryItem {
	return &MemoryItem{
		ID:         cell.Spec.ID,
		Kind:       cell.Spec.Kind,
		Scope:      cell.Spec.Scope,
		Subject:    cell.Spec.Subject,
		Title:      cell.Spec.Title,
		Content:    cell.Spec.Content,
		Confidence: cell.Spec.Confidence,
		Tags:       cell.Spec.Tags,
		Status:     cell.Spec.Status,
		EventDate:  cell.Spec.EventDate,
		Data:       cell.Spec.Data,
		StaleAfter: metaTimeToTimePtr(cell.Spec.StaleAfter),
		CreatedAt:  cell.Spec.CreatedAt.Time,
		UpdatedAt:  cell.Spec.UpdatedAt.Time,
		Links:      cell.Spec.Links,

		Origin:       effectiveOrigin(cell.Spec.Origin),
		EvidenceRefs: cell.Spec.EvidenceRefs,
		Supersedes:   cell.Spec.Supersedes,
	}
}

// itemsSemanticallyEqual reports whether two memory items carry the same
// user-visible content — every field except CreatedAt/UpdatedAt (the
// timestamps a caller mirroring a CR into memory is trying not to churn).
// MemoryItem never carries an embedding (that lives only on the backing CR's
// Spec.Embedding — see cellToItem), so an embedding-only CR diff is already
// excluded from this comparison without special-casing it. Tags/Data compare
// nil and empty as equal (slices.Equal/maps.Equal); StaleAfter compares by
// value through the pointer (nil vs non-nil is a real difference). ADR-033
// discipline, ported from the sibling triage repo's
// memorySpecsEqualIgnoringEmbedding.
func itemsSemanticallyEqual(a, b *MemoryItem) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.ID == b.ID &&
		a.Kind == b.Kind &&
		a.Scope == b.Scope &&
		a.Subject == b.Subject &&
		a.Title == b.Title &&
		a.Content == b.Content &&
		a.Confidence == b.Confidence &&
		a.EventDate == b.EventDate &&
		a.Status == b.Status &&
		a.Origin == b.Origin &&
		slices.Equal(a.Tags, b.Tags) &&
		slices.Equal(a.EvidenceRefs, b.EvidenceRefs) &&
		slices.Equal(a.Supersedes, b.Supersedes) &&
		maps.Equal(a.Data, b.Data) &&
		timePtrEqual(a.StaleAfter, b.StaleAfter)
}

// timePtrEqual reports whether two optional timestamps are equal: both nil,
// or both non-nil and instant-equal (time.Time.Equal, not ==, since two equal
// instants can differ in monotonic reading/location).
func timePtrEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func isActive(status string) bool {
	return status == "active" || status == "indexed"
}

// Scope is a two-value axis (ADR-037): scope answers "private or shared?",
// subject answers "whose?". A private cell is isolated to its subject — a
// userHash for a person, or the reserved owners "agent"/"system". A public cell
// is shared knowledge, visible on the graph and in user-facing search.
const (
	scopePrivate = "private" // isolated; subject identifies the owner
	scopePublic  = "public"  // shared; visible to everyone
)

// isPrivateScope reports whether a scope is the private (isolated) tier.
func isPrivateScope(scope string) bool { return scope == scopePrivate }

// isPrivateItem is the single privacy predicate every guard keys off — replacing
// the former `kind == "agent_note"` checks. A cell is private if scope=="private"
// or it is the reserved system_configuration kind (internal config, never shared
// regardless of scope).
func isPrivateItem(kind, scope string) bool {
	return isPrivateScope(scope) || kind == "system_configuration"
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b) //nolint:errcheck
	return hex.EncodeToString(b)
}
