package memstore

import (
	"testing"

	"github.com/stretchr/testify/assert"

	memoryv1 "github.com/triageagent-dev/x-ray/v2/api/v1"
)

// The four numbers below decide which writes fold into an existing cell and
// which cells a consolidation retires. A host that tunes them silently changes
// what its memory forgets, so they are pinned here.
func TestNearDuplicateThresholds_ArePinned(t *testing.T) {
	assert.Equal(t, 0.3, MemoryNearDupThreshold)
	assert.Equal(t, 8, NearDupMinTokens)
	assert.Equal(t, 0.8, memoryNearDupShortTextThreshold)
	assert.Equal(t, 0.9, nearDupContainment)
}

func dedupItem(id, title, content string) Item {
	return Item{MemoryCellSpec: memoryv1.MemoryCellSpec{ID: id, Kind: KindStableFact, Scope: ScopePublic, Title: title, Content: content, Status: "active"}}
}

func TestDedupTokens_KeepsShortTokensWithADigit(t *testing.T) {
	toks := DedupTokens(dedupItem("a", "Plan for Q3", "ship it to EU"))
	assert.Contains(t, toks, "q3", "a digit makes a short token a signal")
	assert.NotContains(t, toks, "to", "an ordinary short token is noise")
	assert.NotContains(t, toks, "it")
	assert.NotContains(t, toks, "for", "stop words are dropped")
}

func TestNearDuplicateMemory_ShortTextOnlyDiffersByAQuarter(t *testing.T) {
	q3 := dedupItem("q3", "Roadmap Q3", "ship search")
	q4 := dedupItem("q4", "Roadmap Q4", "ship search")
	_, dup := NearDuplicateMemory(q3, q4)
	assert.False(t, dup, "two plans that differ only by their quarter are two facts")
}

func TestDedupComparable_SameScopeSubjectAndOrigin(t *testing.T) {
	base := dedupItem("a", "Cache layer", "the cache evicts after ten minutes of silence")
	same := dedupItem("b", "Cache layer", "the cache evicts after ten minutes of silence")
	assert.True(t, DedupComparable(base, same))

	other := same
	other.Scope = ScopePrivate
	assert.False(t, DedupComparable(base, other), "a different scope never compares")

	other = same
	other.Subject = "another-instance"
	assert.False(t, DedupComparable(base, other), "a different subject never compares")

	other = same
	other.Subject = "ONE"
	base.Subject = "one"
	assert.True(t, DedupComparable(base, other), "subjects compare case-insensitively")

	human := base
	human.Origin = OriginHuman
	derived := same
	derived.Subject = "one"
	derived.Origin = OriginDerived
	assert.False(t, DedupComparable(human, derived), "a model-written item never folds into a person's note")
	assert.True(t, DedupComparable(derived, human), "the reverse is allowed: a person's restatement may fold into a derived cell")
}
