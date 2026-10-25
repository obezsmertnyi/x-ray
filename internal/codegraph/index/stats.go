package index

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/embedder"
)

// SearchStats accumulates process-lifetime metrics for the hybrid
// (semantic + lexical) search path, so retrieval quality can be judged from
// live traffic instead of only from offline ground-truth runs
// (docs/todo/corpus-map-router.md's calibration work covers the latter; this
// is the former).
//
// One instance is meant to be SHARED across every *Index that searches the
// same logical map over the server's whole lifetime. In Mode B a
// project's *Index is torn down and rebuilt on every evict/reload cycle
// (engine.Registry.load creates a fresh *Indexer, hence a fresh *Index, each
// time a project is revived from its snapshot) — counters stored ONLY on the
// *Index struct itself would silently reset to zero on every eviction, which
// is not "process-lifetime cumulative". engine.Registry owns exactly one
// SearchStats for its whole process lifetime and injects it into every
// *Index it builds via Index.UseStats, called once, before the Index is
// published to any concurrent reader. An Index that never receives an
// injected instance keeps the private one New creates for it — still
// correct, just not shared (this is what Mode A's single long-lived Index
// gets, which is fine: nothing recreates it mid-process there).
//
// Every simple counter is an atomic — increment-only on the hot path, no
// allocation. The latency fields need a compound update (sum + count + max
// together) so they are guarded by a small mutex instead; the critical
// section is a handful of integer ops, not a source of contention at this
// server's request rate.
type SearchStats struct {
	searches           atomic.Int64
	semanticCalls      atomic.Int64
	lexicalAbstentions atomic.Int64
	coverageRejections atomic.Int64
	keywordPromotions  atomic.Int64
	semanticOnlyHits   atomic.Int64
	bothChannelHits    atomic.Int64

	latMu      sync.Mutex
	latCount   int64
	embedSumNs int64
	embedMaxNs int64
	idxSumNs   int64
	idxMaxNs   int64
	totSumNs   int64
	totMaxNs   int64
}

// NewSearchStats returns a fresh, zeroed SearchStats.
func NewSearchStats() *SearchStats { return &SearchStats{} }

// recordSearch marks one completed Index.Search call — the "searches" total.
// Each call to Index.Search is exactly one hybrid search execution against
// one map's index; a caller that fans a single request out to several maps
// (Registry.Resolve with fanout>1) produces one recordSearch per map, not
// one per incoming request — see engine.RegistryStats for the request-level
// (routing/per-map) view.
func (s *SearchStats) recordSearch() { s.searches.Add(1) }

// recordSemanticCall marks one execution of the semantic (HNSW/brute-force)
// channel. In the current implementation this always fires exactly once per
// Search call (the query is always embedded and scanned), so it tracks
// searches 1:1 today; it is recorded independently, at the point the
// semantic branch actually runs, so it stays correct if that ever changes.
func (s *SearchStats) recordSemanticCall() { s.semanticCalls.Add(1) }

// recordLexicalAbstention marks a query the keyword channel opted OUT of
// entirely — empty query text, or a query that normalizes to zero
// content-bearing tokens (all stopwords). cmd/corpuscalib deliberately
// searches with an empty query text to characterise the embedding signals in
// isolation, so this is expected traffic, not an error condition.
func (s *SearchStats) recordLexicalAbstention() { s.lexicalAbstentions.Add(1) }

// recordCoverageRejections adds n candidates that the keyword channel found
// (matched at least one query token) but then dropped because they matched
// fewer than minCoverage's required token count.
func (s *SearchStats) recordCoverageRejections(n int) {
	if n > 0 {
		s.coverageRejections.Add(int64(n))
	}
}

// recordFusion classifies one search's final top-k hits into the three
// buckets that partition them: found by the keyword channel only (a
// "promotion" — the semantic channel's own ranked list did not surface it at
// all), found by the semantic channel only, or found by both. Callers must
// pass counts that sum to the number of hits actually returned — see
// Index.Search and fuseRRF, which is the only place that can compute this
// (it already builds both channels' membership sets to fuse them).
func (s *SearchStats) recordFusion(promotions, semanticOnly, both int) {
	if promotions > 0 {
		s.keywordPromotions.Add(int64(promotions))
	}
	if semanticOnly > 0 {
		s.semanticOnlyHits.Add(int64(semanticOnly))
	}
	if both > 0 {
		s.bothChannelHits.Add(int64(both))
	}
}

