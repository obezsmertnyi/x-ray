package memstore

import (
	"testing"

	"github.com/stretchr/testify/require"

	memoryv1 "github.com/triageagent-dev/x-ray/v2/api/v1"
)

// ── predicate table test ────────────────────────────────────────────────

func TestVisibleToUser_Table(t *testing.T) {
	cases := []struct {
		name     string
		scope    string
		tags     []string
		userHash string
		want     bool
	}{
		{"public item, no caller", "global", nil, "", true},
		{"public item, any caller", "global", nil, "someone", true},
		{"private item, owner", ScopePrivate, []string{"user:alice-hash"}, "alice-hash", true},
		{"private item, another caller", ScopePrivate, []string{"user:alice-hash"}, "bob-hash", false},
		{"private item, no caller identity", ScopePrivate, []string{"user:alice-hash"}, "", false},
		{"private item, no owner tag at all", ScopePrivate, nil, "alice-hash", false},
		{"private item, owner tag empty suffix never matches", ScopePrivate, []string{"user:"}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			it := Item{MemoryCellSpec: memoryv1.MemoryCellSpec{Scope: tc.scope, Tags: tc.tags}}
			require.Equal(t, tc.want, VisibleToUser(it, tc.userHash))
		})
	}
}

func TestIsPrivateItem_ScopeOnly(t *testing.T) {
	require.True(t, IsPrivateItem(Item{MemoryCellSpec: memoryv1.MemoryCellSpec{Scope: ScopePrivate}}))
	require.False(t, IsPrivateItem(Item{MemoryCellSpec: memoryv1.MemoryCellSpec{Scope: "global"}}))
	require.False(t, IsPrivateItem(Item{MemoryCellSpec: memoryv1.MemoryCellSpec{Scope: ""}}))
}

func TestPrivateOwner_ReadsUserTag(t *testing.T) {
	it := Item{MemoryCellSpec: memoryv1.MemoryCellSpec{
		Tags: []string{"session", "user:alice-hash", "tenant/partition-a"},
	}}
	require.Equal(t, "alice-hash", PrivateOwner(it))
	require.Equal(t, "", PrivateOwner(Item{MemoryCellSpec: memoryv1.MemoryCellSpec{Tags: []string{"session"}}}))
}

func TestFilterVisibleToUser_OwnerOnly_NoAdminOrPartitionBypass(t *testing.T) {
	items := []Item{
		{MemoryCellSpec: memoryv1.MemoryCellSpec{ID: "pub", Scope: "global"}},
		{MemoryCellSpec: memoryv1.MemoryCellSpec{ID: "alice-priv", Scope: ScopePrivate, Tags: []string{"user:alice-hash"}}},
		{MemoryCellSpec: memoryv1.MemoryCellSpec{ID: "bob-priv", Scope: ScopePrivate, Tags: []string{"user:bob-hash"}}},
	}
	// "admin"/"operator" identities get no special treatment whatsoever —
	// they must pass the exact owner hash like anyone else.
	got := FilterVisibleToUser(items, "admin")
	require.Len(t, got, 1)
	require.Equal(t, "pub", got[0].ID)

	got = FilterVisibleToUser(items, "alice-hash")
	require.Len(t, got, 2)
	ids := map[string]bool{got[0].ID: true, got[1].ID: true}
	require.True(t, ids["pub"] && ids["alice-priv"])
}
