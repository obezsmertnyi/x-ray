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

// General retention sweep (decay by default) . Mirrors the style of the episodic-reaper tests in
// memory_recent_test.go: map-mode items are seeded directly into s.items with
// an explicit StaleAfter, and the sweep function is called directly rather than
// through the ticker goroutine.

// seedRetentionItem inserts an item straight into s.items with an explicit
// StaleAfter (nil meaning "not set"), the same direct-seed pattern
// memory_recent_test.go uses for CreatedAt via mkEpisodic.
func seedRetentionItem(s *MemoryStore, id, kind string, staleAfter *time.Time) {
	s.mu.Lock()
	s.items[id] = &MemoryItem{
		ID: id, Kind: kind, Scope: scopePublic, Status: "active",
		Title: "T", Content: "C", StaleAfter: staleAfter,
	}
	s.mu.Unlock()
}

func pastDeadline() *time.Time {
	t := time.Now().Add(-time.Hour)
	return &t
}

func futureDeadline() *time.Time {
	t := time.Now().Add(time.Hour)
	return &t
}

// TestSweepExpiredRetention_DeletesPastDeadlineNonExemptKind confirms the core
// behavior: a non-exempt kind (event) past its StaleAfter deadline is removed
// from the store.
func TestSweepExpiredRetention_DeletesPastDeadlineNonExemptKind(t *testing.T) {
	s := newTestStore()
	seedRetentionItem(s, "ev-expired", "event", pastDeadline())

	s.sweepExpiredRetention(context.Background(), nil)

	assert.Nil(t, s.GetItem("ev-expired"), "item past its StaleAfter deadline must be removed")
}

// TestSweepExpiredRetention_NoStaleAfterSurvives confirms an item with no
// StaleAfter set is never touched by the sweep, regardless of kind or age.
func TestSweepExpiredRetention_NoStaleAfterSurvives(t *testing.T) {
	s := newTestStore()
	seedRetentionItem(s, "ev-forever", "event", nil)

	s.sweepExpiredRetention(context.Background(), nil)

	assert.NotNil(t, s.GetItem("ev-forever"), "item with no StaleAfter must never be swept")
}

// TestSweepExpiredRetention_FutureDeadlineSurvives confirms an item whose
// deadline has not yet passed is left alone.
func TestSweepExpiredRetention_FutureDeadlineSurvives(t *testing.T) {
	s := newTestStore()
	seedRetentionItem(s, "ev-future", "event", futureDeadline())

	s.sweepExpiredRetention(context.Background(), nil)

	assert.NotNil(t, s.GetItem("ev-future"), "item whose deadline is still in the future must survive")
}

// TestSweepExpiredRetention_ExemptKindsSurviveDespitePastDeadline confirms the
// unconditional exemption list: even with a StaleAfter in the past,
// system_configuration/system_identity/policy/episodic and a map-carrying cell
// are never removed by the sweep.
func TestSweepExpiredRetention_ExemptKindsSurviveDespitePastDeadline(t *testing.T) {
	s := newTestStore()
	seedRetentionItem(s, "sysconf", "system_configuration", pastDeadline())
	seedRetentionItem(s, "sysid", "system_identity", pastDeadline())
	seedRetentionItem(s, "pol", "policy", pastDeadline())
	seedRetentionItem(s, "mm-by-kind", "mental_map", pastDeadline())

	// Map-carrying cell identified by the Data["graph"] blob, mirroring
	// CellSyncer.syncMapCell's own identification — even under a kind that
	// isn't literally "mental_map".
	s.mu.Lock()
	s.items["mm-by-data"] = &MemoryItem{
		ID: "mm-by-data", Kind: "note", Scope: scopePublic, Status: "active",
		Title: "Map", Content: "C", StaleAfter: pastDeadline(),
		Data: map[string]mcell.DataEntry{graphDataKey: {Bytes: "Z2Ji"}},
	}
	s.mu.Unlock()

	s.sweepExpiredRetention(context.Background(), nil)

	assert.NotNil(t, s.GetItem("sysconf"), "system_configuration must never be swept")
	assert.NotNil(t, s.GetItem("sysid"), "system_identity must never be swept")
	assert.NotNil(t, s.GetItem("pol"), "policy must never be swept")
	assert.NotNil(t, s.GetItem("mm-by-kind"), "kind==mental_map must never be swept")
	assert.NotNil(t, s.GetItem("mm-by-data"), "a Data[\"graph\"]-carrying cell must never be swept regardless of kind")
}

