package memstore

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func rrfItem(id string, age time.Duration) Item {
	it := mk(id, "note", id, id)
	it.UpdatedAt = metav1.NewTime(time.Now().Add(-age))
	return it
}

func rrfIDs(items []Item) []string { return ids(items) }

func TestFuseRRF_ATieGoesToTheChannelTheCallerTrusts(t *testing.T) {
	lexOnly := rrfItem("zzz-lex", time.Hour)
	semOnly := rrfItem("aaa-sem", time.Hour)

	// Both sit at rank 0 of their own channel, so they tie. Alphabetical order
	// would always pick aaa-sem; the channel the caller trusts decides instead.
	assert.Equal(t, []string{"zzz-lex", "aaa-sem"}, rrfIDs(fuseRRF([]Item{lexOnly}, []Item{semOnly}, nil, 10, true)))
	assert.Equal(t, []string{"aaa-sem", "zzz-lex"}, rrfIDs(fuseRRF([]Item{lexOnly}, []Item{semOnly}, nil, 10, false)))
}

func TestFuseRRF_ATieInTheSameChannelGoesToTheNewerItemThenTheSmallerID(t *testing.T) {
	older := rrfItem("a-older", 2*time.Hour)
	newer := rrfItem("z-newer", time.Minute)
	// older is lexical rank 0 and newer is exact rank 0 (which counts for the
	// lexical channel): equal fused score, equal standing, so recency decides
	// where the id would have.
	got := fuseRRF([]Item{older}, nil, []Item{newer}, 10, true)
	assert.Equal(t, []string{"z-newer", "a-older"}, rrfIDs(got))

	same := metav1.NewTime(time.Now())
	x, y := rrfItem("b", 0), rrfItem("a", 0)
	x.UpdatedAt, y.UpdatedAt = same, same
	assert.Equal(t, []string{"a", "b"}, rrfIDs(fuseRRF([]Item{x}, nil, []Item{y}, 10, true)), "equal time: the smaller id")
}

func TestFuseRRF_AnExactMatchOutranksSingleChannelHits(t *testing.T) {
	named := rrfItem("named", time.Hour)
	lexTop := rrfItem("lex-top", time.Hour)
	semTop := rrfItem("sem-top", time.Hour)
	// named is lexical rank 1 and the exact match; the other two each lead a channel.
	got := fuseRRF([]Item{lexTop, named}, []Item{semTop}, []Item{named}, 10, true)
	assert.Equal(t, "named", got[0].ID, "two votes beat the best single-channel rank 0")
}

func TestFuseRRF_AnItemInBothChannelsBeatsOneInEither(t *testing.T) {
	a, b, c := rrfItem("a", 0), rrfItem("b", 0), rrfItem("c", 0)
	assert.Equal(t, "b", fuseRRF([]Item{a, b}, []Item{b, c}, nil, 10, true)[0].ID)
}

func TestFuseRRF_LimitAndDefault(t *testing.T) {
	items := make([]Item, 15)
	for i := range items {
		items[i] = rrfItem(string(rune('a'+i)), 0)
	}
	assert.Len(t, fuseRRF(items, nil, nil, 0, true), 10, "a zero limit means ten")
	assert.Len(t, fuseRRF(items, nil, nil, 3, true), 3)
}

func TestExactMatches_FindsATitleOrServiceTagEqualToTheQuery(t *testing.T) {
	s := NewStore()
	upsert(t, s, mk("by-title", "note", "Payment API", "body one"))
	upsert(t, s, mk("by-service", "note", "Something else", "body two", ServiceTagPrefix+"payment-api"))
	upsert(t, s, mk("partial", "note", "Payment API outage notes", "body three"))
	upsert(t, s, mk("other-kind", "stable_fact", "payment api", "body four"))
	priv := mk("private", "note", "payment api", "body five")
	priv.Scope = ScopePrivate
	upsert(t, s, priv)

	got := s.ExactMatches("payment-api?", "note", "")
	assert.ElementsMatch(t, []string{"by-title", "by-service"}, ids(got), "the whole title or the whole service name, punctuation aside")
	all := s.ExactMatches("payment api", "", "")
	assert.ElementsMatch(t, []string{"by-title", "by-service", "other-kind"}, ids(all), "no kind filter matches every kind; a private item never")
	assert.Empty(t, s.ExactMatches("payment", "", ""), "a partial title is not an exact match")
	assert.Empty(t, s.ExactMatches("  ", "", ""))
}
