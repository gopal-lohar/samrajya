// Package control is how Senapati manages opencode sessions: the opencode API
// itself, through a gateway that holds the credentials. Senapati calls the
// real API - create a session, prompt it, interrupt it, read its messages,
// answer its permission requests - and the gateway only adds what has to be
// true of every call:
//
//   - creating a session makes a sainik: its title must name the issue
//     (sainik-<ISSUE>-<slug>), an issue gets one sainik, the configured
//     defaults are filled in (permissions, directory, model, agent), and it
//     is registered so Senapati hears about it;
//   - every prompt is tagged as Senapati's (that is how a person stepping in
//     is told apart from it) and delivered "queue" unless it says otherwise;
//   - an interrupt is recorded as Senapati's own, not a person's;
//   - deleting a session unregisters it;
//   - waiting on a session, and changing Senapati's own session, are refused.
package control

import (
	"bytes"
	"encoding/json"
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
	Proxy(stripPrefix string) http.Handler
}

// Registrar is the attention registry, as far as this package needs it.
type Registrar interface {
	Register(sessionID, label string) (attention.Instance, error)
	Unregister(sessionID string) error
	ForIssue(issue string) []attention.Instance
	IsProtected(sessionID string) bool
	ExpectInterrupt(sessionID string)
}

// Options are the defaults for the sainik sessions Senapati creates. Each is
// applied only when the request doesn't set it itself.
type Options struct {
	Directory    string // working directory; empty = the server's default
	DefaultModel string // provider/id
	Agent        string
	Permissions  []opencode.PermissionRule
}

// GatewayPrefix is where the opencode API is mounted, e.g.
// GET /opencode/api/session/<id>/message.
const GatewayPrefix = "/opencode"

// Routes registers the gateway, and an API description on every other path.
func Routes(mux *http.ServeMux, be Backend, reg Registrar, opt Options) {
	g := &gateway{proxy: be.Proxy(GatewayPrefix), reg: reg, opt: opt}
	mux.Handle(GatewayPrefix+"/", g)
	mux.HandleFunc("/", handleHelp)
}

var (
	createPath    = regexp.MustCompile(`^` + GatewayPrefix + `/api/session/?$`)
	promptPath    = regexp.MustCompile(`^` + GatewayPrefix + `/api/session/([^/]+)/prompt$`)
	interruptPath = regexp.MustCompile(`^` + GatewayPrefix + `/api/session/([^/]+)/interrupt$`)
	waitPath      = regexp.MustCompile(`^` + GatewayPrefix + `/api/(?:experimental/)?session/[^/]+/wait$`)
	sessionPath   = regexp.MustCompile(`^` + GatewayPrefix + `/api/(?:experimental/)?session/([^/]+)(/.*)?$`)
	sainikTitle   = regexp.MustCompile(`^sainik-([A-Za-z][A-Za-z0-9]*-[0-9]+)-[a-z0-9][a-z0-9-]*$`)
)

type gateway struct {
	proxy http.Handler
	reg   Registrar
	opt   Options
}

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if waitPath.MatchString(path) {
		attention.WriteError(w, http.StatusForbidden, "Senapati never waits on a session: Mahamantri messages you when a sainik "+
			"finishes, fails or needs an answer. End your turn instead.")
		return
	}
	if m := sessionPath.FindStringSubmatch(path); m != nil && r.Method != http.MethodGet && g.reg.IsProtected(m[1]) {
		attention.WriteError(w, http.StatusForbidden, "that is your own Senapati session; it is managed by Mahamantri")
		return
	}
	switch {
	case r.Method == http.MethodPost && createPath.MatchString(path):
		g.createSainik(w, r)
		return
	case r.Method == http.MethodPost && promptPath.MatchString(path):
		if !tagPrompt(w, r) {
			return
		}
	case r.Method == http.MethodPost && interruptPath.MatchString(path):
		g.reg.ExpectInterrupt(interruptPath.FindStringSubmatch(path)[1])
	case r.Method == http.MethodDelete:
		if m := sessionPath.FindStringSubmatch(path); m != nil && m[2] == "" {
			rec := record(g.proxy, r)
			if rec.status < 300 {
				g.reg.Unregister(m[1]) // not registered is fine
			}
			rec.copyTo(w)
			return
		}
	}
	g.proxy.ServeHTTP(w, r)
}

