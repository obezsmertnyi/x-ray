package memstore

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── Phase timing (docs/todo/voice-proactive-audio-and-search-metrics.md,
// Change 2) ──────────────────────────────────────────────────────────────

// TestMemorySearchStatsSnapshot_PerPhaseBreakdown proves each of the five
// named phase recorders lands in its own PhaseStats field, independently of
// the others — the whole point of the phase split (a 930ms first-search
// spike couldn't be attributed to embed vs. index vs. hydrate before this).
func TestMemorySearchStatsSnapshot_PerPhaseBreakdown(t *testing.T) {
	stats := NewSearchStats(nil)
	stats.recordLexicalPhase(10 * time.Millisecond)
	stats.recordLexicalPhase(30 * time.Millisecond)
	stats.recordEmbedPhase(900 * time.Millisecond) // the reported spike shape
	stats.recordIndexPhase(2 * time.Millisecond)
	stats.recordHydratePhase(1 * time.Millisecond)
	stats.recordRerankPhase(3 * time.Millisecond)

	snap := stats.Snapshot()

	assert.Equal(t, int64(2), snap.Phases.Lexical.Count)
	assert.InDelta(t, 20.0, snap.Phases.Lexical.AvgMs, 0.01)

	assert.Equal(t, int64(1), snap.Phases.Embed.Count)
	assert.InDelta(t, 900.0, snap.Phases.Embed.AvgMs, 0.01)

	assert.Equal(t, int64(1), snap.Phases.Index.Count)
	assert.Equal(t, int64(1), snap.Phases.Hydrate.Count)
	assert.Equal(t, int64(1), snap.Phases.Rerank.Count)
}

// ── Per-caller attribution ───────────────────────────────────────────────

// TestMemorySearchStats_ByCaller_SortedAndAveraged proves recordCaller
// accumulates per-caller count/duration correctly and callerSnapshot returns
// them sorted by name (a deterministic snapshot, not map iteration order).
func TestMemorySearchStats_ByCaller_SortedAndAveraged(t *testing.T) {
	stats := NewSearchStats(nil)
	stats.recordCaller("voice", 10*time.Millisecond)
	stats.recordCaller("voice", 30*time.Millisecond)
	stats.recordCaller("mcp", 5*time.Millisecond)

	snap := stats.Snapshot()

	require.Len(t, snap.ByCaller, 2)
	assert.Equal(t, "mcp", snap.ByCaller[0].Caller, "sorted alphabetically before voice")
	assert.Equal(t, int64(1), snap.ByCaller[0].Count)
	assert.InDelta(t, 5.0, snap.ByCaller[0].AvgMs, 0.01)

	assert.Equal(t, "voice", snap.ByCaller[1].Caller)
	assert.Equal(t, int64(2), snap.ByCaller[1].Count)
	assert.InDelta(t, 20.0, snap.ByCaller[1].AvgMs, 0.01, "(10+30)/2")
}

// TestMemorySearchStats_ByCaller_EmptyWhenUnused proves a fresh
// SearchStats reports an empty (not nil-panicking) ByCaller.
func TestMemorySearchStats_ByCaller_EmptyWhenUnused(t *testing.T) {
	assert.Empty(t, NewSearchStats(nil).Snapshot().ByCaller)
}

// ── Headroom recording ───────────────────────────────────────────────────

// TestRecordSearchHeadroom_AccumulatesAcrossCalls proves
// SearchStats.RecordHeadroom sums rows/truncated/chars across multiple
// calls (the four compression call sites each call it independently).
func TestRecordSearchHeadroom_AccumulatesAcrossCalls(t *testing.T) {
	stats := NewSearchStats(nil)
	stats.RecordHeadroom(3, 1, 900, 300)
	stats.RecordHeadroom(2, 0, 200, 200)

	snap := stats.Snapshot()
	assert.Equal(t, int64(5), snap.HeadroomRows)
	assert.Equal(t, int64(1), snap.HeadroomTruncated)
	assert.Equal(t, int64(1100), snap.HeadroomCharsIn)
	assert.Equal(t, int64(500), snap.HeadroomCharsOut)
}

// TestRecordSearchHeadroom_NilStatsIsNoOp proves a nil SearchStats
// (a host built without search) doesn't panic when a headroom call site
// records against it.
func TestRecordSearchHeadroom_NilStatsIsNoOp(t *testing.T) {
	var stats *SearchStats
	assert.NotPanics(t, func() { stats.RecordHeadroom(1, 1, 10, 5) })
}

func TestSnapshotFromOps_ReadsCountersTheHostKeepsItself(t *testing.T) {
	snap := SnapshotFromOps(map[string]int64{
		StatTotal:                   7,
		StatSemanticCalls:           2,
		"search.mode.fused.count":   2,
		"search.mode.fused.ns":      int64(30 * time.Millisecond),
		"search.phase.embed.count":  2,
		"search.phase.embed.ns":     int64(20 * time.Millisecond),
		"search.caller.voice.count": 3,
		"search.caller.voice.ns":    int64(9 * time.Millisecond),
		"search.headroom.rows":      11,
	})
	assert.EqualValues(t, 7, snap.Total)
	assert.EqualValues(t, 2, snap.SemanticCalls)
	assert.InDelta(t, 15.0, snap.AvgFusedMs, 0.001)
	assert.InDelta(t, 10.0, snap.Phases.Embed.AvgMs, 0.001)
	require.Len(t, snap.ByCaller, 1)
	assert.Equal(t, "voice", snap.ByCaller[0].Caller)
	assert.InDelta(t, 3.0, snap.ByCaller[0].AvgMs, 0.001)
	assert.EqualValues(t, 11, snap.HeadroomRows)
	assert.Equal(t, SearchStatsSnapshot{}, (*SearchStats)(nil).Snapshot())
}
