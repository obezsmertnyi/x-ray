package hub

import (
	"net/http"
	"net/url"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
)

const (
	wsPingInterval = 30 * time.Second
	wsPongTimeout  = 30 * time.Second
)

// upgrader is package-level (not per-request) so every connection shares one
// configuration; gorilla/websocket's Upgrader holds no per-connection state.
var upgrader = websocket.Upgrader{
	CheckOrigin: sameOriginCheck,
}

// sameOriginCheck allows an empty Origin (same-origin requests, and every
// non-browser client, never set one) or one whose host matches r.Host, and
// rejects everything else. Hit frames carry raw query strings, so a page on
// another origin must not be able to open this stream and read them —
// gorilla/websocket's default CheckOrigin accepts any origin, which would
// leak search queries to any site a user happens to have open.
func sameOriginCheck(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == r.Host
}

// ServeWS upgrades the connection and streams hub Frame envelopes
// (`{"type":"hit"|"change","data":...}`), one per WebSocket text message,
// until the client disconnects. Server -> client push only: the client is
// never expected to send anything, so the read pump below exists solely to
// notice a disconnect and answer pong frames.
func ServeWS(logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			logger.Error().Err(err).Msg("hub: failed to upgrade WebSocket connection")
			return
		}
		defer conn.Close()

		frames, unsub := Subscribe()
		defer unsub()

		done := make(chan struct{})
		conn.SetReadDeadline(time.Now().Add(wsPingInterval + wsPongTimeout)) //nolint:errcheck
		conn.SetPongHandler(func(string) error {
			return conn.SetReadDeadline(time.Now().Add(wsPingInterval + wsPongTimeout))
		})
		go func() {
			defer close(done)
			for {
				// Drained, never interpreted: this stream carries no
				// client->server commands, only disconnect/pong signals.
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()

		ping := time.NewTicker(wsPingInterval)
		defer ping.Stop()

		for {
			select {
			case f, ok := <-frames:
				if !ok {
					return
				}
				conn.SetWriteDeadline(time.Now().Add(wsPongTimeout)) //nolint:errcheck
				if err := conn.WriteJSON(f); err != nil {
					return
				}
			case <-ping.C:
				conn.SetWriteDeadline(time.Now().Add(wsPongTimeout)) //nolint:errcheck
				if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					return
				}
			case <-done:
				return
			}
		}
	}
}
