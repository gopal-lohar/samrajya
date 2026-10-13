package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/gopal-lohar/samrajya/mahamantri/attention"
	"github.com/gopal-lohar/samrajya/mahamantri/claudecode"
	"github.com/gopal-lohar/samrajya/mahamantri/config"
	"github.com/gopal-lohar/samrajya/mahamantri/opencode"
)

// Sessions is the Claude Code harness, as far as this package needs it.
type Sessions interface {
	Create(spec claudecode.Spec) (string, error)
	Prompt(id, text, delivery string) error
	Interrupt(id string) error
	Info(id string) (claudecode.Info, error)
	ActiveSessions(ctx context.Context) (map[string]bool, error)
	SetModel(id, model string) error
	Delete(id string) error
	Limits() []claudecode.Limit
}

// ClaudeOptions are the defaults for the sainik sessions Senapati creates on
// Claude Code.
type ClaudeOptions struct {
	Directory    string
	DefaultModel string
	Agent        string
	Args         []string // permission and tool flags (senapati.ClaudeSainikArgs)
	Workspace    string   // sainiks must work inside it; empty = anywhere
	// Role gives the standing rules for a sainik working in dir
	// (senapati.SainikRole plus its project's notes).
	Role func(dir string) string
}

// ClaudePrefix is where the Claude Code sessions API is mounted.
const ClaudePrefix = "/claude"

// ClaudeRoutes registers the API Senapati drives Claude Code sainiks with,
// and an API description on every other path. It enforces what the opencode
// gateway does: sainik titles name their issue, one sainik per issue,
// creation registers the sainik before answering, deleting unregisters it,
// and Senapati's own session cannot be driven through it.
func ClaudeRoutes(mux *http.ServeMux, sessions Sessions, reg Registrar, opt ClaudeOptions) {
	c := &claudeAPI{s: sessions, reg: reg, opt: opt}
	mux.HandleFunc("POST "+ClaudePrefix+"/session", c.create)
	mux.HandleFunc("GET "+ClaudePrefix+"/session/active", c.active)
	mux.HandleFunc("GET "+ClaudePrefix+"/session/{id}", c.info)
	mux.HandleFunc("POST "+ClaudePrefix+"/session/{id}/prompt", c.prompt)
	mux.HandleFunc("POST "+ClaudePrefix+"/session/{id}/interrupt", c.interrupt)
	mux.HandleFunc("POST "+ClaudePrefix+"/session/{id}/model", c.model)
	mux.HandleFunc("DELETE "+ClaudePrefix+"/session/{id}", c.delete)
	mux.HandleFunc("GET "+ClaudePrefix+"/limits", c.limits)
	mux.HandleFunc("/", claudeHelp)
}

type claudeAPI struct {
	s   Sessions
	reg Registrar
	opt ClaudeOptions
}

func (c *claudeAPI) create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title string `json:"title"`
		Text  string `json:"text"`
		Model string `json:"model"`
		Agent string `json:"agent"`
		Dir   string `json:"directory"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 5<<20)).Decode(&req); err != nil {
		attention.WriteError(w, http.StatusBadRequest, "body must be a JSON object: "+err.Error())
		return
	}
	if !checkSainik(w, r, c.reg, req.Title) {
		return
	}
	dir, ok := workDir(w, req.Dir, c.opt.Directory, c.opt.Workspace)
	if !ok {
		return
	}
	spec := claudecode.Spec{
		Title: req.Title,
		Dir:   dir,
		Model: firstNonEmpty(req.Model, c.opt.DefaultModel),
		Agent: firstNonEmpty(req.Agent, c.opt.Agent),
		Role:  roleFor(c.opt.Role, dir),
		Args:  c.opt.Args,
	}
	id, err := c.s.Create(spec)
	if err != nil {
		writeSessionError(w, "creating the session", err)
		return
	}
	resp := map[string]string{"id": id, "title": req.Title, "model": spec.Model}
	if _, err := c.reg.Register(id, req.Title); err != nil {
		resp["warning"] = "session created but not registered - you will not hear about it: " + err.Error()
	}
	if strings.TrimSpace(req.Text) != "" {
		if err := c.s.Prompt(id, req.Text, "queue"); err != nil {
			resp["warning"] = "session created but the task could not be sent: " + err.Error()
		}
	}
	attention.WriteJSON(w, http.StatusCreated, resp)
}

func (c *claudeAPI) guard(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if c.reg.IsProtected(id) {
		attention.WriteError(w, http.StatusForbidden, "that is your own Senapati session; it is managed by Mahamantri")
		return "", false
	}
	return id, true
}

func (c *claudeAPI) prompt(w http.ResponseWriter, r *http.Request) {
	id, ok := c.guard(w, r)
	if !ok {
		return
	}
	var req struct {
		Text     string `json:"text"`
		Delivery string `json:"delivery"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 5<<20)).Decode(&req); err != nil {
		attention.WriteError(w, http.StatusBadRequest, "body must be a JSON object: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		attention.WriteError(w, http.StatusBadRequest, `"text" is required`)
		return
	}
	delivery := firstNonEmpty(req.Delivery, "queue")
	if delivery != "queue" && delivery != "steer" {
		attention.WriteError(w, http.StatusBadRequest, `"delivery" must be "queue" (after its current turn) or "steer" (into its current turn); to stop it first, POST .../interrupt`)
		return
	}
	if err := c.s.Prompt(id, req.Text, delivery); err != nil {
		writeSessionError(w, "sending the message", err)
		return
	}
	info, _ := c.s.Info(id)
	attention.WriteJSON(w, http.StatusOK, map[string]any{"delivery": delivery, "state": info.State, "queued": info.Queued})
}

