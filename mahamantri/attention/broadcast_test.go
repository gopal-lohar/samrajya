package attention

import (
	"context"
	"testing"
	"time"

	"github.com/gopal-lohar/samrajya/mahamantri/opencode"
)

func TestBroadcasterFansOutToAllSubscribers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b := NewBroadcaster()
	in := make(chan opencode.Event)
	go b.Run(ctx, in)

	a := b.Subscribe(4)
	c := b.Subscribe(4)

	in <- opencode.Event{Type: "session.idle"}

	for _, ch := range []chan opencode.Event{a, c} {
		select {
		case ev := <-ch:
			if ev.Type != "session.idle" {
				t.Errorf("got %q, want session.idle", ev.Type)
			}
		case <-time.After(time.Second):
			t.Fatal("subscriber did not receive event")
		}
	}
}

func TestBroadcasterDropsForSlowSubscriberWithoutBlockingOthers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b := NewBroadcaster()
	in := make(chan opencode.Event)
	go b.Run(ctx, in)

	slow := b.Subscribe(1)  // never drained
	fast := b.Subscribe(16) // drained below

	done := make(chan struct{})
	go func() {
		for i := 0; i < 8; i++ {
			in <- opencode.Event{Type: "session.step.started"}
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run blocked on the slow subscriber instead of dropping for it")
	}

	received := 0
loop:
	for {
		select {
		case <-fast:
			received++
		case <-time.After(200 * time.Millisecond):
			break loop
		}
	}
	if received != 8 {
		t.Errorf("fast subscriber received %d events, want 8", received)
	}
	_ = slow
}