// TestSweepExpiredRetention_EpisodicIgnored confirms the new sweep does not
// double-handle episodic memories — those are owned exclusively by
// StartEpisodicReaper (CreatedAt+episodicTTL, not StaleAfter). Even an
// episodic item with an (unusual) past StaleAfter must survive this sweep.
func TestSweepExpiredRetention_EpisodicIgnored(t *testing.T) {
	s := newTestStore()
	seedRetentionItem(s, "ep-1", "episodic", pastDeadline())

	s.sweepExpiredRetention(context.Background(), nil)

	assert.NotNil(t, s.GetItem("ep-1"), "episodic items must be ignored by the retention sweep")
}

// TestSweepExpiredRetention_MixedBatch runs a realistic mixed set through one
// sweep call and checks every outcome at once.
func TestSweepExpiredRetention_MixedBatch(t *testing.T) {
	s := newTestStore()
	seedRetentionItem(s, "note-expired", "note", pastDeadline())
	seedRetentionItem(s, "note-fresh", "note", futureDeadline())
	seedRetentionItem(s, "fact-no-ttl", "stable_fact", nil)
	seedRetentionItem(s, "policy-expired", "policy", pastDeadline())
	seedRetentionItem(s, "episodic-expired", "episodic", pastDeadline())

	s.sweepExpiredRetention(context.Background(), nil)

	assert.Nil(t, s.GetItem("note-expired"))
	assert.NotNil(t, s.GetItem("note-fresh"))
	assert.NotNil(t, s.GetItem("fact-no-ttl"))
	assert.NotNil(t, s.GetItem("policy-expired"))
	assert.NotNil(t, s.GetItem("episodic-expired"))
}

