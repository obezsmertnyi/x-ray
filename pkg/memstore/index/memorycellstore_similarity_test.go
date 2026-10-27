package index

import (
	"context"
	"testing"
	"time"
)

// TestSemanticCache_SimilarityIsCosine pins SearchByEmbedding's minSimilarity
// to the true cosine. kshard/vector's Cosine() distance is (1 - cos)/2;
// reading it as 1 - dist/2 let a 0.92 threshold match anything with
// cos >= 0.84, so a templated message for one event reused another's
// disposition.
func TestSemanticCache_SimilarityIsCosine(t *testing.T) {
	ctx := context.Background()
	s := newRawStore(nil)
	src := "app/env-a/shop/currency"
	if !s.SeedFromEmbedding(IndexEntry{
		ID: "event-1", Kind: "event", Source: src, Disposition: "signal", Timestamp: time.Now(),
	}, []float32{1, 0, 0, 0}) {
		t.Fatal("SeedFromEmbedding refused the entry")
	}
	cases := []struct {
		cos  float64
		want bool
	}{
		{0.99, true},
		{0.93, true},
		{0.90, false},
		{0.85, false}, // matched under the old transform
	}
	for _, c := range cases {
		hit, err := s.SearchByEmbedding(ctx, "", unitAt(c.cos), time.Time{}, 0.92, src)
		if err != nil {
			t.Fatalf("cos=%.2f: %v", c.cos, err)
		}
		if (hit != nil) != c.want {
			t.Errorf("cos=%.2f threshold=0.92: hit=%v, want %v", c.cos, hit != nil, c.want)
		}
	}
}
