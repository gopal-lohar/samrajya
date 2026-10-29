package linear

import (
	"fmt"
	"io"
	"log"
	"net/http"
)

const maxBodySize = 5 << 20 // 5MB, generous for a Linear webhook payload

// ThreadStore is the ThreadIndex that also learns threads.
type ThreadStore interface {
	ThreadIndex
	Add(threadID, issue string) error
}

// Sink durably records a ping. The webhook is acknowledged only once Ingest
// has returned without error, so a ping it could not keep is redelivered by
// Linear instead of lost. Ingest must be idempotent: Linear retries a
// delivery it thinks failed.
type Sink interface {
	Ingest(p Ping, delivery string) error
}

// Handler receives Linear's webhooks. Every delivery is verified; pings (see
// Classify) are recorded in Sink, and the threads of Senapati's own comments
// in Threads, before Linear is told the delivery arrived. Only metadata is
// logged, never comment text.
type Handler struct {
	Secret  string
	Self    Identity
	Threads ThreadStore
	Sink    Sink
	Logger  *log.Logger
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodySize))
	if err != nil {
		http.Error(w, "body too large or unreadable", http.StatusBadRequest)
		return
	}
	if !ValidSignature(h.Secret, body, r.Header.Get("Linear-Signature")) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	if !FreshTimestamp(r.Header.Get("Linear-Timestamp")) {
		http.Error(w, "stale timestamp", http.StatusBadRequest)
		return
	}
	eventType := r.Header.Get("Linear-Event")
	delivery := r.Header.Get("Linear-Delivery")
	if err := h.record(eventType, delivery, body); err != nil {
		h.Logger.Printf("linear: delivery %s not recorded, asking Linear to retry: %v", delivery, err)
		http.Error(w, "could not record delivery", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// record returns anything that fails, so the delivery is retried rather than
// acknowledged and dropped.
func (h *Handler) record(eventType, delivery string, body []byte) error {
	if thread, issue, ok := ownComment(body, h.Self); ok {
		if err := h.Threads.Add(thread, issue); err != nil {
			return fmt.Errorf("recording Senapati's comment thread %s: %w", thread, err)
		}
	}
	ping, ok, why := Classify(eventType, body, h.Self, h.Threads)
	if !ok {
		h.Logger.Printf("linear: delivery %s (%s) not recorded - %s", delivery, eventType, why)
		return nil
	}
	if err := h.Sink.Ingest(ping, delivery); err != nil {
		return err
	}
	h.Logger.Printf("linear: delivery %s recorded: %s on %s by %s (comment %s)", delivery, ping.Kind, ping.Issue, ping.Author, ping.CommentID)
	return nil
}
