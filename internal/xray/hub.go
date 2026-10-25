package xray

import (
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// heartbeatInterval is the cadence of the SSE keepalive comment ServeWS
// writes on the ticker branch of its select loop. Comfortably under a
// typical L7 load balancer's ~40s idle backend timeout, so a reconnect
// becomes the exception rather than the steady state on a quiet instance. A
// package-level var rather than a const so a test can drive it down and
// observe a heartbeat fire within a bounded wait instead of sleeping out a
// real 15s.
var heartbeatInterval = 15 * time.Second

// MemoryHitEvent is broadcast over SSE whenever search_memory is called.
type MemoryHitEvent struct {
	ItemIDs []string `json:"itemIds"`
	Query   string   `json:"query"`
	Kind    string   `json:"kind,omitempty"` // the search's kind FILTER, usually empty — not a per-item kind, see Kinds
	// Kinds is the per-item kind, parallel to ItemIDs (same order, same
	// length) — lets a client resolve each hit's UI category (see
	// ui/lib/category-config.ts categoryOf) even when the item itself isn't
	// currently rendered on the bounded graph preview (MAX_RENDER_NODES caps
	// what's drawn; hits land overwhelmingly on kinds like event/
	// active_concern that dwarf the per-kind render budget, so without this
	// the flash effect was a no-op for the overwhelming majority of hits).
	// Purely additive (`omitempty`; empty/absent on the wire when a caller
	// passes nil): an older client ignores an unknown field. A caller MUST
	// fill it from the same items it derived ItemIDs from, in the same
	// order. A client that finds Kinds absent or shorter than ItemIDs must
	// fall back to cell-only behavior rather than index out of range.
	Kinds []string `json:"kinds,omitempty"`
}

// MemoryChangeEvent is broadcast over SSE when a memory item is created, updated, or deleted.
type MemoryChangeEvent struct {
	Op   string      `json:"op"`             // "upsert" | "delete"
	Item *MemoryItem `json:"item,omitempty"` // set for op=upsert
	ID   string      `json:"id,omitempty"`   // set for op=delete
}

// Hub broadcasts memory events to connected SSE clients. Clients already hold a
// session token gated by requireSession middleware, so the hub does not
// re-authenticate — but it records each client's userHash so private events
// (e.g. personal notes) can be delivered to their owner alone instead of being
// broadcast to every client. Shared events (hits, non-private changes) still fan
// out to all clients.
//
// hitHook/changeHook are the replication seam for a future multi-replica
// deployment (ported from
// a sibling project's memoryhub package — see SetHitPublishHook
// for the full rationale). Unlike that package, this Hub is instance-based
// rather than a package-level singleton, so the hooks are plain struct
// fields/methods instead of package vars/funcs — whatever embeds a mesh
// transport holds the *Hub and wires its own forwarding closure onto it, with
// no risk of one process's hooks leaking into another's (a real concern for a
// package singleton, moot here).
type Hub struct {
	mu        sync.RWMutex
	clients   map[chan []byte]string // channel → owner userHash ("" = anonymous/auth-disabled)
	hitMu     sync.RWMutex
	hitCounts map[string]*atomic.Int64 // item ID → SSE hit count

	hookMu     sync.RWMutex
	hitHook    func(MemoryHitEvent)
	changeHook func(MemoryChangeEvent)
}

// SetHitPublishHook installs (or clears with nil) the callback invoked at the
// end of every LOCALLY-originated Publish call — never for an event applied
// via ApplyRemoteHit (see that method's doc comment). That asymmetry is the
// split-horizon rule a replication transport needs: a hit received from a
// peer is delivered to this process's own SSE subscribers but never handed
// back to this hook, so a mesh built on top of it cannot loop a hit back and
// forth between replicas forever. The intended caller is whatever embeds a
// future replication transport (a mesh client, a pub/sub bridge, …) — it
// installs a closure that forwards evt onto that transport's outbound send.
// Nil-safe: with no hook installed (the default, and every single-replica
// deployment), Publish behaves exactly as it did before this method existed —
// zero added cost, zero behavior change.
func (h *Hub) SetHitPublishHook(fn func(MemoryHitEvent)) {
	h.hookMu.Lock()
	h.hitHook = fn
	h.hookMu.Unlock()
}

func (h *Hub) getHitPublishHook() func(MemoryHitEvent) {
	h.hookMu.RLock()
	defer h.hookMu.RUnlock()
	return h.hitHook
}

// SetChangePublishHook installs (or clears with nil) the callback invoked at
// the end of every LOCALLY-originated PublishChange call — the change-event
// analogue of SetHitPublishHook, with the identical split-horizon contract:
// never invoked for a change applied via ApplyRemoteChange.
//
// PublishChangeTo (the userHash-scoped variant, used here for private items
// such as personal notes) deliberately has NO hook of its own, mirroring the
// reference implementation's choice even though the reasons differ slightly:
// the reference has nothing calling PublishChangeTo at all yet, so there was
// nothing to forward; this Hub's callers DO use PublishChangeTo today
// (memory.go's private-item upsert/delete paths), but MemoryChangeEvent
// carries no userHash on the wire — only the recipient-side subscriber map
// knows who owns a channel. A peer replica applying a forwarded private event
// via ApplyRemoteChange would have no owner to scope it to and would fall
// back to broadcasting it to every one of ITS subscribers, leaking the
// private item to users who never should have seen it. Replicating a scoped
// publish is only safe once the userHash travels with the event itself — a
// future private-scope replication feature needs that wire-format change
// first, not just a hook here.
func (h *Hub) SetChangePublishHook(fn func(MemoryChangeEvent)) {
	h.hookMu.Lock()
	h.changeHook = fn
	h.hookMu.Unlock()
}

func (h *Hub) getChangePublishHook() func(MemoryChangeEvent) {
	h.hookMu.RLock()
	defer h.hookMu.RUnlock()
	return h.changeHook
}

// subscribe registers an anonymous client channel (no owner) — it receives only
// broadcast events, never user-scoped ones. Convenience wrapper over subscribeAs.
func (h *Hub) subscribe() chan []byte { return h.subscribeAs("") }

// subscribeAs registers a client channel owned by userHash (empty when auth is
// disabled). The userHash scopes who receives PublishChangeTo events.
func (h *Hub) subscribeAs(userHash string) chan []byte {
	ch := make(chan []byte, 8)
	h.mu.Lock()
	h.clients[ch] = userHash
	h.mu.Unlock()
	return ch
}

func (h *Hub) unsubscribe(ch chan []byte) {
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
	close(ch)
}

// Publish sends an event to all connected SSE clients. Non-blocking.
// Also increments per-item hit counters for demand ranking. Call this for a
// LOCALLY-originated hit only — every existing call site already does, this
// is unchanged. After the local effects land, the hit-publish hook (if any)
// fires so a replication transport can forward the event to peer replicas —
// see SetHitPublishHook and ApplyRemoteHit.
func (h *Hub) Publish(evt MemoryHitEvent) {
	h.applyHitLocal(evt)
	if hook := h.getHitPublishHook(); hook != nil {
		hook(evt)
	}
}

// ApplyRemoteHit applies a hit event received from a PEER replica: the exact
// same local effects as Publish (increment hitCounts, broadcast to this
// process's own subscribers) but WITHOUT invoking the hit-publish hook. This
// omission is the split-horizon half of the contract described on
// SetHitPublishHook — a remote apply must never be re-forwarded, or two
// replicas would echo the same hit back and forth forever. Intended to be
// called only from a replication transport's inbound handler, never from any
// of this package's own call sites.
func (h *Hub) ApplyRemoteHit(evt MemoryHitEvent) {
	h.applyHitLocal(evt)
}

// applyHitLocal is the shared local-effect core of Publish/ApplyRemoteHit:
// bump per-item hit counters and broadcast to this process's subscribers.
func (h *Hub) applyHitLocal(evt MemoryHitEvent) {
	h.hitMu.Lock()
	for _, id := range evt.ItemIDs {
		if _, ok := h.hitCounts[id]; !ok {
			h.hitCounts[id] = &atomic.Int64{}
		}
		h.hitCounts[id].Add(1)
	}
	h.hitMu.Unlock()
	h.broadcast(evt)
}

// HitCount returns the number of times the item with the given ID has been hit by search.
func (h *Hub) HitCount(id string) int64 {
	h.hitMu.RLock()
	c := h.hitCounts[id]
	h.hitMu.RUnlock()
	if c == nil {
		return 0
	}
	return c.Load()
}

// PublishChange sends a memory change event to all connected SSE clients. Non-blocking.
// Use only for shared (non-private) items — every authenticated client receives it.
// Call this for a LOCALLY-originated change only. After the local broadcast,
// the change-publish hook (if any) fires so a replication transport can
// forward the event to peer replicas — see SetChangePublishHook and
// ApplyRemoteChange.
func (h *Hub) PublishChange(evt MemoryChangeEvent) {
	h.broadcast(evt)
	if hook := h.getChangePublishHook(); hook != nil {
		hook(evt)
	}
}

// ApplyRemoteChange applies a change event received from a PEER replica: the
// same local broadcast as PublishChange but WITHOUT invoking the
// change-publish hook, enforcing split horizon exactly like ApplyRemoteHit.
// Intended to be called only from a replication transport's inbound handler.
func (h *Hub) ApplyRemoteChange(evt MemoryChangeEvent) {
	h.broadcast(evt)
}

// PublishChangeTo sends a memory change event only to clients owned by userHash —
// the channel for private items (personal notes) whose content must not reach
// other users. When userHash is empty (auth disabled), it falls back to a full
// broadcast so the single local user still receives live updates.
func (h *Hub) PublishChangeTo(userHash string, evt MemoryChangeEvent) {
	if userHash == "" {
		h.broadcast(evt)
		return
	}
	b, _ := json.Marshal(evt)
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch, owner := range h.clients {
		if owner != userHash {
			continue
		}
		select {
		case ch <- b:
		default:
		}
	}
}

func (h *Hub) broadcast(v any) {
	b, _ := json.Marshal(v)
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch := range h.clients {
		select {
		case ch <- b:
		default:
		}
	}
}

// ServeWS upgrades the connection and streams memory events as SSE. The channel
// is registered under userHash so private events (e.g. personal notes) reach only
// their owner; pass "" for an anonymous (broadcast-only) subscription.
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request, userHash string) {
	// Use server-sent events — no external WS library needed and works through proxies.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	// Deliberately NOT setting "Connection: keep-alive" here. Connection is
	// an HTTP/1.1 connection-specific header field; RFC 9113 §8.2.2 (HTTP/2)
	// and RFC 9114 §4.2 (HTTP/3) both forbid connection-specific fields
	// (Connection, Keep-Alive, Transfer-Encoding, Upgrade) and require a
	// compliant receiver to treat one as malformed. A browser reaching this
	// endpoint through an HTTP/3 hop (e.g. a GCLB/Cloud Run front door
	// negotiating QUIC) can hit net::ERR_QUIC_PROTOCOL_ERROR on exactly this
	// response — the header is a plausible trigger for the intermediary
	// rejecting it — while an HTTP/2 client streams fine. It buys nothing on
	// any protocol version this server actually speaks: HTTP/1.1 defaults to
	// persistent connections without it, and Go's net/http server strips it
	// on HTTP/2 anyway.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}

	ch := h.subscribeAs(userHash)
	defer h.unsubscribe(ch)

	// Send a heartbeat comment immediately so the client knows the stream is open.
	w.Write([]byte(": connected\n\n")) //nolint:errcheck
	flusher.Flush()

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			// SSE comment line ("ping"): a line starting with ':' is a
			// comment per the SSE spec and is invisible to EventSource — it
			// never reaches onmessage, so it cannot be mistaken for a real
			// event. Keeps the connection from going idle long enough for a
			// front-door load balancer's idle backend timeout to tear it
			// down, which otherwise makes a reconnect storm the steady
			// state on a quiet instance. Same select loop as the event
			// branch below, so there is no second writer goroutine and no
			// race on w/flusher.
			w.Write([]byte(": ping\n\n")) //nolint:errcheck
			flusher.Flush()
		case msg, ok := <-ch:
			if !ok {
				return
			}
			w.Write([]byte("data: ")) //nolint:errcheck
			w.Write(msg)              //nolint:errcheck
			w.Write([]byte("\n\n"))   //nolint:errcheck
			flusher.Flush()
		}
	}
}
