package xray

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/triageagent-dev/x-ray/v2/pkg/memstore/hub"
)

// ── serveMemoryEvents (SSE) transport hardening ─────────────────────────────────────────

// TestHub_serveMemoryEvents_NoConnectionHeader proves serveMemoryEvents never sets the
// "Connection" header. It is an HTTP/1.1 connection-specific header field
// that RFC 9113 §8.2.2 (HTTP/2) and RFC 9114 §4.2 (HTTP/3) both forbid on a
// response; a browser reaching this endpoint through an HTTP/3 hop can get
// net::ERR_QUIC_PROTOCOL_ERROR while the header is present. This test exists
// so a future "restore it for parity" edit doesn't silently reintroduce it.
func TestHub_serveMemoryEvents_NoConnectionHeader(t *testing.T) {
	h := hub.New(nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/memory/events", nil)
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	rec := newFlushRecorder()

	done := make(chan struct{})
	go func() {
		serveMemoryEvents(h, rec, req, "")
		close(done)
	}()

	waitUntil(t, func() bool {
		return h.ClientCount() == 1
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

// TestHub_serveMemoryEvents_Heartbeat proves serveMemoryEvents writes a periodic SSE comment
// line (": ping\n\n") on its ticker branch even when no event is ever
// published — the fix for a front-door load balancer terminating idle
// backend streams, which otherwise makes a reconnect storm the steady state
// on a quiet instance. heartbeatInterval is a package var precisely so this
// test can drive it down instead of waiting out a real 15s. The ping must
// never appear as a "data: " frame — a line starting with ':' is an SSE
// comment, invisible to EventSource's onmessage, so it can never be mistaken
// for a real event on the wire.
func TestHub_serveMemoryEvents_Heartbeat(t *testing.T) {
	h := hub.New(nil)

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
		serveMemoryEvents(h, rec, req, "")
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
// http.Flusher with a mutex-guarded buffer, since serveMemoryEvents writes from its
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
	ch, stop := s.hub.Subscribe("")
	defer stop()

	s.sharedHit([]*MemoryItem{
		{ID: "pub1", Kind: "stable_fact", Scope: "public"},
		{ID: "priv1", Kind: "note", Scope: "private"},
	}, "")

	msg := string((<-ch).Data)
	assert.Contains(t, msg, "pub1")
	assert.NotContains(t, msg, "priv1")
	assert.Contains(t, msg, `"query":""`)

	s.sharedHit([]*MemoryItem{{ID: "priv2", Kind: "note", Scope: "private"}}, "")
	select {
	case m := <-ch:
		t.Fatalf("a hit with only private items must not be published, got %s", m.Data)
	default:
	}
}

// An event reaches the client as the bare JSON of the hit or the change, the
// shape the UI has always read, under a data: line.
func TestServeMemoryEvents_WritesTheBareEventAsAnSSEDataLine(t *testing.T) {
	h := hub.New(nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/memory/events", nil)
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	rec := newFlushRecorder()
	done := make(chan struct{})
	go func() {
		serveMemoryEvents(h, rec, req, "alice")
		close(done)
	}()
	waitUntil(t, func() bool { return h.ClientCount() == 1 })

	h.PublishHit(hub.HitEvent{ItemIDs: []string{"a"}, Kinds: []string{"note"}})
	h.PublishChangeTo("bob", hub.ChangeEvent{Op: "delete", ID: "not-for-alice"})
	h.PublishChangeTo("alice", hub.ChangeEvent{Op: "delete", ID: "for-alice"})
	waitUntil(t, func() bool {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return strings.Contains(rec.buf.String(), "for-alice")
	})
	cancel()
	<-done

	rec.mu.Lock()
	body := rec.buf.String()
	rec.mu.Unlock()
	assert.Contains(t, body, `data: {"itemIds":["a"],"query":"","kinds":["note"]}`+"\n\n")
	assert.Contains(t, body, `data: {"op":"delete","id":"for-alice"}`+"\n\n")
	assert.NotContains(t, body, "not-for-alice", "another user's private event never reaches this stream")
	assert.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
}
