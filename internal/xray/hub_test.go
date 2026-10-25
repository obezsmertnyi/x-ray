package xray

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestHub() *Hub {
	return &Hub{
		clients:   make(map[chan []byte]string),
		hitCounts: make(map[string]*atomic.Int64),
	}
}

// ── subscribe / unsubscribe ───────────────────────────────────────────────────

func TestHub_SubscribeUnsubscribe(t *testing.T) {
	h := newTestHub()
	ch := h.subscribe()
	require.NotNil(t, ch)
	assert.Len(t, h.clients, 1)

	h.unsubscribe(ch)
	assert.Len(t, h.clients, 0)

	_, ok := <-ch
	assert.False(t, ok, "channel should be closed after unsubscribe")
}

func TestHub_MultipleSubscribers(t *testing.T) {
	h := newTestHub()
	ch1 := h.subscribe()
	ch2 := h.subscribe()
	ch3 := h.subscribe()

	assert.Len(t, h.clients, 3)

	h.unsubscribe(ch2)
	assert.Len(t, h.clients, 2)

	h.unsubscribe(ch1)
	h.unsubscribe(ch3)
	assert.Len(t, h.clients, 0)
}

// ── Publish (MemoryHitEvent) ──────────────────────────────────────────────────

func TestHub_PublishHitEvent(t *testing.T) {
	h := newTestHub()
	ch := h.subscribe()
	defer h.unsubscribe(ch)

	evt := MemoryHitEvent{ItemIDs: []string{"id1", "id2"}, Query: "go routines", Kind: "stable_fact"}
	h.Publish(evt)

	raw := <-ch
	var got MemoryHitEvent
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, evt.ItemIDs, got.ItemIDs)
	assert.Equal(t, evt.Query, got.Query)
	assert.Equal(t, evt.Kind, got.Kind)
}

// TestHub_PublishHitEvent_KindsRoundTrips proves the per-item Kinds slice
// (parallel to ItemIDs) survives a full Publish → SSE JSON → Unmarshal round
// trip in order, unmodified — this is what lets the UI resolve a hit's
// region even when the hit item isn't one of the currently-rendered graph
// nodes (see MemoryHitEvent.Kinds doc comment).
func TestHub_PublishHitEvent_KindsRoundTrips(t *testing.T) {
	h := newTestHub()
	ch := h.subscribe()
	defer h.unsubscribe(ch)

	evt := MemoryHitEvent{
		ItemIDs: []string{"id1", "id2", "id3"},
		Query:   "go routines",
		Kinds:   []string{"stable_fact", "event", "active_concern"},
	}
	h.Publish(evt)

	raw := <-ch
	assert.Contains(t, string(raw), `"kinds":["stable_fact","event","active_concern"]`)

	var got MemoryHitEvent
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, evt.ItemIDs, got.ItemIDs)
	assert.Equal(t, evt.Kinds, got.Kinds)
	require.Len(t, got.Kinds, len(got.ItemIDs), "Kinds must stay parallel to ItemIDs")
}

// TestHub_PublishHitEvent_KindsOmittedWhenAbsent proves an event published
// with no Kinds (the zero value, e.g. from a caller that hasn't been ported
// yet) serializes with the field entirely absent — `omitempty` — so an
// older client that has never heard of "kinds" sees exactly the wire shape
// it always has, and a newer client can distinguish "no kinds supplied"
// (degrade to cell-only) from "kinds supplied but empty".
func TestHub_PublishHitEvent_KindsOmittedWhenAbsent(t *testing.T) {
	h := newTestHub()
	ch := h.subscribe()
	defer h.unsubscribe(ch)

	h.Publish(MemoryHitEvent{ItemIDs: []string{"id1"}, Query: "q"})

	raw := <-ch
	assert.NotContains(t, string(raw), "kinds")
}

// ── PublishChange (MemoryChangeEvent) ────────────────────────────────────────

