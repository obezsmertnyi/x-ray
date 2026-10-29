package xray

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
)

// When the model asks for several tools at once, each runs in its own goroutine and writes to the browser,
// so concurrent writes are expected. Run with -race: without browserConn's lock this panics with
// "concurrent write to websocket connection" or trips the race detector.
func TestBrowserConn_ConcurrentWritersAllArrive(t *testing.T) {
	const writers, perWriter = 8, 50
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		s := &VoiceSession{browser: &browserConn{Conn: conn}}
		var wg sync.WaitGroup
		for i := 0; i < writers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < perWriter; j++ {
					if err := s.browser.WriteJSON(VoiceServerMessage{ToolCall: "search_graph"}); err != nil {
						t.Error(err)
						return
					}
				}
			}()
		}
		wg.Wait()
		<-done
	}))
	defer srv.Close()

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	for n := 0; n < writers*perWriter; n++ {
		var m VoiceServerMessage
		if err := client.ReadJSON(&m); err != nil {
			t.Fatalf("message %d: %v", n, err)
		}
		if m.ToolCall != "search_graph" {
			t.Fatalf("message %d: got %+v", n, m)
		}
	}
	close(done)
}