// createSainik is POST /api/session: checked, defaulted, forwarded, and the
// new session registered before the response goes back - so it is watched
// before Senapati can send it anything.
func (g *gateway) createSainik(w http.ResponseWriter, r *http.Request) {
	body, ok := readJSON(w, r)
	if !ok {
		return
	}
	title, _ := body["title"].(string)
	m := sainikTitle.FindStringSubmatch(title)
	if m == nil {
		attention.WriteError(w, http.StatusBadRequest, fmt.Sprintf("title %q: a sainik's title must be sainik-<ISSUE>-<slug>, "+
			"e.g. \"sainik-SEN-33-request-info\" (lowercase slug) - it is how Linear pings about the issue find it", title))
		return
	}
	issue := strings.ToUpper(m[1])
	q := r.URL.Query()
	if existing := g.reg.ForIssue(issue); len(existing) > 0 && q.Get("parallel") != "true" {
		inst := existing[0]
		attention.WriteJSON(w, http.StatusConflict, map[string]string{
			"error": fmt.Sprintf("%s already has a sainik: %q (session %s). Prompt that session instead of starting another; "+
				"add ?parallel=true only if the issue splits into independent parts.", issue, inst.Label, inst.SessionID),
			"sessionID": inst.SessionID,
		})
		return
	}
	q.Del("parallel")
	r.URL.RawQuery = q.Encode()

	if _, set := body["permissions"]; !set && g.opt.Permissions != nil {
		body["permissions"] = g.opt.Permissions
	}
	if _, set := body["location"]; !set && g.opt.Directory != "" {
		body["location"] = map[string]string{"directory": g.opt.Directory}
	}
	if _, set := body["model"]; !set && g.opt.DefaultModel != "" {
		if provider, id, ok := strings.Cut(g.opt.DefaultModel, "/"); ok {
			body["model"] = map[string]string{"providerID": provider, "id": id}
		}
	}
	if _, set := body["agent"]; !set && g.opt.Agent != "" {
		body["agent"] = g.opt.Agent
	}
	setBody(r, body)

	rec := record(g.proxy, r)
	if rec.status < 300 {
		var created struct {
			Data struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if json.Unmarshal(rec.body.Bytes(), &created) == nil && created.Data.ID != "" {
			if _, err := g.reg.Register(created.Data.ID, title); err != nil {
				rec.header.Set("X-Mahamantri-Warning", "session created but not registered - you will not hear about it: "+err.Error())
			}
		}
	}
	rec.copyTo(w)
}

// tagPrompt stamps metadata.source = "senapati" on a prompt, which keeps
// manual-takeover detection from mistaking it for a person, and makes
// "queue" (after the session's current turn) the delivery when none is
// given. A body that isn't a JSON object is passed through for opencode to
// reject. It reports false if it already answered the request.
func tagPrompt(w http.ResponseWriter, r *http.Request) bool {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 5<<20))
	if err != nil {
		http.Error(w, "request body too large or unreadable", http.StatusBadRequest)
		return false
	}
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil || body == nil {
		r.Body = io.NopCloser(bytes.NewReader(raw))
		return true
	}
	md, _ := body["metadata"].(map[string]any)
	if md == nil {
		md = map[string]any{}
	}
	md["source"] = attention.SourceSenapati
	body["metadata"] = md
	if d, _ := body["delivery"].(string); d == "" {
		body["delivery"] = "queue"
	}
	setBody(r, body)
	return true
}

func readJSON(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 5<<20))
	if err != nil {
		http.Error(w, "request body too large or unreadable", http.StatusBadRequest)
		return nil, false
	}
	body := map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			attention.WriteError(w, http.StatusBadRequest, "body must be a JSON object: "+err.Error())
			return nil, false
		}
	}
	return body, true
}

func setBody(r *http.Request, body map[string]any) {
	data, _ := json.Marshal(body)
	r.Body = io.NopCloser(bytes.NewReader(data))
	r.ContentLength = int64(len(data))
	r.Header.Set("Content-Length", fmt.Sprint(len(data)))
	r.Header.Set("Content-Type", "application/json")
}

// recorded is a response held back so the gateway can act on it before
// passing it on.
type recorded struct {
	status int
	header http.Header
	body   bytes.Buffer
}

func record(h http.Handler, r *http.Request) *recorded {
	rec := &recorded{status: http.StatusOK, header: http.Header{}}
	h.ServeHTTP(rec, r)
	return rec
}

func (rec *recorded) Header() http.Header         { return rec.header }
func (rec *recorded) Write(b []byte) (int, error) { return rec.body.Write(b) }
func (rec *recorded) WriteHeader(status int)      { rec.status = status }

func (rec *recorded) copyTo(w http.ResponseWriter) {
	for k, v := range rec.header {
		if k != "Content-Length" {
			w.Header()[k] = v
		}
	}
	w.WriteHeader(rec.status)
	w.Write(rec.body.Bytes())
}

// Usage is what Mahamantri answers on any path it doesn't serve. Senapati,
// when unsure, probes for an API description; a bare "404 page not found"
// sent it off guessing, so the answer is the API itself.
const Usage = `Mahamantri (no auth). The opencode API is at /opencode/api/..., credentials handled; full description at /opencode/openapi.json.
  POST   /opencode/api/session                         create a sainik: {"title":"sainik-SEN-1-short-name"} (one per issue; add ?parallel=true to override)
  POST   /opencode/api/session/{id}/prompt             send it work: {"text":"...","delivery":"queue"} (queue = after its current turn; "steer" = at its next step)
  POST   /opencode/api/session/{id}/interrupt          stop it now (then prompt it)
  GET    /opencode/api/session/active                  sessions running right now (absent = idle)
  GET    /opencode/api/session/{id}/message?type=assistant&order=desc&limit=1   its latest reply
  GET    /opencode/api/session/{id}/permission         its pending permission requests; answer with POST .../permission/{requestID}/reply {"decision":"once|always|reject"}
  GET    /opencode/api/session/{id}/form               its pending questions; answer with POST .../form/{formID}/reply {"answer":{...}}
  GET    /instances                                    registered sainiks with status and phase
  PATCH  /instances/{id}                               record its phase: {"phase":"..."}
  DELETE /instances/{id}                               stop watching a sainik (DELETE /opencode/api/session/{id} deletes it)`

func handleHelp(w http.ResponseWriter, r *http.Request) {
	status := http.StatusNotFound
	msg := r.Method + " " + r.URL.Path + " is not a Mahamantri endpoint"
	if r.URL.Path == "/" {
		status, msg = http.StatusOK, "Mahamantri"
	}
	attention.WriteJSON(w, status, map[string]string{"error": msg, "usage": Usage})
}
