package xray

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mcell "github.com/triageagent-dev/x-ray/v2/api/v1"
)

// The retention backstop deletes a cell past its StaleAfter, and the item with it,
// across every kind except those the deadline must never delete. Deletion at the
// deadline itself is the reconcile's (cellsync.Decide); these tests drive the
// sweep directly rather than through its ticker.

func retentionCell(name, kind string, staleAfter *time.Time, mut ...func(*mcell.MemoryCell)) *mcell.MemoryCell {
	c := &mcell.MemoryCell{
		ObjectMeta: metav1.ObjectMeta{Name: cellResourceName(name), Namespace: "ns"},
		Spec: mcell.MemoryCellSpec{
			ID: name, Kind: kind, Scope: "public", Title: "T", Content: "C", Status: "active",
		},
	}
	if staleAfter != nil {
		t := metav1.NewTime(*staleAfter)
		c.Spec.StaleAfter = &t
	}
	for _, m := range mut {
		m(c)
	}
	return c
}

func retentionFixture(t *testing.T, cells ...*mcell.MemoryCell) (*CellSyncer, *MemoryStore, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, mcell.AddToScheme(scheme))
	objs := make([]client.Object, len(cells))
	for i, c := range cells {
		objs[i] = c
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	mem := NewMemoryStore()
	for _, c := range cells {
		mem.MirrorCell(c)
	}
	return NewCellSyncer(cl, mem, zerolog.Nop()), mem, cl
}

func cellExists(t *testing.T, cl client.Client, name string) bool {
	t.Helper()
	err := cl.Get(context.Background(), k8stypes.NamespacedName{Namespace: "ns", Name: cellResourceName(name)}, &mcell.MemoryCell{})
	if apierrors.IsNotFound(err) {
		return false
	}
	require.NoError(t, err)
	return true
}

func TestSweepExpired_DeletesAPastDeadlineCellAndItsItem(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	syncer, mem, cl := retentionFixture(t, retentionCell("old-event", "event", &past))
	require.NotNil(t, mem.GetItem("old-event"))

	syncer.sweepExpired(context.Background(), "ns")
	assert.False(t, cellExists(t, cl, "old-event"), "the cell is deleted")
	assert.Nil(t, mem.GetItem("old-event"), "and the item leaves the store")
}

func TestSweepExpired_KeepsWhatHasNoDeadlineOrAFutureOne(t *testing.T) {
	future := time.Now().Add(time.Hour)
	syncer, mem, cl := retentionFixture(t,
		retentionCell("no-deadline", "event", nil),
		retentionCell("not-yet", "event", &future),
	)
	syncer.sweepExpired(context.Background(), "ns")
	for _, id := range []string{"no-deadline", "not-yet"} {
		assert.True(t, cellExists(t, cl, id), id)
		assert.NotNil(t, mem.GetItem(id), id)
	}
}

func TestSweepExpired_NeverDeletesDurableOrConfigKindsOrGraphCells(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	withGraph := retentionCell("graph", "note", &past, func(c *mcell.MemoryCell) {
		c.Spec.Data = map[string]mcell.DataEntry{"graph": {Bytes: "Zw=="}}
	})
	withRef := retentionCell("graph-ref", "note", &past, func(c *mcell.MemoryCell) {
		c.Spec.Data = map[string]mcell.DataEntry{"graphRef": {Bytes: "registry.example/x@sha256:abc"}}
	})
	cells := []*mcell.MemoryCell{
		retentionCell("sysconf", "system_configuration", &past),
		retentionCell("identity", "system_identity", &past),
		retentionCell("policy", "policy", &past),
		retentionCell("episodic", "episodic", &past),
		retentionCell("map", "mental_map", &past),
		withGraph, withRef,
	}
	syncer, _, cl := retentionFixture(t, cells...)
	syncer.sweepExpired(context.Background(), "ns")
	for _, c := range cells {
		assert.True(t, cellExists(t, cl, c.Spec.ID), c.Spec.ID+" survives despite a past deadline")
	}
}

func TestSweepExpired_MixedBatchDeletesOnlyTheExpiredOnes(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)
	syncer, _, cl := retentionFixture(t,
		retentionCell("a-expired", "event", &past),
		retentionCell("b-expired", "note", &past),
		retentionCell("c-live", "event", &future),
		retentionCell("d-policy", "policy", &past),
	)
	syncer.sweepExpired(context.Background(), "ns")
	assert.False(t, cellExists(t, cl, "a-expired"))
	assert.False(t, cellExists(t, cl, "b-expired"))
	assert.True(t, cellExists(t, cl, "c-live"))
	assert.True(t, cellExists(t, cl, "d-policy"))
}

