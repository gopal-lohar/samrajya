// Package control is what Senapati uses to manage opencode sessions: a few
// shortcuts for the operations it does constantly, plus a gateway to the
// entire opencode API. Senapati never holds opencode credentials - mahamantri
// does - and everything it sends is tagged as its own, which is how a person
// stepping in is told apart from it.
package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/gopal-lohar/samrajya/mahamantri/attention"
	"github.com/gopal-lohar/samrajya/mahamantri/opencode"
)

// Backend is the opencode client, as far as this package needs it.
type Backend interface {
	CreateSession(ctx context.Context, req opencode.CreateSessionRequest) (opencode.SessionInfo, error)
	Prompt(ctx context.Context, sessionID string, req opencode.PromptRequest) (opencode.PromptResponse, error)
	Interrupt(ctx context.Context, sessionID string) error
	Snapshot(ctx context.Context, sessionID string) (opencode.Snapshot, error)
	Proxy(stripPrefix string) http.Handler
}

// Registrar is the attention registry, as far as this package needs it.
type Registrar interface {
	Register(sessionID, label string) (attention.Instance, error)
	Update(sessionID string, label, phase *string) (attention.Instance, error)
}

// Options are the defaults for sessions Senapati spawns.
type Options struct {
	Directory    string // working directory for new sessions; empty = server default
	DefaultModel string // "provider/id" used when a spawn names no model
	Agent        string
}

// GatewayPrefix is where the opencode API is mounted, e.g.
// GET /opencode/api/session/<id>/message.
const GatewayPrefix = "/opencode"

// Routes registers the sainik operations and the gateway on mux.
func Routes(mux *http.ServeMux, be Backend, reg Registrar, opt Options) {
	mux.HandleFunc("POST /sainiks", handleSpawn(be, reg, opt))
	mux.HandleFunc("POST /sainiks/{id}/message", handleMessage(be))
	mux.HandleFunc("GET /sainiks/{id}/status", handleStatus(be))
	mux.Handle(GatewayPrefix+"/", tagPrompts(be.Proxy(GatewayPrefix)))
}

type spawnRequest struct {
	Issue     string `json:"issue"`
	Slug      string `json:"slug"`
	Task      string `json:"task"`
	Model     string `json:"model"`
	Agent     string `json:"agent"`
	Directory string `json:"directory"`
	Phase     string `json:"phase"`
}

// handleSpawn creates a session titled sainik-<issue>-<slug>, registers it,
// and sends it the task - in that order, so none of its events are missed.
func handleSpawn(be Backend, reg Registrar, opt Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req spawnRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			attention.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		if strings.TrimSpace(req.Issue) == "" || strings.TrimSpace(req.Task) == "" {
			attention.WriteError(w, http.StatusBadRequest, `"issue" and "task" are required`)
			return
		}

		title := "sainik-" + req.Issue + "-" + slugify(req.Slug)
		modelRef := firstNonEmpty(req.Model, opt.DefaultModel)
		create := opencode.CreateSessionRequest{Title: title, Agent: firstNonEmpty(req.Agent, opt.Agent)}
		if modelRef != "" {
			m, err := ParseModel(modelRef)
			if err != nil {
				attention.WriteError(w, http.StatusBadRequest, err.Error())
				return
			}
			create.Model = &m
		}
		if dir := firstNonEmpty(req.Directory, opt.Directory); dir != "" {
			create.Location = &opencode.SessionLocation{Directory: dir}
		}

		info, err := be.CreateSession(r.Context(), create)
		if err != nil {
			backendError(w, "creating the session", err)
			return
		}
		if _, err := reg.Register(info.ID, title); err != nil {
			attention.WriteError(w, http.StatusInternalServerError, fmt.Sprintf("session %s was created but could not be registered: %v", info.ID, err))
			return
		}
		phase := firstNonEmpty(req.Phase, "started")
		reg.Update(info.ID, nil, &phase)
		if _, err := be.Prompt(r.Context(), info.ID, senapatiPrompt(req.Task)); err != nil {
			attention.WriteError(w, http.StatusBadGateway, fmt.Sprintf("session %s was created and registered but the task could not be sent: %v", info.ID, err))
			return
		}
		attention.WriteJSON(w, http.StatusCreated, map[string]string{"sessionID": info.ID, "title": title, "model": modelRef})
	}
}