// recordLatency records one search's wall-clock breakdown: the time spent in
// the query-embedding call, and the time spent in everything else (the
// semantic scan, the lexical scan, and RRF fusion). Recorded separately
// because for a cold map the embed call dominates and an undifferentiated
// average would hide that.
func (s *SearchStats) recordLatency(embed, indexPhase time.Duration) {
	e, ix := embed.Nanoseconds(), indexPhase.Nanoseconds()
	tot := e + ix

	s.latMu.Lock()
	s.latCount++
	s.embedSumNs += e
	if e > s.embedMaxNs {
		s.embedMaxNs = e
	}
	s.idxSumNs += ix
	if ix > s.idxMaxNs {
		s.idxMaxNs = ix
	}
	s.totSumNs += tot
	if tot > s.totMaxNs {
		s.totMaxNs = tot
	}
	s.latMu.Unlock()
}

// LatencySnapshot is a point-in-time readout of search latency, in
// microseconds. Averages are derived from Count (0 when Count is 0).
type LatencySnapshot struct {
	Count      int64 `json:"count"`
	AvgTotalUs int64 `json:"avgTotalUs"`
	MaxTotalUs int64 `json:"maxTotalUs"`
	AvgEmbedUs int64 `json:"avgEmbedUs"`
	MaxEmbedUs int64 `json:"maxEmbedUs"`
	AvgIndexUs int64 `json:"avgIndexUs"`
	MaxIndexUs int64 `json:"maxIndexUs"`
}

// SearchStatsSnapshot is a point-in-time, JSON-friendly readout of SearchStats.
type SearchStatsSnapshot struct {
	Searches           int64           `json:"searches"`
	SemanticCalls      int64           `json:"semanticCalls"`
	LexicalAbstentions int64           `json:"lexicalAbstentions"`
	CoverageRejections int64           `json:"coverageRejections"`
	KeywordPromotions  int64           `json:"keywordPromotions"`
	SemanticOnlyHits   int64           `json:"semanticOnlyHits"`
	BothChannelHits    int64           `json:"bothChannelHits"`
	Latency            LatencySnapshot `json:"latency"`
}

// Snapshot reads every counter at one point in time. Individual fields are
// not read atomically WITH RESPECT TO EACH OTHER (there is no global lock),
// so under concurrent writers this is a consistent-enough readout for a
// metrics endpoint, not a transactional one.
func (s *SearchStats) Snapshot() SearchStatsSnapshot {
	s.latMu.Lock()
	count := s.latCount
	embedSum, embedMax := s.embedSumNs, s.embedMaxNs
	idxSum, idxMax := s.idxSumNs, s.idxMaxNs
	totSum, totMax := s.totSumNs, s.totMaxNs
	s.latMu.Unlock()

	avg := func(sum int64) int64 {
		if count == 0 {
			return 0
		}
		return (sum / count) / int64(time.Microsecond)
	}
	toUs := func(ns int64) int64 { return ns / int64(time.Microsecond) }

	return SearchStatsSnapshot{
		Searches:           s.searches.Load(),
		SemanticCalls:      s.semanticCalls.Load(),
		LexicalAbstentions: s.lexicalAbstentions.Load(),
		CoverageRejections: s.coverageRejections.Load(),
		KeywordPromotions:  s.keywordPromotions.Load(),
		SemanticOnlyHits:   s.semanticOnlyHits.Load(),
		BothChannelHits:    s.bothChannelHits.Load(),
		Latency: LatencySnapshot{
			Count:      count,
			AvgTotalUs: avg(totSum),
			MaxTotalUs: toUs(totMax),
			AvgEmbedUs: avg(embedSum),
			MaxEmbedUs: toUs(embedMax),
			AvgIndexUs: avg(idxSum),
			MaxIndexUs: toUs(idxMax),
		},
	}
}

// EmbedStatter is an optional capability an Embedder may implement to report
// its own query-embedding call/cache-hit counters. *embedder.Embedder
// does (it is the only place that actually knows whether a given embed was a
// cache hit or a real API round-trip — see its embed method). Callers that
// want that number on a metrics surface type-assert for this rather than
// re-deriving or approximating it: SearchStats deliberately does not track
// its own embedCalls/embedCacheHits, since the embedder is shared across
// every map's Index (all of them route query embeds through the same
// *embedder.Embedder instance) and duplicating the counters here would
// either drift from the real ones or double-count.
type EmbedStatter interface {
	Metrics() embedder.EmbedMetrics
}
