package xray

import (
	"net/http"
	"time"

	"github.com/triageagent-dev/x-ray/v2/pkg/memstore/hub"
)

// heartbeatInterval is the cadence of the SSE keepalive comment serveMemoryEvents
// writes on the ticker branch of its select loop. Comfortably under a typical L7
// load balancer's ~40s idle backend timeout, so a reconnect becomes the exception
// rather than the steady state on a quiet instance. A package-level var rather
// than a const so a test can drive it down and observe a heartbeat fire within a
// bounded wait instead of sleeping out a real 15s.
var heartbeatInterval = 15 * time.Second

// MemoryHitEvent and MemoryChangeEvent are the payloads the live stream carries.
type (
	MemoryHitEvent    = hub.HitEvent
	MemoryChangeEvent = hub.ChangeEvent
)

// serveMemoryEvents streams the hub's events to one client as server-sent events.
// The subscription is registered under userHash so a private event reaches only
// its owner; pass "" for an anonymous (broadcast-only) subscription. Each event
// is written as the bare JSON of the hit or the change, as clients have always
// read it.
func serveMemoryEvents(h *hub.Hub, w http.ResponseWriter, r *http.Request, userHash string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	// Deliberately NOT setting "Connection: keep-alive". Connection is an
	// HTTP/1.1 connection-specific header field; RFC 9113 §8.2.2 (HTTP/2) and RFC
	// 9114 §4.2 (HTTP/3) both forbid connection-specific fields and require a
	// compliant receiver to treat one as malformed. A browser reaching this
	// endpoint through an HTTP/3 hop can hit net::ERR_QUIC_PROTOCOL_ERROR on
	// exactly this response. It buys nothing on any protocol version this server
	// speaks: HTTP/1.1 defaults to persistent connections without it.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}

	ch, stop := h.Subscribe(userHash)
	defer stop()

	// Send a comment immediately so the client knows the stream is open.
	w.Write([]byte(": connected\n\n")) //nolint:errcheck
	flusher.Flush()

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			// An SSE comment line: invisible to EventSource, it keeps the
			// connection from going idle long enough for a front-door load
			// balancer's idle timeout to tear it down. Same select loop as the
			// event branch, so there is no second writer and no race on w.
			w.Write([]byte(": ping\n\n")) //nolint:errcheck
			flusher.Flush()
		case f, ok := <-ch:
			if !ok {
				return
			}
			w.Write([]byte("data: ")) //nolint:errcheck
			w.Write(f.Data)           //nolint:errcheck
			w.Write([]byte("\n\n"))   //nolint:errcheck
			flusher.Flush()
		}
	}
}
