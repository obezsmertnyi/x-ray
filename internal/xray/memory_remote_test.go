package xray

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	mcell "github.com/triageagent-dev/x-ray/v2/api/v1"
	"github.com/triageagent-dev/x-ray/v2/pkg/memstore/hub"
)

// events drains what the hub has published so far, decoded.
func events(t *testing.T, ch <-chan hub.Frame) []changeEvent {
	t.Helper()
	var out []changeEvent
	for {
		select {
		case f := <-ch:
			if f.Type != hub.FrameChange {
				continue
			}
			var e changeEvent
			require.NoError(t, json.Unmarshal(f.Data, &e))
			out = append(out, e)
		case <-time.After(20 * time.Millisecond):
			return out
		}
	}
}

// changeEvent is a change frame with the item decoded as the wire item.
type changeEvent struct {
	Op   string      `json:"op"`
	Item *MemoryItem `json:"item,omitempty"`
	ID   string      `json:"id,omitempty"`
}

func reconcile(t *testing.T, s *CellSyncer, name string) {
	t.Helper()
	_, err := s.Reconcile(context.Background(), ctrl.Request{NamespacedName: k8stypes.NamespacedName{Namespace: "ns", Name: name}})
	require.NoError(t, err)
}

// A cell another replica (or kubectl) created is indexed here and announced to
// this pod's live clients exactly once, and its deletion is applied too.
func TestCellSyncer_FollowsCellsWrittenElsewhere(t *testing.T) {
	syncer, _ := newTrackedCellSyncer(t)
	ctx := context.Background()
	ch, cancel := syncer.memory.hub.Subscribe("")
	defer cancel()

	now := metav1.Now()
	cell := &mcell.MemoryCell{
		ObjectMeta: metav1.ObjectMeta{Name: cellResourceName("elsewhere"), Namespace: "ns"},
		Spec: mcell.MemoryCellSpec{ID: "elsewhere", Kind: "note", Scope: "public", Title: "Remote note",
			Content: "written on another replica", Status: "active", CreatedAt: now, UpdatedAt: now},
	}
	require.NoError(t, syncer.Create(ctx, cell))

	reconcile(t, syncer, cell.Name)
	got := events(t, ch)
	require.Len(t, got, 1)
	assert.Equal(t, "upsert", got[0].Op)
	assert.Equal(t, "elsewhere", got[0].Item.ID)
	assert.Equal(t, []string{"elsewhere"}, idsOf(syncer.memory.KeywordSearch("remote note", "", "", 5)), "the keyword index follows the cell")

	reconcile(t, syncer, cell.Name)
	assert.Empty(t, events(t, ch), "an unchanged cell is not announced again")

	var live mcell.MemoryCell
	require.NoError(t, syncer.Get(ctx, k8stypes.NamespacedName{Namespace: "ns", Name: cell.Name}, &live))
	live.Spec.Content = "edited elsewhere"
	require.NoError(t, syncer.Update(ctx, &live))
	reconcile(t, syncer, cell.Name)
	got = events(t, ch)
	require.Len(t, got, 1, "a changed cell is")
	assert.Equal(t, "edited elsewhere", got[0].Item.Content)

	require.NoError(t, syncer.Delete(ctx, &live))
	reconcile(t, syncer, cell.Name)
	got = events(t, ch)
	require.Len(t, got, 1)
	assert.Equal(t, "delete", got[0].Op)
	assert.Equal(t, "elsewhere", got[0].ID)
	assert.Empty(t, idsOf(syncer.memory.KeywordSearch("remote note", "", "", 5)), "the deleted cell leaves the keyword index")
}

func TestCellSyncer_ItsOwnWriteIsNotAnnouncedTwice(t *testing.T) {
	syncer, _ := newTrackedCellSyncer(t)
	ctx := context.Background()
	mem := syncer.memory
	res, errMsg := mem.Upsert(ctx, "mine", "note", "public", "Own note", "written here", "", "", "", nil, nil)
	require.Empty(t, errMsg)

	ch, cancel := mem.hub.Subscribe("")
	defer cancel()
	require.NoError(t, syncer.Persist(ctx, res.Item, "ns"))
	reconcile(t, syncer, cellResourceName("mine"))
	assert.Empty(t, events(t, ch), "the watch event for this pod's own write is recognised")

	mem.Delete("mine")
	require.NoError(t, syncer.Remove(ctx, "mine", "ns"))
	ch2, cancel2 := mem.hub.Subscribe("")
	defer cancel2()
	reconcile(t, syncer, cellResourceName("mine"))
	assert.Empty(t, events(t, ch2), "and so is the delete")
}

func TestCellSyncer_RetiredElsewhereLeavesTheIndexes(t *testing.T) {
	syncer, _ := newTrackedCellSyncer(t)
	ctx := context.Background()
	now := metav1.Now()
	cell := &mcell.MemoryCell{
		ObjectMeta: metav1.ObjectMeta{Name: cellResourceName("r1"), Namespace: "ns"},
		Spec: mcell.MemoryCellSpec{ID: "r1", Kind: "note", Scope: "public", Title: "Retire me",
			Content: "soon stale", Status: "active", CreatedAt: now, UpdatedAt: now},
	}
	require.NoError(t, syncer.Create(ctx, cell))
	reconcile(t, syncer, cell.Name)
	require.NotEmpty(t, syncer.memory.KeywordSearch("retire me", "", "", 5))

	ch, cancel := syncer.memory.hub.Subscribe("")
	defer cancel()
	var live mcell.MemoryCell
	require.NoError(t, syncer.Get(ctx, k8stypes.NamespacedName{Namespace: "ns", Name: cell.Name}, &live))
	live.Spec.Status = "stale"
	require.NoError(t, syncer.Update(ctx, &live))
	reconcile(t, syncer, cell.Name)

	assert.Empty(t, syncer.memory.KeywordSearch("retire me", "", "", 5))
	got := events(t, ch)
	require.Len(t, got, 1)
	assert.Equal(t, "delete", got[0].Op)
}

func idsOf(items []*MemoryItem) []string {
	var ids []string
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	return ids
}
