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

// NewHandler verifies Linear-Signature/Linear-Timestamp, logs the full
// pretty-printed payload plus Linear-Event/Linear-Delivery headers
// unabridged, and forwards a short natural-language Summarize() to Senapati.
//
// self is the Linear user Senapati acts as. Deliveries whose actor is that
// user are logged but not forwarded - otherwise everything Senapati does on
// Linear (comments, status changes) comes straight back to it as a new
// prompt - and every summary is worded from its point of view.
func NewHandler(secret string, self Identity, fwd Forwarder, logger *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodySize))
		if err != nil {
			http.Error(w, "body too large or unreadable", http.StatusBadRequest)
			return
		}

		if !ValidSignature(secret, body, r.Header.Get("Linear-Signature")) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		if !FreshTimestamp(r.Header.Get("Linear-Timestamp")) {
			http.Error(w, "stale timestamp", http.StatusBadRequest)
			return
		}

		eventType := r.Header.Get("Linear-Event")
		logDelivery(logger, eventType, r.Header.Get("Linear-Delivery"), body)

		if self.known() && actorID(body) == self.UserID {
			logger.Printf("linear: not forwarding - the actor is Senapati's own Linear user (%s)", self.UserID)
		} else if err := fwd.Forward(Summarize(eventType, body, self)); err != nil {
			logger.Printf("linear: failed to forward event to senapati: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}
}

func actorID(body []byte) string {
	var env envelope
	if json.Unmarshal(body, &env) != nil {
		return ""
	}
	return env.Actor.ID
}

func logDelivery(logger *log.Logger, eventType, deliveryID string, body []byte) {
	var pretty bytes.Buffer
	if json.Indent(&pretty, body, "", "  ") != nil {
		pretty.Write(body) // not JSON somehow - print raw rather than drop it
	}
	logger.Printf("\n=== %s | event=%s delivery=%s ===\n%s\n", time.Now().Format(time.RFC3339), eventType, deliveryID, pretty.String())
}
