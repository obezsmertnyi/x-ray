package memstore

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	memoryv1 "github.com/triageagent-dev/x-ray/v2/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestCollapseBetter_TieBreakOrder drives collapseBetter directly with
// hand-crafted, score-equal candidates: open beats resolved, then newer beats
// older, then lower ID wins as the final deterministic tie-break.
func TestCollapseBetter_TieBreakOrder(t *testing.T) {
	now := time.Now()
	open := map[string]bool{"open-1": true, "resolved-1": false, "resolved-2": false}
	s := NewSearcher(SearchConfig{State: func(it Item) (bool, bool) {
		o, known := open[it.ID]
		return o, known
	}})

	mk := func(id string, age time.Duration) rerankCandidate {
		return rerankCandidate{result: SearchResult{Item: Item{MemoryCellSpec: memoryv1.MemoryCellSpec{
			ID: id, Kind: "report", UpdatedAt: metav1.NewTime(now.Add(-age)),
		}}, Score: 1.0}}
	}

	openC := mk("open-1", time.Hour)
	resolvedNewer := mk("resolved-1", time.Minute)
	resolvedOlder := mk("resolved-2", 2*time.Hour)

	assert.True(t, s.collapseBetter(openC, resolvedNewer), "open must beat resolved even when resolved is newer, at equal score")
	assert.False(t, s.collapseBetter(resolvedNewer, openC))

	assert.True(t, s.collapseBetter(resolvedNewer, resolvedOlder), "same status: newer must win")
	assert.False(t, s.collapseBetter(resolvedOlder, resolvedNewer))

	// Same status, same UpdatedAt: lower ID wins (final determinism tie-break).
	tie1 := mk("resolved-1", time.Hour)
	tie2 := rerankCandidate{result: SearchResult{Item: Item{MemoryCellSpec: memoryv1.MemoryCellSpec{
		ID: "resolved-2", Kind: "report", UpdatedAt: tie1.result.Item.UpdatedAt,
	}}, Score: 1.0}}
	assert.True(t, s.collapseBetter(tie1, tie2), "resolved-1 < resolved-2 lexically, must win the final tie-break")
}
