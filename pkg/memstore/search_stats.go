package memstore

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// StatsSink receives the search counters as they happen, so a host can keep
// them where it keeps its other counters. Add adds n to a named counter;
// Observe records one duration under a name (a host typically keeps a count and
// a summed nanoseconds beside each name). Either may be called from several
// goroutines.
type StatsSink interface {
	Add(name string, n int64)
	Observe(name string, d time.Duration)
}

// SearchStats counts hybrid search: how often the lexical fast path served a
// confident answer versus how often it fell through to (or failed over from) a
// semantic round trip, and where the time of a call went. The counters are kept
// in process for Snapshot and also forwarded to the sink, if any. A nil
// *SearchStats counts nothing.
type SearchStats struct {
	sink StatsSink
	mu   sync.Mutex
	ops  map[string]int64
}

// NewSearchStats counts in process and into sink (which may be nil).
func NewSearchStats(sink StatsSink) *SearchStats {
	return &SearchStats{sink: sink, ops: make(map[string]int64)}
}

// The counter names. Durations are summed in nanoseconds beside a count, so the
// mean is read as a quotient and never stored.
const (
	StatTotal               = "search.total"                // total HybridSearch calls
	StatKeywordHits         = "search.keyword_hits"         // calls where the lexical pass ran (always, on every call)
	StatSemanticCalls       = "search.semantic_calls"       // calls where lexical was not confident, so semantic search ran
	StatSemanticFallbacks   = "search.semantic_fallbacks"   // semantic search calls that errored and fell back to lexical-only
	StatExhaustiveFallbacks = "search.exhaustive_fallbacks" // calls rescued from a zero-result starvation by the full exact-cosine scan
	StatKWTrigramHits       = "search.kw_trigram_hits"      // keyword searches where the trigram index contributed a candidate
	StatKWVoiceHits         = "search.kw_voice_hits"        // calls served by the confident lexical fast path (no embedding call)
	StatKWVoiceMisses       = "search.kw_voice_misses"      // calls that fell through to semantic RRF fusion
	statHeadroomRows        = "search.headroom.rows"
	statHeadroomTruncated   = "search.headroom.truncated"
	statHeadroomCharsIn     = "search.headroom.chars_in"
	statHeadroomCharsOut    = "search.headroom.chars_out"
	statModePrefix          = "search.mode."   // + lexical|fused, then .ns or .count
	statPhasePrefix         = "search.phase."  // + the phase, then .ns or .count
	statCallerPrefix        = "search.caller." // + the caller, then .ns or .count
	statNs                  = ".ns"
	statCount               = ".count"
)

func (s *SearchStats) add(name string, n int64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.ops[name] += n
	s.mu.Unlock()
	if s.sink != nil {
		s.sink.Add(name, n)
	}
}

// timed counts one duration under prefix: a count and the summed nanoseconds.
func (s *SearchStats) timed(prefix string, d time.Duration) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.ops[prefix+statCount]++
	s.ops[prefix+statNs] += d.Nanoseconds()
	s.mu.Unlock()
	if s.sink != nil {
		s.sink.Observe(prefix, d)
	}
}

// PhaseStat is one hybridSearch phase's point-in-time latency readout.
type PhaseStat struct {
	AvgMs float64
	Count int64
}

// PhaseStats is hybridSearch's per-phase latency breakdown. lexical times the
// keyword scan; embed/index time the two halves of the cell store's semantic
// round trip (MemoryCellStore.SearchTimed); hydrate times resolving each
// semantic hit back to a memory.Item; rerank times Rerank. Recorded on whichever
// branch of hybridSearch actually runs a phase, so counts differ across phases
// by design (a confident lexical fast path never runs embed/index/hydrate).
type PhaseStats struct {
	Lexical PhaseStat
	Embed   PhaseStat
	Index   PhaseStat
	Hydrate PhaseStat
	Rerank  PhaseStat
}

// CallerStat is one caller's search count + mean duration — see
// WithSearchCaller and SearchStatsSnapshot.ByCaller.
type CallerStat struct {
	Caller string
	Count  int64
	AvgMs  float64
}

// recordCaller attributes one hybridSearch call's total duration to the
// subsystem that made it (see searchCallerFromCtx — "internal" when unstamped).
func (s *SearchStats) recordCaller(caller string, d time.Duration) {
	s.timed(statCallerPrefix+caller, d)
}

// RecordHeadroom folds in one headroom.CompressRows call's row/char counts —
// see Agent.RecordSearchHeadroom, the exported entry point every compression
// call site uses.
func (s *SearchStats) RecordHeadroom(rows, truncated, charsIn, charsOut int) {
	s.add(statHeadroomRows, int64(rows))
	s.add(statHeadroomTruncated, int64(truncated))
	s.add(statHeadroomCharsIn, int64(charsIn))
	s.add(statHeadroomCharsOut, int64(charsOut))
}

func (s *SearchStats) recordKeywordHit() {
	s.add(StatTotal, 1)
	s.add(StatKeywordHits, 1)
}

func (s *SearchStats) recordSemanticCall()     { s.add(StatSemanticCalls, 1) }
func (s *SearchStats) recordSemanticFallback() { s.add(StatSemanticFallbacks, 1) }

// recordExhaustiveFallback records that a call would otherwise have returned
// zero results (both the lexical and semantic channels came back empty) and
// was rescued by the full exact-cosine scan instead — see
// exhaustiveSemanticFallback's doc comment.
func (s *SearchStats) recordExhaustiveFallback() { s.add(StatExhaustiveFallbacks, 1) }

