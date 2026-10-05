package linear

import (
	"bytes"
	"encoding/json"
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

// Handler receives Linear's webhooks. Every delivery is verified and logged
// in full; only pings (see Classify) are forwarded to Senapati, with what
// mahamantri knows about the issue's sainik attached.
type Handler struct {
	Secret  string
	Self    Identity
	Threads *Threads
	// Sainiks lists the sainik session(s) working on a Linear issue
	// identifier. Optional.
	Sainiks func(issue string) []Sainik
	// Mahamantri is the address Senapati calls, used to spell out the
	// commands for routing each ping.
	Mahamantri string
	Forward    Forwarder
	Logger     *log.Logger
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
	logDelivery(h.Logger, eventType, r.Header.Get("Linear-Delivery"), body)
	h.handle(eventType, body)
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) handle(eventType string, body []byte) {
	if thread, issue, ok := ownComment(body, h.Self); ok && h.Threads != nil {
		if err := h.Threads.Add(thread, issue); err != nil {
			h.Logger.Printf("linear: could not record Senapati's comment thread %s: %v", thread, err)
		}
	}
	ping, ok, why := Classify(eventType, body, h.Self, h.Threads)
	if !ok {
		h.Logger.Printf("linear: not forwarded - %s", why)
		return
	}
	var sainiks []Sainik
	if h.Sainiks != nil {
		sainiks = h.Sainiks(ping.Issue)
	}
	if err := h.Forward.Forward(Format(ping, sainiks, h.Mahamantri)); err != nil {
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
