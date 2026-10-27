package memstore

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	memoryv1 "github.com/triageagent-dev/x-ray/v2/api/v1"
)

func itemWithTags(tags ...string) Item {
	return Item{MemoryCellSpec: memoryv1.MemoryCellSpec{ID: "i", Tags: tags}}
}

func TestStampAuthorTag_ReplacesAForgedAuthorAndIsIdempotent(t *testing.T) {
	it := itemWithTags("topic/x", "author/forged", "author/other")
	StampAuthorTag(&it, "alice")
	assert.Equal(t, []string{"topic/x", "author/alice"}, it.Tags)

	StampAuthorTag(&it, "alice")
	assert.Equal(t, []string{"topic/x", "author/alice"}, it.Tags, "stamping twice changes nothing")

	StampAuthorTag(&it, "")
	assert.Equal(t, []string{"topic/x", "author/alice"}, it.Tags, "an empty author is a no-op")
}

func TestAuthorAndServiceTagValues(t *testing.T) {
	it := itemWithTags("service/auth", "author/alice")
	v, ok := ServiceTagValue(it)
	assert.True(t, ok)
	assert.Equal(t, "auth", v)
	v, ok = AuthorTagValue(it)
	assert.True(t, ok)
	assert.Equal(t, "alice", v)

	_, ok = ServiceTagValue(itemWithTags("service/"))
	assert.False(t, ok, "an empty value is not a tag")
	_, ok = AuthorTagValue(itemWithTags("topic/x"))
	assert.False(t, ok)
}

func TestStripAuthorTags(t *testing.T) {
	assert.Nil(t, StripAuthorTags(nil))
	assert.Equal(t, []string{"a", "b"}, StripAuthorTags([]string{"a", "author/x", "b"}))
}

func TestPartitionFromTagsAndSharedMark(t *testing.T) {
	assert.Equal(t, "alpha", PartitionFromTags([]string{"x", "tenant/alpha"}))
	assert.Equal(t, "", PartitionFromTags([]string{"x"}))
	assert.True(t, HasSharedTag([]string{"shared"}))
	assert.False(t, HasSharedTag([]string{"shared/x"}))
	assert.True(t, IsSharedItem(itemWithTags("tenant/alpha", SharedTag)), "shared combines with a partition tag")
}

func TestSanitizeTagValue(t *testing.T) {
	assert.Equal(t, "a b c", SanitizeTagValue("  a,b\n c  "))
	assert.Equal(t, "", SanitizeTagValue(" , "))
}

func TestItem_CloneSharesNoSliceAndExpiry(t *testing.T) {
	it := Item{MemoryCellSpec: memoryv1.MemoryCellSpec{
		Status: "active", Tags: []string{"a"}, EvidenceRefs: []string{"e"}, Supersedes: []string{"s"},
		Links: []memoryv1.ThoughtLink{{ToID: "t", EvidenceRefs: []string{"x"}}},
	}}
	c := it.Clone()
	c.Tags[0], c.EvidenceRefs[0], c.Supersedes[0], c.Links[0].EvidenceRefs[0] = "z", "z", "z", "z"
	assert.Equal(t, "a", it.Tags[0])
	assert.Equal(t, "e", it.EvidenceRefs[0])
	assert.Equal(t, "s", it.Supersedes[0])
	assert.Equal(t, "x", it.Links[0].EvidenceRefs[0])

	assert.True(t, it.IsActive())
	past := metav1.NewTime(time.Now().Add(-time.Minute))
	it.StaleAfter = &past
	assert.True(t, it.IsExpired())
	assert.False(t, it.IsActive(), "an expired item is not active")
	require.NotNil(t, it.StaleAfter)
}