func TestHub_PublishChangeUpsert(t *testing.T) {
	h := newTestHub()
	ch := h.subscribe()
	defer h.unsubscribe(ch)

	item := &MemoryItem{ID: "abc", Title: "Test", Kind: "stable_fact"}
	h.PublishChange(MemoryChangeEvent{Op: "upsert", Item: item})

	raw := <-ch
	var got MemoryChangeEvent
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, "upsert", got.Op)
	require.NotNil(t, got.Item)
	assert.Equal(t, "abc", got.Item.ID)
}

func TestHub_PublishChangeDelete(t *testing.T) {
	h := newTestHub()
	ch := h.subscribe()
	defer h.unsubscribe(ch)

	h.PublishChange(MemoryChangeEvent{Op: "delete", ID: "abc"})

	raw := <-ch
	var got MemoryChangeEvent
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, "delete", got.Op)
	assert.Equal(t, "abc", got.ID)
}

// ── PublishChangeTo (owner-scoped, e.g. private notes) ───────────────────────

// A private note must reach only its owner's channel, never other users'.
func TestHub_PublishChangeTo_OwnerOnly(t *testing.T) {
	h := newTestHub()
	alice := h.subscribeAs("alicehash")
	bob := h.subscribeAs("bobhash")
	defer h.unsubscribe(alice)
	defer h.unsubscribe(bob)

	note := &MemoryItem{ID: "n1", Title: "Alice secret", Kind: "note", Scope: scopePrivate, Subject: "alicehash"}
	h.PublishChangeTo("alicehash", MemoryChangeEvent{Op: "upsert", Item: note})

	raw := <-alice
	var got MemoryChangeEvent
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, "n1", got.Item.ID, "owner must receive their note")

	select {
	case leaked := <-bob:
		t.Fatalf("bob must not receive alice's private note, got: %s", leaked)
	default:
	}
}

// With an empty userHash (auth disabled / local dev), the event falls back to a
// full broadcast so the single local user still sees it.
func TestHub_PublishChangeTo_EmptyHashBroadcasts(t *testing.T) {
	h := newTestHub()
	ch := h.subscribeAs("")
	defer h.unsubscribe(ch)

	h.PublishChangeTo("", MemoryChangeEvent{Op: "upsert", Item: &MemoryItem{ID: "n2"}})

	raw := <-ch
	var got MemoryChangeEvent
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, "n2", got.Item.ID)
}

// ── Broadcast to all subscribers ─────────────────────────────────────────────

func TestHub_BroadcastToAll(t *testing.T) {
	h := newTestHub()
	const n = 10
	channels := make([]chan []byte, n)
	for i := range n {
		channels[i] = h.subscribe()
	}

	h.Publish(MemoryHitEvent{Query: "broadcast test"})

	for i, ch := range channels {
		raw := <-ch
		var got MemoryHitEvent
		require.NoError(t, json.Unmarshal(raw, &got), "subscriber %d", i)
		assert.Equal(t, "broadcast test", got.Query)
		h.unsubscribe(ch)
	}
}

// ── Non-blocking on slow subscriber ──────────────────────────────────────────

func TestHub_NonBlockingOnSlowSubscriber(t *testing.T) {
	h := newTestHub()
	// Buffer size is 8; don't drain it — publish 16 events.
	slow := h.subscribe()
	defer h.unsubscribe(slow)

	fast := h.subscribe()
	defer h.unsubscribe(fast)

	// This must complete without blocking even though slow's buffer fills up.
	for i := range 16 {
		h.Publish(MemoryHitEvent{Query: "msg", Kind: string(rune('a' + i))})
	}
	// fast receives up to its buffer; exact count depends on timing — just verify no deadlock.
}

// ── Concurrent publish + subscribe ───────────────────────────────────────────

func TestHub_ConcurrentPublish(t *testing.T) {
	h := newTestHub()
	ch := h.subscribe()

	var received atomic.Int64
	var wg sync.WaitGroup

	// Drain goroutine — exits when channel is closed by unsubscribe.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range ch {
			received.Add(1)
		}
	}()

	// 20 concurrent publishers
	var pub sync.WaitGroup
	const publishers = 20
	for range publishers {
		pub.Add(1)
		go func() {
			defer pub.Done()
			h.Publish(MemoryHitEvent{Query: "concurrent"})
		}()
	}
	pub.Wait()

	h.unsubscribe(ch) // closes ch → drain goroutine exits
	wg.Wait()

	// Channel buffer is 8; some drops are expected under concurrent flood.
	// The key assertion is that no panic or deadlock occurred.
	assert.GreaterOrEqual(t, received.Load(), int64(1))
}

