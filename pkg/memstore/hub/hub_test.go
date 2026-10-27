package hub

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recorder struct {
	mu      sync.Mutex
	hits    []HitEvent
	changes []ChangeEvent
}

func (r *recorder) PublishHit(e HitEvent) {
	r.mu.Lock()
	r.hits = append(r.hits, e)
	r.mu.Unlock()
}

func (r *recorder) PublishChange(e ChangeEvent) {
	r.mu.Lock()
	r.changes = append(r.changes, e)
	r.mu.Unlock()
}

func recv(t *testing.T, ch <-chan Frame) Frame {
	t.Helper()
	select {
	case f := <-ch:
		return f
	case <-time.After(time.Second):
		t.Fatal("no frame")
		return Frame{}
	}
}

func none(t *testing.T, ch <-chan Frame) {
	t.Helper()
	select {
	case f := <-ch:
		t.Fatalf("unexpected frame %s", f.Data)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestHub_AFrameIsTheTypedEnvelopeTheClientsRead(t *testing.T) {
	h := New(nil)
	ch, stop := h.Subscribe("")
	defer stop()

	h.PublishHit(HitEvent{ItemIDs: []string{"a", "b"}, Query: "q", Kind: "note", Kinds: []string{"note", "event"}})
	f := recv(t, ch)
	assert.Equal(t, FrameHit, f.Type)
	assert.JSONEq(t, `{"itemIds":["a","b"],"query":"q","kind":"note","kinds":["note","event"]}`, string(f.Data))

	h.PublishChange(ChangeEvent{Op: "upsert", Item: map[string]string{"id": "a"}})
	f = recv(t, ch)
	assert.Equal(t, FrameChange, f.Type)
	assert.JSONEq(t, `{"op":"upsert","item":{"id":"a"}}`, string(f.Data))

	h.PublishChange(ChangeEvent{Op: "delete", ID: "a", Kind: "note"})
	f = recv(t, ch)
	assert.JSONEq(t, `{"op":"delete","id":"a","kind":"note"}`, string(f.Data), "a delete carries no content, only the id and kind")

	h.PublishMetrics(map[string]int{"searches": 3})
	f = recv(t, ch)
	assert.Equal(t, FrameMetrics, f.Type)
	assert.JSONEq(t, `{"searches":3}`, string(f.Data))

	raw, err := json.Marshal(f)
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"metrics","data":{"searches":3}}`, string(raw))
}

func TestHub_TheTransportSeesLocalEventsAndNeverARemoteOne(t *testing.T) {
	rec := &recorder{}
	h := New(rec)
	ch, stop := h.Subscribe("")
	defer stop()

	hit := HitEvent{ItemIDs: []string{"a"}, Query: "q"}
	chg := ChangeEvent{Op: "delete", ID: "a"}
	h.PublishHit(hit)
	h.PublishChange(chg)
	assert.Equal(t, []HitEvent{hit}, rec.hits)
	assert.Equal(t, []ChangeEvent{chg}, rec.changes)
	recv(t, ch)
	recv(t, ch)

	h.ApplyRemoteHit(HitEvent{ItemIDs: []string{"b"}, Query: "remote"})
	h.ApplyRemoteChange(ChangeEvent{Op: "delete", ID: "b"})
	assert.Len(t, rec.hits, 1, "split horizon: a peer's hit is never handed back to the transport")
	assert.Len(t, rec.changes, 1)
	assert.Equal(t, FrameHit, recv(t, ch).Type, "but local clients see it")
	assert.Equal(t, FrameChange, recv(t, ch).Type)
	assert.EqualValues(t, 1, h.HitCount("b"), "and it counts")
}

func TestHub_APrivateChangeReachesItsOwnerOnlyAndNeverTheTransport(t *testing.T) {
	rec := &recorder{}
	h := New(rec)
	alice, stopA := h.Subscribe("alice")
	defer stopA()
	bob, stopB := h.Subscribe("bob")
	defer stopB()
	anon, stopN := h.Subscribe("")
	defer stopN()

	h.PublishChangeTo("alice", ChangeEvent{Op: "upsert", Item: "secret"})
	assert.Equal(t, FrameChange, recv(t, alice).Type)
	none(t, bob)
	none(t, anon)
	assert.Empty(t, rec.changes, "a scoped publish is not replicated: the event carries no owner")

	h.PublishChangeTo("", ChangeEvent{Op: "upsert", Item: "no auth"})
	for _, c := range []<-chan Frame{alice, bob, anon} {
		assert.Equal(t, FrameChange, recv(t, c).Type, "without auth the single user still gets live updates")
	}
}

func TestHub_HitCountsClientCountAndUnsubscribe(t *testing.T) {
	h := New(nil)
	assert.Zero(t, h.ClientCount())
	ch, stop := h.Subscribe("u")
	assert.Equal(t, 1, h.ClientCount())

	h.PublishHit(HitEvent{ItemIDs: []string{"x", "y"}})
	h.PublishHit(HitEvent{ItemIDs: []string{"x"}})
	assert.EqualValues(t, 2, h.HitCount("x"))
	assert.EqualValues(t, 1, h.HitCount("y"))
	assert.Zero(t, h.HitCount("never"))

	stop()
	assert.Zero(t, h.ClientCount())
	for range ch { // drain what was buffered; the channel is closed
	}
	_, open := <-ch
	assert.False(t, open)
}

func TestHub_ASlowClientDropsFramesInsteadOfStallingThePublisher(t *testing.T) {
	h := New(nil)
	_, stop := h.Subscribe("")
	defer stop()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			h.PublishChange(ChangeEvent{Op: "delete", ID: "x"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a client that never reads blocked the publisher")
	}
}

func TestHub_InstancesShareNothing(t *testing.T) {
	a, b := New(nil), New(nil)
	cha, stopA := a.Subscribe("")
	defer stopA()
	chb, stopB := b.Subscribe("")
	defer stopB()
	a.PublishHit(HitEvent{ItemIDs: []string{"x"}})
	recv(t, cha)
	none(t, chb)
	assert.EqualValues(t, 1, a.HitCount("x"))
	assert.Zero(t, b.HitCount("x"))
}

func TestHub_ConcurrentUse(t *testing.T) {
	h := New(&recorder{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, stop := h.Subscribe("u")
			defer stop()
			for j := 0; j < 50; j++ {
				h.PublishHit(HitEvent{ItemIDs: []string{"x"}})
				h.PublishChangeTo("u", ChangeEvent{Op: "delete", ID: "x"})
				select {
				case <-ch:
				default:
				}
			}
		}()
	}
	wg.Wait()
	assert.EqualValues(t, 400, h.HitCount("x"))
}
