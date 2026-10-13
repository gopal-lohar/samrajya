package brain

import (
	"encoding/json"
	"errors"
	"net/http"
)

// Routes registers the memory API Senapati keeps the brain with:
//
//	GET    /brain          the notes, without bodies
//	GET    /brain/{name}   one note
//	PUT    /brain/{name}   create or replace: {"description","project","body"}
//	DELETE /brain/{name}   forget it
func Routes(mux *http.ServeMux, s *Store) {
	mux.HandleFunc("GET /brain", func(w http.ResponseWriter, r *http.Request) {
		notes, err := s.List()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if notes == nil {
			notes = []Note{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"notes": notes, "count": len(notes), "max": MaxNotes})
	})
	mux.HandleFunc("GET /brain/{name}", func(w http.ResponseWriter, r *http.Request) {
		n, err := s.Get(r.PathValue("name"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, n)
	})
	mux.HandleFunc("PUT /brain/{name}", func(w http.ResponseWriter, r *http.Request) {
		var n Note
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&n); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": `body must be {"description":"<one line>","project":"<optional>","body":"<the note>"}: ` + err.Error()})
			return
		}
		n.Name = r.PathValue("name")
		created, err := s.Put(n)
		if err != nil {
			writeErr(w, err)
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		writeJSON(w, status, map[string]any{"name": n.Name, "created": created})
	})
	mux.HandleFunc("DELETE /brain/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Delete(r.PathValue("name")); err != nil {
			writeErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func writeErr(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, ErrInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, ErrFull):
		status = http.StatusConflict
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