func (c *claudeAPI) interrupt(w http.ResponseWriter, r *http.Request) {
	id, ok := c.guard(w, r)
	if !ok {
		return
	}
	if err := c.s.Interrupt(id); err != nil {
		writeSessionError(w, "interrupting the session", err)
		return
	}
	info, _ := c.s.Info(id)
	attention.WriteJSON(w, http.StatusOK, map[string]any{"interrupted": true, "state": info.State})
}

func (c *claudeAPI) info(w http.ResponseWriter, r *http.Request) {
	info, err := c.s.Info(r.PathValue("id"))
	if err != nil {
		writeSessionError(w, "reading the session", err)
		return
	}
	attention.WriteJSON(w, http.StatusOK, info)
}

func (c *claudeAPI) active(w http.ResponseWriter, r *http.Request) {
	running, _ := c.s.ActiveSessions(r.Context())
	data := map[string]map[string]string{}
	for id := range running {
		data[id] = map[string]string{"type": "running"}
	}
	attention.WriteJSON(w, http.StatusOK, map[string]any{"data": data})
}

func (c *claudeAPI) model(w http.ResponseWriter, r *http.Request) {
	id, ok := c.guard(w, r)
	if !ok {
		return
	}
	var req struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Model) == "" {
		attention.WriteError(w, http.StatusBadRequest, `body must be {"model":"<model id>"}`)
		return
	}
	if err := c.s.SetModel(id, req.Model); err != nil {
		writeSessionError(w, "switching the model", err)
		return
	}
	attention.WriteJSON(w, http.StatusOK, map[string]string{"model": req.Model,
		"note": "takes effect from its next turn; it keeps its conversation"})
}

func (c *claudeAPI) delete(w http.ResponseWriter, r *http.Request) {
	id, ok := c.guard(w, r)
	if !ok {
		return
	}
	if err := c.s.Delete(id); err != nil {
		writeSessionError(w, "deleting the session", err)
		return
	}
	c.reg.Unregister(id) // not registered is fine
	w.WriteHeader(http.StatusNoContent)
}

func (c *claudeAPI) limits(w http.ResponseWriter, r *http.Request) {
	attention.WriteJSON(w, http.StatusOK, map[string]any{"limits": c.s.Limits()})
}

func roleFor(role func(string) string, dir string) string {
	if role == nil {
		return ""
	}
	return role(dir)
}

// workDir resolves a sainik's working directory - the one asked for (relative
// to the workspace), else the default, else the workspace - and refuses one
// outside the workspace, answering the request itself.
func workDir(w http.ResponseWriter, asked, def, workspace string) (string, bool) {
	dir := firstNonEmpty(asked, def, workspace)
	if dir != "" && !filepath.IsAbs(dir) && workspace != "" {
		dir = filepath.Join(workspace, dir)
	}
	if workspace != "" && !config.Within(workspace, dir) {
		attention.WriteError(w, http.StatusBadRequest, fmt.Sprintf("directory %q is outside the workspace %s: "+
			"sainiks work only on the projects inside it - pick the project's directory there", dir, workspace))
		return "", false
	}
	return dir, true
}

func writeSessionError(w http.ResponseWriter, doing string, err error) {
	status := http.StatusBadGateway
	switch {
	case errors.Is(err, opencode.ErrSessionNotFound):
		status = http.StatusNotFound
	case errors.Is(err, opencode.ErrBadRequest):
		status = http.StatusBadRequest
	}
	attention.WriteError(w, status, doing+": "+err.Error())
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ClaudeUsage is what Mahamantri answers on any path it doesn't serve, on the
// Claude Code harness.
const ClaudeUsage = `Mahamantri (no auth). Sainiks are Claude Code sessions Mahamantri runs.
  POST   /claude/session                    create a sainik and give it its task: {"title":"sainik-ENG-1-short-name","text":"<task>","model":"<optional>"} (one per issue; add ?parallel=true to override)
  POST   /claude/session/{id}/prompt        send it work: {"text":"...","delivery":"queue"} (queue = after its current turn; "steer" = into it)
  POST   /claude/session/{id}/interrupt     stop it now (then prompt it)
  GET    /claude/session/{id}               its state (running|idle|manual|limited), model, limitedUntil, lastReply
  GET    /claude/session/active             sessions running right now
  POST   /claude/session/{id}/model         move it to another model: {"model":"<model id>"}
  DELETE /claude/session/{id}               stop and forget it
  GET    /claude/limits                     subscription usage per provider
  GET    /instances                         registered sainiks with status and phase
  PATCH  /instances/{id}                    record its phase: {"phase":"..."}
  DELETE /instances/{id}                    stop watching a sainik`

func claudeHelp(w http.ResponseWriter, r *http.Request) {
	status := http.StatusNotFound
	msg := r.Method + " " + r.URL.Path + " is not a Mahamantri endpoint"
	if r.URL.Path == "/" {
		status, msg = http.StatusOK, "Mahamantri"
	}
	attention.WriteJSON(w, status, map[string]string{"error": msg, "usage": ClaudeUsage})
}