// ── No panic on publish with zero subscribers ─────────────────────────────────

func TestHub_PublishNoSubscribers(t *testing.T) {
	h := newTestHub()
	assert.NotPanics(t, func() {
		h.Publish(MemoryHitEvent{Query: "nobody home"})
		h.PublishChange(MemoryChangeEvent{Op: "delete", ID: "x"})
	})
}

// ── Replication hook surface (ported from a sibling project's memoryhub):
// SetHitPublishHook/SetChangePublishHook fire exactly once, with the event
// payload, at the end of a LOCAL Publish/PublishChange call; ApplyRemoteHit/
// ApplyRemoteChange deliver to local subscribers identically but must never
// re-arm the hook (split horizon — the whole point is that a replication
// transport built on top of this can't loop a remote event back out). Unset
// hooks (the default) must not change existing Publish/PublishChange
// behavior at all. ─────────────────────────────────────────────────────────

// A LOCAL Publish must fire the hit hook exactly once, with the exact event
// payload passed to Publish.
func TestHub_Publish_FiresHitHookOnceWithPayload(t *testing.T) {
	h := newTestHub()
	var got []MemoryHitEvent
	h.SetHitPublishHook(func(evt MemoryHitEvent) { got = append(got, evt) })

	evt := MemoryHitEvent{ItemIDs: []string{"id1", "id2"}, Query: "go routines", Kind: "stable_fact"}
	h.Publish(evt)

	require.Len(t, got, 1, "hook must fire exactly once per local Publish")
	assert.Equal(t, evt, got[0], "hook must receive the exact event payload")
}

// A LOCAL PublishChange must fire the change hook exactly once, with the
// exact event payload passed to PublishChange.
func TestHub_PublishChange_FiresChangeHookOnceWithPayload(t *testing.T) {
	h := newTestHub()
	var got []MemoryChangeEvent
	h.SetChangePublishHook(func(evt MemoryChangeEvent) { got = append(got, evt) })

	item := &MemoryItem{ID: "abc", Title: "Test", Kind: "stable_fact"}
	evt := MemoryChangeEvent{Op: "upsert", Item: item}
	h.PublishChange(evt)

	require.Len(t, got, 1, "hook must fire exactly once per local PublishChange")
	assert.Equal(t, evt, got[0], "hook must receive the exact event payload")
}

// ApplyRemoteHit must deliver the same local effects as Publish (hitCounts +
// broadcast to subscribers) but must NEVER fire the hit-publish hook — the
// split-horizon half of the contract that keeps a replication mesh from
// looping a hit back and forth between replicas forever.
func TestHub_ApplyRemoteHit_DoesNotFireHook(t *testing.T) {
	h := newTestHub()
	hookFired := false
	h.SetHitPublishHook(func(MemoryHitEvent) { hookFired = true })

	h.ApplyRemoteHit(MemoryHitEvent{ItemIDs: []string{"id1"}, Query: "remote hit"})

	assert.False(t, hookFired, "ApplyRemoteHit must never re-arm the hit-publish hook")
	assert.Equal(t, int64(1), h.HitCount("id1"), "ApplyRemoteHit must still update local hit counters")
}

// ApplyRemoteChange must deliver the same local broadcast as PublishChange
// but must NEVER fire the change-publish hook — split horizon, same
// contract as ApplyRemoteHit.
func TestHub_ApplyRemoteChange_DoesNotFireHook(t *testing.T) {
	h := newTestHub()
	hookFired := false
	h.SetChangePublishHook(func(MemoryChangeEvent) { hookFired = true })

	ch := h.subscribe()
	defer h.unsubscribe(ch)

	h.ApplyRemoteChange(MemoryChangeEvent{Op: "delete", ID: "remote-1"})

	assert.False(t, hookFired, "ApplyRemoteChange must never re-arm the change-publish hook")
	raw := <-ch
	var got MemoryChangeEvent
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, "remote-1", got.ID)
}

