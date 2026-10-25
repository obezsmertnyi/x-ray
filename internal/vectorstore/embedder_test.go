package vectorstore

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func l2norm(v []float32) float64 {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	return math.Sqrt(s)
}

func TestTruncateNormalize(t *testing.T) {
	// A non-unit 4-vector so we can see renormalization happen.
	vec := []float32{3, 4, 12, 84} // arbitrary

	t.Run("truncates to dims and renormalizes to unit length", func(t *testing.T) {
		got := truncateNormalize(vec, 2)
		require.Len(t, got, 2)
		assert.InDelta(t, 1.0, l2norm(got), 1e-6, "truncated vector must be unit-norm")
		// direction preserved: first two components proportional to {3,4}
		assert.InDelta(t, 3.0/5.0, got[0], 1e-6)
		assert.InDelta(t, 4.0/5.0, got[1], 1e-6)
	})

	t.Run("dims >= len returns vector unchanged (no truncation, no renorm)", func(t *testing.T) {
		got := truncateNormalize(vec, len(vec))
		assert.Equal(t, vec, got, "dims == len is a passthrough")
		got = truncateNormalize(vec, len(vec)+10)
		assert.Equal(t, vec, got, "dims > len is a passthrough")
	})

	t.Run("dims <= 0 returns native vector unchanged", func(t *testing.T) {
		assert.Equal(t, vec, truncateNormalize(vec, 0))
		assert.Equal(t, vec, truncateNormalize(vec, -1))
	})

	t.Run("zero vector does not divide by zero", func(t *testing.T) {
		got := truncateNormalize([]float32{0, 0, 0, 0}, 2)
		require.Len(t, got, 2)
		assert.Equal(t, []float32{0, 0}, got)
	})

	t.Run("already-unit truncation stays unit", func(t *testing.T) {
		// unit 3-vector; truncating to 2 then renormalizing must still be unit
		u := []float32{0.6, 0.8, 0}
		got := truncateNormalize(u, 2)
		assert.InDelta(t, 1.0, l2norm(got), 1e-6)
	})
}
