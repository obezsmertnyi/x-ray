package hub

import (
	"encoding/json"
	"testing"
	"time"
)

func TestSubscribeFanOut(t *testing.T) {
	ResetForTest()
	defer ResetForTest()

	f1, unsub1 := Subscribe()
	defer unsub1()
	f2, unsub2 := Subscribe()
	defer unsub2()

	if got := ClientCount(); got != 2 {
		t.Fatalf("ClientCount = %d, want 2", got)
	}

	PublishHit(HitEvent{QNs: []string{"repo//a.go::Foo"}, Query: "foo"})

	for i, ch := range []<-chan Frame{f1, f2} {
		select {
		case f := <-ch:
			if f.Type != FrameHit {
				t.Fatalf("subscriber %d: Type = %q, want %q", i, f.Type, FrameHit)
			}
			var evt HitEvent
			if err := json.Unmarshal(f.Data, &evt); err != nil {
				t.Fatalf("subscriber %d: unmarshal: %v", i, err)
			}
			if len(evt.QNs) != 1 || evt.QNs[0] != "repo//a.go::Foo" {
				t.Fatalf("subscriber %d: QNs = %v", i, evt.QNs)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d: never received the broadcast frame", i)
		}
	}
}

// TestSlowSubscriberDropsFramesInsteadOfBlocking asserts the non-blocking
// broadcast contract: a subscriber that never drains its channel must not be
// able to stall a publisher, since every search request is a publisher.
func TestSlowSubscriberDropsFramesInsteadOfBlocking(t *testing.T) {
	ResetForTest()
	defer ResetForTest()

	slow, unsub := Subscribe()
	defer unsub()

	done := make(chan struct{})
	go func() {
		defer close(done)
		// One more than the channel's buffer (8) so at least one send would
		// block on a channel nobody is reading, if broadcast were blocking.
		for i := 0; i < 20; i++ {
			PublishHit(HitEvent{QNs: []string{"x"}})
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("PublishHit blocked on a slow subscriber instead of dropping frames")
	}

	// The channel should be full (buffer 8) but no more — proving frames past
	// the buffer were dropped rather than queued or blocking.
	if n := len(slow); n != cap(slow) {
		t.Fatalf("subscriber channel len = %d, want cap %d (buffer 8)", n, cap(slow))
	}
}

func TestUnsubscribeClosesChannel(t *testing.T) {
	ResetForTest()
	defer ResetForTest()

	frames, unsub := Subscribe()
	if got := ClientCount(); got != 1 {
		t.Fatalf("ClientCount = %d, want 1", got)
	}
	unsub()
	if got := ClientCount(); got != 0 {
		t.Fatalf("ClientCount after unsubscribe = %d, want 0", got)
	}

	select {
	case _, ok := <-frames:
		if ok {
			t.Fatal("channel yielded a frame after unsubscribe, want closed+empty")
		}
	case <-time.After(time.Second):
		t.Fatal("channel was never closed by unsubscribe")
	}
}

func TestHitCountIncrements(t *testing.T) {
	ResetForTest()
	defer ResetForTest()

	if got := HitCount("repo//a.go::Foo"); got != 0 {
		t.Fatalf("HitCount before any publish = %d, want 0", got)
	}

	PublishHit(HitEvent{QNs: []string{"repo//a.go::Foo", "repo//b.go::Bar"}})
	PublishHit(HitEvent{QNs: []string{"repo//a.go::Foo"}})

	if got := HitCount("repo//a.go::Foo"); got != 2 {
		t.Fatalf("HitCount(Foo) = %d, want 2", got)
	}
	if got := HitCount("repo//b.go::Bar"); got != 1 {
		t.Fatalf("HitCount(Bar) = %d, want 1", got)
	}
	if got := HitCount("never-published"); got != 0 {
		t.Fatalf("HitCount(unknown) = %d, want 0", got)
	}
}

// TestPublishChangeNoSubscribersIsNoop guards the cheap-no-op contract: a
// caller (every incremental Update) must be able to publish freely even when
// nobody is connected, without allocating a frame for nobody to read.
func TestPublishChangeNoSubscribersIsNoop(t *testing.T) {
	ResetForTest()
	defer ResetForTest()

	if got := ClientCount(); got != 0 {
		t.Fatalf("ClientCount = %d, want 0", got)
	}
	PublishChange(ChangeEvent{Op: "update", Paths: []string{"a.go"}})
}