func TestSweepExpired_PublishesTheSameDeleteEventAUserDeleteDoes(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	syncer, mem, _ := retentionFixture(t, retentionCell("ev-expired", "event", &past))
	ch, stop := mem.hub.Subscribe("")
	defer stop()

	syncer.sweepExpired(context.Background(), "ns")
	select {
	case f := <-ch:
		assert.Contains(t, string(f.Data), "ev-expired", "the sweep fires the change event a user delete does")
		assert.Contains(t, string(f.Data), `"op":"delete"`)
	case <-time.After(time.Second):
		t.Fatal("expected a delete event for the expired item")
	}
}

func TestStartRetentionSweep_ZeroIntervalDisabled(t *testing.T) {
	syncer, _, _ := retentionFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	syncer.StartRetentionSweep(ctx, "ns", 0) // must return before spawning a goroutine
}

// ── decay by default: deadline stamping on Upsert ──────────────────────────

func TestUpsert_DefaultTTLStampedOnCreate(t *testing.T) {
	s := newTestStore()
	before := time.Now()
	ctx := context.Background()
	deadline := func(kind string) *time.Time {
		res, errMsg := s.Upsert(ctx, "", kind, "public", "T", "C", "", "", "", nil, nil)
		require.Empty(t, errMsg)
		return res.Item.StaleAfter
	}
	require.NotNil(t, deadline("event"), "event must get a default deadline")
	assert.WithinDuration(t, before.Add(30*24*time.Hour), *deadline("event"), time.Minute)
	assert.WithinDuration(t, before.Add(90*24*time.Hour), *deadline("recent_change"), time.Minute)
	assert.WithinDuration(t, before.Add(90*24*time.Hour), *deadline("note"), time.Minute)
	assert.Nil(t, deadline("stable_fact"), "a kind with no table entry never gets a default deadline")
	assert.Nil(t, deadline("operational_pattern"))
}

func TestUpsert_ExplicitStaleAfterOnCreateRespected(t *testing.T) {
	s := newTestStore()
	explicit := time.Now().Add(6 * time.Hour)
	res, errMsg := s.Upsert(context.Background(), "", "event", "public", "T", "C", "", "", "", nil, nil, withStaleAfter(explicit))
	require.Empty(t, errMsg)
	require.NotNil(t, res.Item.StaleAfter)
	assert.WithinDuration(t, explicit, *res.Item.StaleAfter, time.Second, "an explicit deadline wins over the per-kind default")
}

func TestUpsert_UpdateDoesNotRestampStaleAfter(t *testing.T) {
	s := newTestStore()
	ctx := context.Background()
	created, errMsg := s.Upsert(ctx, "", "event", "public", "T", "v1", "", "", "", nil, nil)
	require.Empty(t, errMsg)
	original := *created.Item.StaleAfter

	updated, errMsg := s.Upsert(ctx, created.ID, "event", "public", "T", "v2", "", "", "", nil, nil)
	require.Empty(t, errMsg)
	require.Equal(t, "updated", updated.Status)
	require.NotNil(t, updated.Item.StaleAfter)
	assert.WithinDuration(t, original, *updated.Item.StaleAfter, time.Second, "an update without a deadline must not restamp")

	renewal := time.Now().Add(365 * 24 * time.Hour)
	renewed, errMsg := s.Upsert(ctx, created.ID, "event", "public", "T", "v3", "", "", "", nil, nil, withStaleAfter(renewal))
	require.Empty(t, errMsg)
	require.NotNil(t, renewed.Item.StaleAfter)
	assert.WithinDuration(t, renewal, *renewed.Item.StaleAfter, time.Second, "an explicit deadline on an update renews it")
}

func TestUpsert_AnItemSeededFromACellKeepsItsDeadlineOnUpdate(t *testing.T) {
	stale := metav1.NewTime(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	cell := retentionCell("prior-stale-id", "event", nil)
	cell.Spec.StaleAfter = &stale
	s := newTestStore()
	s.MirrorCell(cell)

	res, errMsg := s.Upsert(context.Background(), "prior-stale-id", "event", "public", "T", "v2", "", "", "", nil, nil)
	require.Empty(t, errMsg)
	assert.Equal(t, "updated", res.Status)
	require.NotNil(t, res.Item.StaleAfter, "a seeded item's StaleAfter survives an update")
	assert.True(t, res.Item.StaleAfter.Equal(stale.Time))
}
