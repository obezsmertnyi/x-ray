package memstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The coverage cutoff drops tail items that cover fewer query tokens than the
// top hit. When the count is dominated by filler words, an item that carries the
// token naming the subject but lacks one filler word should not be cut because
// another item happens to contain every filler word.
func TestRankAndLimit_CoverageCutoffSparesItemsThatCarryTheQuerysWeight(t *testing.T) {
	toks := []string{"alpha", "beta", "gamma", "entity"}
	weights := map[string]float64{"alpha": 0.5, "beta": 0.5, "gamma": 0.5, "entity": 3.0}

	prepare := func(id, title string) Item {
		it := mk(id, "note", title, "")
		it.Content = title
		buildLexicalCache(&it)
		return it
	}
	all := prepare("all", "alpha beta gamma entity")
	most := prepare("most", "alpha beta entity")        // matched 3 of 4, enough for the count gate
	fillerOnly := prepare("filler", "alpha beta gamma") // matched 3 of 4 but misses the weighty token

	got, _ := rankAndLimit("alpha beta gamma entity", toks, []Item{all, most, fillerOnly}, nil, nil, weights, 10)
	assert.Equal(t, "all", got[0].ID)
	assert.Contains(t, ids(got), "most", "the item with the entity is kept although the top hit matched one more token")
	assert.NotContains(t, ids(got), "filler", "an item without the heavy token and below the top coverage is still cut")
}