// A hit applied via ApplyRemoteHit must reach subscribed SSE clients
// identically to a locally-Published one — same wire shape, same delivery.
func TestHub_ApplyRemoteHit_ReachesSubscribersLikeLocal(t *testing.T) {
	h := newTestHub()
	ch := h.subscribe()
	defer h.unsubscribe(ch)

	h.ApplyRemoteHit(MemoryHitEvent{ItemIDs: []string{"id9"}, Query: "from a peer", Kind: "note"})

	raw := <-ch
	var got MemoryHitEvent
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, []string{"id9"}, got.ItemIDs)
	assert.Equal(t, "from a peer", got.Query)
	assert.Equal(t, "note", got.Kind)
}

// A change applied via ApplyRemoteChange must reach subscribed SSE clients
// identically to a locally-PublishChange'd one.
func TestHub_ApplyRemoteChange_ReachesSubscribersLikeLocal(t *testing.T) {
	h := newTestHub()
	ch := h.subscribe()
	defer h.unsubscribe(ch)

	item := &MemoryItem{ID: "peer-item", Title: "From peer", Kind: "note"}
	h.ApplyRemoteChange(MemoryChangeEvent{Op: "upsert", Item: item})

	raw := <-ch
	var got MemoryChangeEvent
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, "upsert", got.Op)
	require.NotNil(t, got.Item)
	assert.Equal(t, "peer-item", got.Item.ID)
}

// Unset hooks (the default, and every single-replica deployment) must cost
// nothing: Publish/PublishChange behave exactly as before this surface
// existed, with no panic and no observable difference in local delivery.
func TestHub_UnsetHooksAreSafe(t *testing.T) {
	h := newTestHub()
	ch := h.subscribe()
	defer h.unsubscribe(ch)

	assert.NotPanics(t, func() {
		h.Publish(MemoryHitEvent{Query: "no hook installed"})
		h.PublishChange(MemoryChangeEvent{Op: "delete", ID: "y"})
	})

	raw1 := <-ch
	var hit MemoryHitEvent
	require.NoError(t, json.Unmarshal(raw1, &hit))
	assert.Equal(t, "no hook installed", hit.Query)

	raw2 := <-ch
	var change MemoryChangeEvent
	require.NoError(t, json.Unmarshal(raw2, &change))
	assert.Equal(t, "y", change.ID)
}

// Setting a hook then clearing it with nil must restore the unset (no-op)
// behavior — SetHitPublishHook/SetChangePublishHook must be safe to toggle.
func TestHub_SetPublishHooks_NilClears(t *testing.T) {
	h := newTestHub()
	calls := 0
	h.SetHitPublishHook(func(MemoryHitEvent) { calls++ })
	h.Publish(MemoryHitEvent{Query: "first"})
	assert.Equal(t, 1, calls)

	h.SetHitPublishHook(nil)
	assert.NotPanics(t, func() {
		h.Publish(MemoryHitEvent{Query: "second"})
	})
	assert.Equal(t, 1, calls, "cleared hook must not fire again")
}

// ── ServeWS (SSE) transport hardening ─────────────────────────────────────────