// TestSweepExpiredRetention_CRBacked confirms the sweep goes through the same
// delete path a user delete uses: MemoryStore.Delete (SSE event) AND the CR
// delete via the syncer, when a cache reader is wired (the production
// configuration) — "memory AND CR gone".
func TestSweepExpiredRetention_CRBacked(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, mcell.AddToScheme(scheme))

	past := metav1.NewTime(time.Now().Add(-time.Hour))
	expiredCell := &mcell.MemoryCell{
		ObjectMeta: metav1.ObjectMeta{Name: cellResourceName("ev-cr-expired"), Namespace: "ns"},
		Spec: mcell.MemoryCellSpec{
			ID: "ev-cr-expired", Kind: "event", Scope: "public", Title: "T", Content: "C",
			Status: "active", StaleAfter: &past,
		},
	}
	future := metav1.NewTime(time.Now().Add(time.Hour))
	freshCell := &mcell.MemoryCell{
		ObjectMeta: metav1.ObjectMeta{Name: cellResourceName("ev-cr-fresh"), Namespace: "ns"},
		Spec: mcell.MemoryCellSpec{
			ID: "ev-cr-fresh", Kind: "event", Scope: "public", Title: "T", Content: "C",
			Status: "active", StaleAfter: &future,
		},
	}
	exemptCell := &mcell.MemoryCell{
		ObjectMeta: metav1.ObjectMeta{Name: cellResourceName("policy-cr-expired"), Namespace: "ns"},
		Spec: mcell.MemoryCellSpec{
			ID: "policy-cr-expired", Kind: "policy", Scope: "public", Title: "T", Content: "C",
			Status: "active", StaleAfter: &past,
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(expiredCell, freshCell, exemptCell).
		WithIndex(&mcell.MemoryCell{}, "spec.id", func(o client.Object) []string {
			return []string{o.(*mcell.MemoryCell).Spec.ID}
		}).Build()

	mem := NewMemoryStore()
	mem.SetCacheReader(fakeClient, "ns")
	syncer := NewCellSyncer(fakeClient, mem, zerolog.Nop())

	// SSE assertion: the expired item's delete must be published, same as a
	// user-initiated delete.
	ch := mem.hub.subscribe()
	defer mem.hub.unsubscribe(ch)

	mem.sweepExpiredRetention(context.Background(), syncer)

	select {
	case raw := <-ch:
		assert.Contains(t, string(raw), "ev-cr-expired", "sweep delete must fire the same SSE change event a user delete does")
	case <-time.After(time.Second):
		t.Fatal("expected a delete SSE event for the expired item")
	}

	assert.Nil(t, mem.GetItem("ev-cr-expired"), "expired event item must be gone from memory")
	got := &mcell.MemoryCell{}
	err := syncer.Get(context.Background(), k8stypes.NamespacedName{Namespace: "ns", Name: cellResourceName("ev-cr-expired")}, got)
	assert.True(t, apierrors.IsNotFound(err), "expired event item's CR must be deleted too")

	assert.NotNil(t, mem.GetItem("ev-cr-fresh"), "fresh item must survive")
	require.NoError(t, syncer.Get(context.Background(), k8stypes.NamespacedName{Namespace: "ns", Name: cellResourceName("ev-cr-fresh")}, &mcell.MemoryCell{}))

	assert.NotNil(t, mem.GetItem("policy-cr-expired"), "exempt kind must survive despite a past deadline")
	require.NoError(t, syncer.Get(context.Background(), k8stypes.NamespacedName{Namespace: "ns", Name: cellResourceName("policy-cr-expired")}, &mcell.MemoryCell{}))
}

// TestStartRetentionSweep_ZeroIntervalDisabled confirms every<=0 disables the
// sweep loop entirely (no goroutine started, no panic) — mirrors
// StartEpisodicReaper's own no-op contract.
func TestStartRetentionSweep_ZeroIntervalDisabled(t *testing.T) {
	s := newTestStore()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartRetentionSweep(ctx, nil, 0)
	// Nothing to assert beyond "did not panic / did not block" — every<=0 must
	// return before spawning the ticker goroutine.
}

// ── decay by default: TTL stamping on Upsert ────────────────────────────────

// TestUpsert_DefaultTTLStampedOnCreate confirms event/recent_change/note get a
// per-kind default deadline on create when none is given, and that a kind with
// no table entry (stable_fact) never gets one.
func TestUpsert_DefaultTTLStampedOnCreate(t *testing.T) {
	s := newTestStore()
	before := time.Now()

	evRes, errMsg := s.Upsert(context.Background(), "", "event", "public", "T", "C", "", "", "", nil, nil)
	require.Empty(t, errMsg)
	require.NotNil(t, evRes.Item.StaleAfter, "event must get a default deadline")
	assert.WithinDuration(t, before.Add(30*24*time.Hour), *evRes.Item.StaleAfter, time.Minute)

	rcRes, errMsg := s.Upsert(context.Background(), "", "recent_change", "public", "T", "C", "", "", "", nil, nil)
	require.Empty(t, errMsg)
	require.NotNil(t, rcRes.Item.StaleAfter, "recent_change must get a default deadline")
	assert.WithinDuration(t, before.Add(90*24*time.Hour), *rcRes.Item.StaleAfter, time.Minute)

	noteRes, errMsg := s.Upsert(context.Background(), "", "note", "public", "T", "C", "", "", "", nil, nil)
	require.Empty(t, errMsg)
	require.NotNil(t, noteRes.Item.StaleAfter, "note must get a default deadline")
	assert.WithinDuration(t, before.Add(90*24*time.Hour), *noteRes.Item.StaleAfter, time.Minute)

	factRes, errMsg := s.Upsert(context.Background(), "", "stable_fact", "public", "T", "C", "", "", "", nil, nil)
	require.Empty(t, errMsg)
	assert.Nil(t, factRes.Item.StaleAfter, "stable_fact has no table entry and must never get a default deadline")

	patternRes, errMsg := s.Upsert(context.Background(), "", "operational_pattern", "public", "T", "C", "", "", "", nil, nil)
	require.Empty(t, errMsg)
	assert.Nil(t, patternRes.Item.StaleAfter, "operational_pattern must never get a default deadline")
}

// TestUpsert_ExplicitStaleAfterOnCreateRespected confirms an explicit deadline
// passed on create always wins over the compiled per-kind default — it is
// respected, not silently overwritten by defaultTTLByKind.
func TestUpsert_ExplicitStaleAfterOnCreateRespected(t *testing.T) {
	s := newTestStore()
	explicit := time.Now().Add(6 * time.Hour)

	res, errMsg := s.upsertWithStaleAfter(context.Background(), "", "event", "public", "T", "C", "", "", "", nil, nil, &explicit)
	require.Empty(t, errMsg)
	require.NotNil(t, res.Item.StaleAfter)
	assert.True(t, res.Item.StaleAfter.Equal(explicit),
		"an explicit StaleAfter on create must be respected, not overwritten by the default table")
}

// TestUpsert_UpdateDoesNotRestampStaleAfter confirms updates never implicitly
// restamp, extend, or clear StaleAfter — only an explicit deadline changes it.
func TestUpsert_UpdateDoesNotRestampStaleAfter(t *testing.T) {
	s := newTestStore()

	created, errMsg := s.Upsert(context.Background(), "", "event", "public", "T", "v1", "", "", "", nil, nil)
	require.Empty(t, errMsg)
	original := *created.Item.StaleAfter

	// A plain update (no explicit staleAfter) must leave the deadline exactly
	// as it was — no restamp, no extension, no clearing.
	updated, errMsg := s.Upsert(context.Background(), created.ID, "event", "public", "T", "v2", "", "", "", nil, nil)
	require.Empty(t, errMsg)
	require.Equal(t, "updated", updated.Status)
	require.NotNil(t, updated.Item.StaleAfter)
	assert.True(t, updated.Item.StaleAfter.Equal(original), "update without an explicit deadline must not restamp StaleAfter")

	// An explicit deadline on update IS the renewal mechanism — it must apply.
	renewal := time.Now().Add(365 * 24 * time.Hour)
	renewed, errMsg := s.upsertWithStaleAfter(context.Background(), created.ID, "event", "public", "T", "v3", "", "", "", nil, nil, &renewal)
	require.Empty(t, errMsg)
	require.NotNil(t, renewed.Item.StaleAfter)
	assert.True(t, renewed.Item.StaleAfter.Equal(renewal), "an explicit StaleAfter on update must renew the deadline")
}

// TestUpsert_PriorCRBackedUpdatePreservesStaleAfter guards the "prior" path
// (an id whose CR exists but was never cached in this pod's s.items map, e.g.
// after a restart): a plain update through that path must preserve the
// persisted StaleAfter rather than dropping it via the create-shaped branch
// Upsert uses internally for it.
func TestUpsert_PriorCRBackedUpdatePreservesStaleAfter(t *testing.T) {
	created := time.Date(2026, 6, 2, 10, 19, 50, 0, time.UTC)
	stale := metav1.NewTime(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	cell := &mcell.MemoryCell{
		ObjectMeta: metav1.ObjectMeta{Name: "cell-prior-stale", Namespace: "avatar"},
		Spec: mcell.MemoryCellSpec{
			ID: "prior-stale-id", Kind: "event", Scope: "public",
			Title: "T", Content: "v1", Status: "active",
			CreatedAt: metav1.NewTime(created), StaleAfter: &stale,
		},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, mcell.AddToScheme(scheme))
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cell).
		WithIndex(&mcell.MemoryCell{}, "spec.id", func(o client.Object) []string {
			return []string{o.(*mcell.MemoryCell).Spec.ID}
		}).Build()

	s := newTestStore()
	s.SetCacheReader(reader, "avatar")

	res, errMsg := s.Upsert(context.Background(), "prior-stale-id", "event", "public", "T", "v2", "", "", "", nil, nil)
	require.Empty(t, errMsg)
	assert.Equal(t, "updated", res.Status)
	require.NotNil(t, res.Item.StaleAfter, "a CR-backed item's existing StaleAfter must survive an update this pod hadn't cached")
	assert.True(t, res.Item.StaleAfter.Equal(stale.Time))
}
