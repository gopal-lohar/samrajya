package attention

import (
	"context"
	"sync"

	"github.com/gopal-lohar/samrajya/mahamantri/opencode"
)

// Broadcaster fans a single upstream event stream out to N subscribers
// (the TUI, the registry, and one per live SSE client).
type Broadcaster struct {
	mu   sync.Mutex
	subs map[chan opencode.Event]struct{}
}

func NewBroadcaster() *Broadcaster {
	return &Broadcaster{subs: make(map[chan opencode.Event]struct{})}
}

func (b *Broadcaster) Subscribe(bufSize int) chan opencode.Event {
	ch := make(chan opencode.Event, bufSize)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *Broadcaster) Unsubscribe(ch chan opencode.Event) {
	b.mu.Lock()
	delete(b.subs, ch)
	b.mu.Unlock()
	close(ch)
}

// Run fans every event from in out to all current subscribers via a
// non-blocking send: a subscriber whose buffer is full has the event
// dropped for it only. This is a live-status feed, not an audit log - a
// consumer that fell behind needs current state (see Registry.List), not a
// backlog - so Run never blocks on a slow subscriber, which would otherwise
// stall the opencode.Client read loop upstream of it.
func (b *Broadcaster) Run(ctx context.Context, in <-chan opencode.Event) {
	for {
		select {
		case ev, ok := <-in:
			if !ok {
				return
			}
			b.mu.Lock()
			for ch := range b.subs {
				select {
				case ch <- ev:
				default:
				}
			}
			b.mu.Unlock()
		case <-ctx.Done():
			return
		}
	}
}
