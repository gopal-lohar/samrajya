package linear

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/gopal-lohar/samrajya/mahamantri/harness"
	"io"
	"log"
	"net/http"
	"time"
)

const maxBodySize = 5 << 20 // 5MB, generous for a Linear webhook payload

// Forwarder is the one thing the webhook handler needs from Senapati's
// session. Forward must not block on the network - main.go wires the real
// senapati.Manager, whose Forward only enqueues - so the handler always
// responds inside Linear's 5-second window.
type Forwarder interface {
	Forward(text string) error
}

// ThreadStore is the ThreadIndex that also learns threads.
type ThreadStore interface {
	ThreadIndex
	Add(threadID, issue string) error
}

// Sink durably records a ping. Unlike Forward, the webhook is acknowledged
// only once Ingest has returned without error, so a ping it could not keep
// is redelivered by Linear instead of lost. Ingest must be idempotent:
// Linear retries a delivery it thinks failed.
type Sink interface {
	Ingest(p Ping, delivery string) error
}

// Handler receives Linear's webhooks. Every delivery is verified; only pings
// (see Classify) are passed on. With Sink set they are recorded durably and
// only metadata is logged; otherwise each delivery is logged in full and
// pings are forwarded to Senapati with what mahamantri knows about the
// issue's sainik attached.
type Handler struct {
	Secret  string
	Self    Identity
	Threads ThreadStore
	Sink    Sink
	// Sainiks lists the sainik session(s) working on a Linear issue
	// identifier. Optional.
	Sainiks func(issue string) []Sainik
	// Commands spells out, for the harness in use, the commands for routing
	// each ping.
	Commands harness.Commands
	Forward  Forwarder
	Logger   *log.Logger
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
	if h.Sink != nil {
		if err := h.record(eventType, delivery, body); err != nil {
			h.Logger.Printf("linear: delivery %s not recorded, asking Linear to retry: %v", delivery, err)
			http.Error(w, "could not record delivery", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	logDelivery(h.Logger, eventType, delivery, body)
	h.handle(eventType, body)
	w.WriteHeader(http.StatusOK)
}

// record is handle for a durable Sink: anything that fails is returned, so
// the delivery is retried rather than acknowledged and dropped.
func (h *Handler) record(eventType, delivery string, body []byte) error {
	if thread, issue, ok := ownComment(body, h.Self); ok && h.Threads != nil {
		if err := h.Threads.Add(thread, issue); err != nil {
			return fmt.Errorf("recording Senapati's comment thread %s: %w", thread, err)
		}
	}
	var threads ThreadIndex
	if h.Threads != nil {
		threads = h.Threads
	}
	ping, ok, why := Classify(eventType, body, h.Self, threads)
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

func (h *Handler) handle(eventType string, body []byte) {
	if thread, issue, ok := ownComment(body, h.Self); ok && h.Threads != nil {
		if err := h.Threads.Add(thread, issue); err != nil {
			h.Logger.Printf("linear: could not record Senapati's comment thread %s: %v", thread, err)
		}
	}
	var threads ThreadIndex
	if h.Threads != nil {
		threads = h.Threads
	}
	ping, ok, why := Classify(eventType, body, h.Self, threads)
	if !ok {
		h.Logger.Printf("linear: not forwarded - %s", why)
		return
	}
	var sainiks []Sainik
	if h.Sainiks != nil {
		sainiks = h.Sainiks(ping.Issue)
	}
	if err := h.Forward.Forward(Format(ping, sainiks, h.Commands)); err != nil {
		h.Logger.Printf("linear: failed to forward ping to senapati: %v", err)
	}
}

func logDelivery(logger *log.Logger, eventType, deliveryID string, body []byte) {
	var pretty bytes.Buffer
	if json.Indent(&pretty, body, "", "  ") != nil {
		pretty.Write(body) // not JSON somehow - print raw rather than drop it
	}
	logger.Printf("\n=== %s | event=%s delivery=%s ===\n%s\n", time.Now().Format(time.RFC3339), eventType, deliveryID, pretty.String())
}