// handleMessage sends a message to a session. With "interrupt": true the
// session is interrupted first - the only way to reach one that is inside a
// long tool call, since `steer` delivery still waits for the tool to finish.
func handleMessage(be Backend) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Text      string `json:"text"`
			Interrupt bool   `json:"interrupt"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			attention.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		if strings.TrimSpace(req.Text) == "" {
			attention.WriteError(w, http.StatusBadRequest, `"text" is required`)
			return
		}
		id := r.PathValue("id")
		if req.Interrupt {
			if err := be.Interrupt(r.Context(), id); err != nil {
				backendError(w, "interrupting the session", err)
				return
			}
		}
		resp, err := be.Prompt(r.Context(), id, senapatiPrompt(req.Text))
		if err != nil {
			backendError(w, "sending the message", err)
			return
		}
		attention.WriteJSON(w, http.StatusOK, map[string]any{"messageID": resp.ID, "interrupted": req.Interrupt})
	}
}

func handleStatus(be Backend) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		snap, err := be.Snapshot(r.Context(), r.PathValue("id"))
		if err != nil {
			backendError(w, "reading the session", err)
			return
		}
		attention.WriteJSON(w, http.StatusOK, snap)
	}
}

// senapatiPrompt builds a prompt tagged as Senapati's. `steer` delivery puts
// it in front of the session's next step rather than behind its whole turn.
func senapatiPrompt(text string) opencode.PromptRequest {
	return opencode.PromptRequest{
		Text:     text,
		Delivery: "steer",
		Metadata: map[string]string{"source": attention.SourceSenapati},
	}
}

var promptPath = regexp.MustCompile(`^` + GatewayPrefix + `/api/session/[^/]+/prompt$`)

// tagPrompts stamps metadata.source = "senapati" on every prompt that goes
// through the gateway. Anything arriving here is Senapati's, and the tag is
// what keeps manual-takeover detection from mistaking it for a person.
func tagPrompts(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && promptPath.MatchString(r.URL.Path) {
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 5<<20))
			if err != nil {
				http.Error(w, "request body too large or unreadable", http.StatusBadRequest)
				return
			}
			var m map[string]any
			if json.Unmarshal(body, &m) == nil {
				md, _ := m["metadata"].(map[string]any)
				if md == nil {
					md = map[string]any{}
				}
				md["source"] = attention.SourceSenapati
				m["metadata"] = md
				body, _ = json.Marshal(m)
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			r.Header.Set("Content-Length", fmt.Sprint(len(body)))
		}
		next.ServeHTTP(w, r)
	})
}

// ParseModel turns "provider/id" into a model reference.
func ParseModel(ref string) (opencode.SessionModel, error) {
	provider, id, ok := strings.Cut(ref, "/")
	if !ok || provider == "" || id == "" {
		return opencode.SessionModel{}, fmt.Errorf("model %q must look like provider/id, e.g. openai/gpt-6-sol", ref)
	}
	return opencode.SessionModel{ProviderID: provider, ID: id}, nil
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// slugify makes a short lowercase, dash-separated title fragment.
func slugify(s string) string {
	s = strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if s == "" {
		return "task"
	}
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// backendError turns an opencode failure into a response that says what was
// being attempted and what the server said.
func backendError(w http.ResponseWriter, doing string, err error) {
	status := http.StatusBadGateway
	switch {
	case errors.Is(err, opencode.ErrSessionNotFound):
		status = http.StatusNotFound
	case errors.Is(err, opencode.ErrBadRequest):
		status = http.StatusBadRequest
	case errors.Is(err, opencode.ErrUnauthorized):
		attention.WriteError(w, http.StatusBadGateway, "mahamantri's opencode password was rejected while "+doing+" - update opencode.password in mahamantri.yaml")
		return
	}
	attention.WriteError(w, status, fmt.Sprintf("%s: %v", doing, err))
}
