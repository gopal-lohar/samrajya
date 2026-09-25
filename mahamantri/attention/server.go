package attention

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/gopal-lohar/samrajya/mahamantri/opencode"
)

// SessionLookup resolves a session's title on the opencode server, or
// opencode.ErrSessionNotFound. Registration uses it to reject IDs that
// don't exist and to default the label to the session's own title.
type SessionLookup func(ctx context.Context, sessionID string) (title string, err error)

// NewServer builds the HTTP+JSON API a manager (any language/harness) uses
// to register instances and receive attention-required events. No auth:
// the default listen address (see main.go) is loopback-only, which is what
// makes that omission safe - revisit if this is ever bound non-locally.
func NewServer(reg *Registry, bcast *Broadcaster, lookup SessionLookup) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /instances", handleListInstances(reg))
	mux.HandleFunc("POST /instances", handleRegister(reg, lookup))
	mux.HandleFunc("DELETE /instances/{id}", handleUnregister(reg))
	mux.HandleFunc("GET /events", handleEvents(reg, bcast))
	return &http.Server{Handler: mux}
}

func handleListInstances(reg *Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"instances": reg.List()})
	}
}

func handleRegister(reg *Registry, lookup SessionLookup) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SessionID string `json:"sessionID"`
			Label     string `json:"label"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if lookup != nil && body.SessionID != "" {
			title, err := lookup(r.Context(), body.SessionID)
			if errors.Is(err, opencode.ErrSessionNotFound) {
				writeError(w, http.StatusNotFound, "session "+body.SessionID+" does not exist on the opencode server")
				return
			}
			if err != nil {
				writeError(w, http.StatusBadGateway, "could not verify session on the opencode server: "+err.Error())
				return
			}
			if body.Label == "" {
				body.Label = title
			}
		}
		inst, err := reg.Register(body.SessionID, body.Label)
		switch {
		case errors.Is(err, ErrEmptySessionID):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, ErrProtectedSession):
			writeError(w, http.StatusForbidden, err.Error())
		case errors.Is(err, ErrAlreadyRegistered):
			writeError(w, http.StatusConflict, err.Error())
		case err != nil:
			writeError(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSON(w, http.StatusCreated, inst)
		}
	}
}

func handleUnregister(reg *Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		err := reg.Unregister(id)
		switch {
		case errors.Is(err, ErrNotRegistered):
			writeError(w, http.StatusNotFound, err.Error())
		case err != nil:
			writeError(w, http.StatusInternalServerError, err.Error())
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

func handleEvents(reg *Registry, bcast *Broadcaster) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeError(w, http.StatusInternalServerError, "streaming unsupported")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		ch := bcast.Subscribe(16)
		defer bcast.Unsubscribe(ch)

		for {
			select {
			case ev := <-ch:
				if !reg.NeedsAttention(ev) {
					continue
				}
				data, err := json.Marshal(ev)
				if err != nil {
					continue
				}
				fmt.Fprintf(w, "id: %s\ndata: %s\n\n", ev.ID, data)
				flusher.Flush()
			case <-r.Context().Done():
				return
			}
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
