package index

import (
	"context"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func scored(scores ...float32) []CellSearchResult {
	out := make([]CellSearchResult, len(scores))
	for i, s := range scores {
		out[i] = CellSearchResult{Document: IndexEntry{ID: string(rune('a' + i))}, Score: s}
	}
	return out
}

func resultIDs(rs []CellSearchResult) string {
	out := make([]byte, len(rs))
	for i, r := range rs {
		out[i] = r.Document.ID[0]
	}
	return string(out)
}

// The store returns the whole window it was asked for. Cutting it before the
// caller has filtered what the caller may see, and fused it with the other
// channel, starves a caller whose answers sit a few points below a
// near-verbatim hit it cannot read.
func TestSearch_ReturnsTheWholeWindowWithNoCutoffInsideTheStore(t *testing.T) {
	emb := textGateway(t, map[string][]float32{"q": vec(1, 0, 0)})
	s := NewCellStore(emb, zerolog.Nop())
	seedEvent(t, s, IndexEntry{ID: "top", Kind: "note"}, vec(1, 0, 0))
	seedEvent(t, s, IndexEntry{ID: "mid", Kind: "note"}, vec(0.9, 0.43, 0))
	seedEvent(t, s, IndexEntry{ID: "low", Kind: "note"}, vec(0.5, 0.86, 0))

	got, err := s.Search(context.Background(), "q", 5, "")
	require.NoError(t, err)
	assert.Equal(t, 3, len(got), "all three, however far the second falls below the first")

	s.SetScoreGap(0.02) // kept for callers that still set it; it no longer cuts
	got, err = s.Search(context.Background(), "q", 5, "")
	require.NoError(t, err)
	assert.Equal(t, 3, len(got))
	sub, err := s.SearchSubject(context.Background(), "q", "", 5, "")
	require.NoError(t, err)
	assert.Equal(t, 3, len(sub))
}

func TestCutAtGap_DropsTheTailMoreThanGapBelowTheTop(t *testing.T) {
	in := scored(0.92, 0.905, 0.88, 0.5)
	assert.Equal(t, "ab", resultIDs(CutAtGap(in, 0.02)))
	assert.Equal(t, "abcd", resultIDs(CutAtGap(in, 0)), "a zero gap keeps everything")
	assert.Equal(t, "a", resultIDs(CutAtGap(scored(0.4), 0.02)))
	assert.Empty(t, CutAtGap(nil, 0.02))
}

func TestCutAtLargestGap_KeepsEverythingAboveTheSteepestDrop(t *testing.T) {
	assert.Equal(t, "ab", resultIDs(CutAtLargestGap(scored(0.95, 0.94, 0.70, 0.69, 0.68))), "the cliff is between b and c")
	assert.Equal(t, "a", resultIDs(CutAtLargestGap(scored(0.95, 0.60, 0.59))))
	assert.Equal(t, "a", resultIDs(CutAtLargestGap(scored(0.9, 0.8, 0.7, 0.6))), "an even decline cuts at the earliest of its equal gaps")
	assert.Equal(t, "a", resultIDs(CutAtLargestGap(scored(0.9))))
	assert.Empty(t, CutAtLargestGap(nil))
}