// recordKWTrigramHit records that the trigram index contributed at least one
// candidate to a keyword search result.
func (s *SearchStats) recordKWTrigramHit() { s.add(StatKWTrigramHits, 1) }

// recordKWVoiceHit records that the keyword-first path returned a confident
// result, short-circuiting without a semantic/embedding round-trip.
func (s *SearchStats) recordKWVoiceHit() { s.add(StatKWVoiceHits, 1) }

// recordKWVoiceMiss records that keyword-first returned a low-confidence
// result and fell through to semantic RRF fusion.
func (s *SearchStats) recordKWVoiceMiss() { s.add(StatKWVoiceMisses, 1) }

func (s *SearchStats) recordSearch(mode string, d time.Duration) {
	if mode != "fused" {
		mode = "lexical"
	}
	s.timed(statModePrefix+mode, d)
}

// Per-phase recorders — see PhaseStats for what each one measures and
// hybridSearch for the instrumented call sites.
func (s *SearchStats) recordLexicalPhase(d time.Duration) { s.timed(statPhasePrefix+"lexical", d) }
func (s *SearchStats) recordEmbedPhase(d time.Duration)   { s.timed(statPhasePrefix+"embed", d) }
func (s *SearchStats) recordIndexPhase(d time.Duration)   { s.timed(statPhasePrefix+"index", d) }
func (s *SearchStats) recordHydratePhase(d time.Duration) { s.timed(statPhasePrefix+"hydrate", d) }
func (s *SearchStats) recordRerankPhase(d time.Duration)  { s.timed(statPhasePrefix+"rerank", d) }

// SearchStatsSnapshot is a point-in-time copy of the search counters.
type SearchStatsSnapshot struct {
	Total               int64
	KeywordHits         int64
	SemanticCalls       int64
	SemanticFallbacks   int64
	ExhaustiveFallbacks int64
	KWTrigramHits       int64
	KWVoiceHits         int64
	KWVoiceMisses       int64
	// Mean in-process duration of a HybridSearch call, split by the path it
	// took: lexical-only (confident fast path, no embedding round trip) versus
	// fused (lexical + semantic RRF). This is the number that separates search
	// cost from the caller's network and framing.
	AvgLexicalMs float64
	AvgFusedMs   float64
	LexicalCount int64
	FusedCount   int64

	Phases   PhaseStats
	ByCaller []CallerStat

	HeadroomRows      int64
	HeadroomTruncated int64
	HeadroomCharsIn   int64
	HeadroomCharsOut  int64
}

// meanMs is the mean of a summed-nanoseconds counter over its count.
func meanMs(ops map[string]int64, prefix string) (avg float64, n int64) {
	n = ops[prefix+statCount]
	if n > 0 {
		avg = float64(ops[prefix+statNs]) / float64(n) / 1e6
	}
	return avg, n
}

// Snapshot returns a point-in-time snapshot of hybrid search
// effectiveness: lexical fast-path hits vs. semantic fallthroughs.
func (s *SearchStats) Snapshot() SearchStatsSnapshot {
	if s == nil {
		return SearchStatsSnapshot{}
	}
	s.mu.Lock()
	ops := make(map[string]int64, len(s.ops))
	for k, v := range s.ops {
		ops[k] = v
	}
	s.mu.Unlock()
	return SnapshotFromOps(ops)
}

// SnapshotFromOps builds a snapshot from counters a host keeps itself, keyed by
// the counter names of this package (StatTotal and the others, with the .count
// and .ns suffixes a sink's Observe maps to). A host that sums its counters
// across replicas reads its ledger and calls this, instead of repeating the
// names.
func SnapshotFromOps(ops map[string]int64) SearchStatsSnapshot {
	snap := SearchStatsSnapshot{
		Total:               ops[StatTotal],
		KeywordHits:         ops[StatKeywordHits],
		SemanticCalls:       ops[StatSemanticCalls],
		SemanticFallbacks:   ops[StatSemanticFallbacks],
		ExhaustiveFallbacks: ops[StatExhaustiveFallbacks],
		KWTrigramHits:       ops[StatKWTrigramHits],
		KWVoiceHits:         ops[StatKWVoiceHits],
		KWVoiceMisses:       ops[StatKWVoiceMisses],
		HeadroomRows:        ops[statHeadroomRows],
		HeadroomTruncated:   ops[statHeadroomTruncated],
		HeadroomCharsIn:     ops[statHeadroomCharsIn],
		HeadroomCharsOut:    ops[statHeadroomCharsOut],
	}
	snap.AvgLexicalMs, snap.LexicalCount = meanMs(ops, statModePrefix+"lexical")
	snap.AvgFusedMs, snap.FusedCount = meanMs(ops, statModePrefix+"fused")
	phase := func(name string) PhaseStat {
		avg, n := meanMs(ops, statPhasePrefix+name)
		return PhaseStat{AvgMs: avg, Count: n}
	}
	snap.Phases = PhaseStats{
		Lexical: phase("lexical"), Embed: phase("embed"), Index: phase("index"),
		Hydrate: phase("hydrate"), Rerank: phase("rerank"),
	}
	for k := range ops {
		if caller, ok := strings.CutSuffix(strings.TrimPrefix(k, statCallerPrefix), statCount); ok && strings.HasPrefix(k, statCallerPrefix) {
			avg, n := meanMs(ops, statCallerPrefix+caller)
			snap.ByCaller = append(snap.ByCaller, CallerStat{Caller: caller, Count: n, AvgMs: avg})
		}
	}
	sort.Slice(snap.ByCaller, func(i, j int) bool { return snap.ByCaller[i].Caller < snap.ByCaller[j].Caller })
	return snap
}
