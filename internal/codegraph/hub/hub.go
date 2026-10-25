// Package hub is a package-level singleton event hub: it broadcasts graph
// "hit" (search) and "change" (incremental update) events to connected
// WebSocket clients so the UI can render a live "graph in use" effect.
//
// Ported from an earlier multi-tenant memory hub, trimmed for a single-process
// local tool: xray-memory has no auth, no tenant/grant filtering, and no
// multi-replica gossip, so those layers (and the userHash-scoped subscribe
// variant they required) are left out entirely — every subscriber sees every
// frame.
package hub

import (
	"encoding/json"
	"sync"
	"sync/atomic"
)

// FrameType is the WebSocket envelope's explicit discriminator (see Frame).
// An explicit field beats shape-sniffing the payload: HitEvent and
// ChangeEvent don't overlap today, but a client should never have to guess
// which one it received from field presence alone.
type FrameType string

const (
	// FrameHit marks a Frame whose Data is a HitEvent.
	FrameHit FrameType = "hit"
	// FrameChange marks a Frame whose Data is a ChangeEvent.
	FrameChange FrameType = "change"
)

// Frame is the one wire envelope every subscriber receives over GET /ws:
// `{"type": "hit"|"change", "data": <payload>}`. json.RawMessage lets
// broadcast marshal the payload once and every subscriber decode it
// independently, without a second round of re-marshaling per client.
type Frame struct {
	Type FrameType       `json:"type"`
	Data json.RawMessage `json:"data"`
}

// HitEvent is broadcast whenever a search resolves (over any surface — MCP,
// HTTP /search, or a direct in-process call, see index.Index.Search's own
// comment for why that one call site is authoritative).
type HitEvent struct {
	QNs   []string `json:"qns"`
	Query string   `json:"query,omitempty"`
	// Kinds is the per-hit node kind, parallel to QNs (same order, same
	// length) — one of "Func" "Test" "Class" "Type" "File" "Doc" "Section"
	// "Manifest". Added so a client can resolve a hit's category even when
	// that node isn't currently drawn on a bounded graph view. Additive
	// (omitempty): a caller that only has QNs still compiles and behaves as
	// before with Kinds left nil. A client that finds Kinds shorter than QNs
	// must fall back to QN-only behavior rather than index out of range.
	Kinds []string `json:"kinds,omitempty"`
}

// ChangeEvent is broadcast whenever the graph is updated in place, carrying
// the paths that were actually re-indexed — never the skipped (CGO-free
// build, minified) or unowned ones, since those describe no real graph
// change a client needs to react to.
type ChangeEvent struct {
	Op    string   `json:"op"` // "update"
	Paths []string `json:"paths,omitempty"`
}

// clients is the fan-out subscriber set. hitCounts tracks per-node search-hit
// counts. Package-level state guarded by its own mutex — one hub per process,
// no instance threaded through server construction, mirroring the reference
// implementation's own package-level singleton.
var (
	mu      sync.RWMutex
	clients = map[chan Frame]struct{}{}

	hitMu     sync.RWMutex
	hitCounts = map[string]*atomic.Int64{}
)

// Subscribe registers a new fan-out subscriber and returns a receive-only
// channel of Frame envelopes plus an unsubscribe func the caller must invoke
// exactly once (typically via defer) when the connection closes.
//
// The channel is buffered at 8 and every send to it is non-blocking (see
// broadcast) — a slow reader drops frames rather than stalling a publisher.
// This is a live "in use" indicator, not a durable event log: a dropped frame
// just means one search's highlight animation is missed, not a correctness
// problem, and a publisher (every search request) must never be made to wait
// on a WebSocket write.
func Subscribe() (<-chan Frame, func()) {
	ch := make(chan Frame, 8)
	mu.Lock()
	clients[ch] = struct{}{}
	mu.Unlock()
	return ch, func() { unsubscribe(ch) }
}

func unsubscribe(ch chan Frame) {
	mu.Lock()
	delete(clients, ch)
	mu.Unlock()
	close(ch)
}

// ClientCount returns the number of currently-connected WebSocket
// subscribers, so a caller can skip sampling/publishing work when nobody is
// listening.
func ClientCount() int {
	mu.RLock()
	defer mu.RUnlock()
	return len(clients)
}

// PublishHit broadcasts a hit event to all connected subscribers and
// increments each QN's hit counter. A cheap no-op when there are no
// subscribers AND evt carries no QNs (the common case for HitCount-only
// callers is still cheap: the map lookups below are O(len(QNs))).
func PublishHit(evt HitEvent) {
	if len(evt.QNs) > 0 {
		hitMu.Lock()
		for _, qn := range evt.QNs {
			c, ok := hitCounts[qn]
			if !ok {
				c = &atomic.Int64{}
				hitCounts[qn] = c
			}
			c.Add(1)
		}
		hitMu.Unlock()
	}
	broadcast(FrameHit, evt)
}

// HitCount returns the number of times qn has been returned as a search hit.
func HitCount(qn string) int64 {
	hitMu.RLock()
	c := hitCounts[qn]
	hitMu.RUnlock()
	if c == nil {
		return 0
	}
	return c.Load()
}

// PublishChange broadcasts a graph-update event to all connected subscribers.
func PublishChange(evt ChangeEvent) {
	broadcast(FrameChange, evt)
}

// broadcast marshals v and sends it to every connected subscriber tagged with
// kind, non-blocking (a full subscriber channel drops the frame rather than
// stalling the publisher — see Subscribe's doc comment). Skips the
// marshal/lock entirely when there are no subscribers, since PublishHit/
// PublishChange are called from every search/update regardless of whether
// anyone is watching.
func broadcast(kind FrameType, v any) {
	mu.RLock()
	n := len(clients)
	if n == 0 {
		mu.RUnlock()
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		mu.RUnlock()
		return
	}
	f := Frame{Type: kind, Data: b}
	for ch := range clients {
		select {
		case ch <- f:
		default:
		}
	}
	mu.RUnlock()
}

// ResetForTest restores the hub to its zero state (no clients, no hit
// counts) so tests don't leak state across each other through this
// package-level singleton.
func ResetForTest() {
	mu.Lock()
	clients = map[chan Frame]struct{}{}
	mu.Unlock()
	hitMu.Lock()
	hitCounts = map[string]*atomic.Int64{}
	hitMu.Unlock()
}
