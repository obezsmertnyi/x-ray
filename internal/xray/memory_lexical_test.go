package xray

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A conversational question names one thing. The cell that names it must beat
// the cells that only share the filler words, and must be found at all although
// it misses most of the query's tokens.
func TestKeywordSearch_RareEntityOutranksFiller(t *testing.T) {
	s := newTestStore()
	s.SetWriteDedup(false)
	upsertItem(t, s, "bq", "stable_fact", "public", "Warehouse notes", "The bigquery export job runs nightly and backfills late partitions", "", nil)
	// A corpus where the filler words are everywhere, so their weight is low
	// and "bigquery" is the one distinguishing token.
	for i := range 40 {
		upsertItem(t, s, fmt.Sprintf("noise-%d", i), "stable_fact", "public", fmt.Sprintf("Auth note %d", i),
			fmt.Sprintf("What is wrong with the auth workload %d? The login query path retries and the checkout service waits", i), "", nil)
	}

	got := s.KeywordSearch("what is wrong with the bigquery export", "", "", 10)
	require.NotEmpty(t, got, "the single rare-entity match must be admitted")
	assert.Equal(t, "bq", got[0].ID)
}

// A two-token name query ranks the item that has both tokens first. The engine's
// coverage cutoff spares an item whose matched tokens carry enough of the query's
// weight (docs/analysis/memory-search-review.md), so the item with one of the two
// follows rather than being dropped.
func TestKeywordSearch_ShortQueryRanksFullCoverageFirst(t *testing.T) {
	s := newTestStore()
	s.SetWriteDedup(false)
	upsertItem(t, s, "cache", "stable_fact", "public", "Edge cache", "response caching layer", "", nil)
	upsertItem(t, s, "proxy", "stable_fact", "public", "Edge proxy", "request routing layer", "", nil)
	got := s.KeywordSearch("Edge cache", "", "", 10)
	require.NotEmpty(t, got)
	assert.Equal(t, "cache", got[0].ID, "the item with both tokens leads")
}

func TestKeywordSearch_IDIsSearchable(t *testing.T) {
	s := newTestStore()
	s.SetWriteDedup(false)
	upsertItem(t, s, "billing-retry-policy", "stable_fact", "public", "Retries", "Failed charges are retried three times", "", nil)
	upsertItem(t, s, "other", "stable_fact", "public", "Other", "Nothing about it", "", nil)
	got := s.KeywordSearch("billing retry policy", "", "", 10)
	require.Len(t, got, 1)
	assert.Equal(t, "billing-retry-policy", got[0].ID)
}

func TestSearch_ExactIDMatchGoesFirst(t *testing.T) {
	s := newTestStore()
	s.SetWriteDedup(false)
	ctx := context.Background()
	upsertItem(t, s, "release-checklist", "stable_fact", "public", "Cutting a release", "Tag main, then publish the chart", "", nil)
	upsertItem(t, s, "unrelated", "stable_fact", "public", "Cutting corners", "Release checklist items are skipped under pressure", "", nil)
	upsertItem(t, s, "private-one", "note", "private", "Mine", "release-checklist is on my list", "alice", nil)

	got := s.Search(ctx, "release-checklist", "", "", 5)
	require.NotEmpty(t, got)
	assert.Equal(t, "release-checklist", got[0].ID)
	ids := map[string]int{}
	for _, it := range got {
		ids[it.ID]++
	}
	assert.Equal(t, 1, ids["release-checklist"], "the id match is not listed twice")

	assert.Empty(t, s.Search(ctx, "private-one", "", "", 5), "a private cell is not reached by id without its subject")
	got = s.Search(ctx, "private-one", "", "alice", 5)
	require.NotEmpty(t, got)
	assert.Equal(t, "private-one", got[0].ID)

	got = s.Search(ctx, "release-checklist", "note", "", 5)
	for _, it := range got {
		assert.Equal(t, "note", it.Kind, "the kind filter applies to the id match too")
	}
}

func TestIsGeneratedID(t *testing.T) {
	assert.True(t, isGeneratedID(newID()))
	assert.True(t, isGeneratedID("0123456789abcdef"))
	assert.False(t, isGeneratedID("0123456789abcdeg"))
	assert.False(t, isGeneratedID("short"))
	s := newTestStore()
	id := upsertItem(t, s, "", "note", "public", "Plain title", "plain body", "", nil)
	assert.Empty(t, s.KeywordSearch(id, "", "", 5), "a minted id is not search text")
}