// TestHub_ServeWS_NoConnectionHeader proves ServeWS never sets the
// "Connection" header. It is an HTTP/1.1 connection-specific header field
// that RFC 9113 §8.2.2 (HTTP/2) and RFC 9114 §4.2 (HTTP/3) both forbid on a
// response; a browser reaching this endpoint through an HTTP/3 hop can get
// net::ERR_QUIC_PROTOCOL_ERROR while the header is present. This test exists
// so a future "restore it for parity" edit doesn't silently reintroduce it.
func TestHub_ServeWS_NoConnectionHeader(t *testing.T) {
	h := newTestHub()

	req := httptest.NewRequest(http.MethodGet, "/v1/memory/events", nil)
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	rec := newFlushRecorder()

	done := make(chan struct{})
	go func() {
		h.ServeWS(rec, req, "")
		close(done)
	}()

	waitUntil(t, func() bool {
		h.mu.RLock()
		defer h.mu.RUnlock()
		return len(h.clients) == 1
	})
	cancel()
	<-done

	if got := rec.Header().Get("Connection"); got != "" {
		t.Errorf(`Connection header = %q, want unset — it is a connection-specific field forbidden on HTTP/2 and HTTP/3 responses (RFC 9113 §8.2.2, RFC 9114 §4.2)`, got)
	}
	for _, hdr := range []string{"Keep-Alive", "Transfer-Encoding", "Upgrade"} {
		if got := rec.Header().Get(hdr); got != "" {
			t.Errorf("%s header = %q, want unset — also connection-specific and forbidden on HTTP/2/3", hdr, got)
		}
	}
}

// TestHub_ServeWS_Heartbeat proves ServeWS writes a periodic SSE comment
// line (": ping\n\n") on its ticker branch even when no event is ever
// published — the fix for a front-door load balancer terminating idle
// backend streams, which otherwise makes a reconnect storm the steady state
// on a quiet instance. heartbeatInterval is a package var precisely so this
// test can drive it down instead of waiting out a real 15s. The ping must
// never appear as a "data: " frame — a line starting with ':' is an SSE
// comment, invisible to EventSource's onmessage, so it can never be mistaken
// for a real event on the wire.
func TestHub_ServeWS_Heartbeat(t *testing.T) {
	h := newTestHub()

	orig := heartbeatInterval
	heartbeatInterval = 20 * time.Millisecond
	defer func() { heartbeatInterval = orig }()

	req := httptest.NewRequest(http.MethodGet, "/v1/memory/events", nil)
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	rec := newFlushRecorder()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		h.ServeWS(rec, req, "")
	}()

	waitUntil(t, func() bool {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		// Wait for at least one ping beyond the initial ": connected" line.
		return strings.Count(rec.buf.String(), ": ping\n\n") >= 1
	})
	cancel()
	wg.Wait()

	rec.mu.Lock()
	body := rec.buf.String()
	rec.mu.Unlock()

	if !strings.Contains(body, ": ping\n\n") {
		t.Fatalf("expected at least one heartbeat comment line, got body=%q", body)
	}
	if strings.Contains(body, "data: ") {
		t.Fatalf("no event was published, so no data: frame should have been written: %q", body)
	}
	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, ":") {
			t.Fatalf("unexpected non-comment, non-data line in SSE stream: %q", line)
		}
	}
}

// flushRecorder is an httptest.ResponseRecorder alternative that implements
// http.Flusher with a mutex-guarded buffer, since ServeWS writes from its
// own goroutine while the test reads concurrently.
type flushRecorder struct {
	mu     sync.Mutex
	header http.Header
	buf    strings.Builder
	code   int
}

func newFlushRecorder() *flushRecorder {
	return &flushRecorder{header: http.Header{}, code: http.StatusOK}
}

func (r *flushRecorder) Header() http.Header { return r.header }

func (r *flushRecorder) Write(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.WriteString(string(b))
}

func (r *flushRecorder) WriteHeader(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.code = code
}

func (r *flushRecorder) Flush() {}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}

// A voice or MCP recall reaches every connected client, so the event names shared
// items only and never carries the query that found them.
func TestSharedHit_OmitsPrivateItemsAndQuery(t *testing.T) {
	s := NewMemoryStore()
	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	s.sharedHit([]*MemoryItem{
		{ID: "pub1", Kind: "stable_fact", Scope: "public"},
		{ID: "priv1", Kind: "note", Scope: "private"},
	}, "")

	msg := string(<-ch)
	assert.Contains(t, msg, "pub1")
	assert.NotContains(t, msg, "priv1")
	assert.Contains(t, msg, `"query":""`)

	s.sharedHit([]*MemoryItem{{ID: "priv2", Kind: "note", Scope: "private"}}, "")
	select {
	case m := <-ch:
		t.Fatalf("a hit with only private items must not be published, got %s", m)
	default:
	}
}
