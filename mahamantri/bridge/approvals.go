package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Samrajya can change itself: a session edits and tests its codebase like any
// other project, but deploying it and restarting Samrajya or its main session
// need the person's explicit OK. An agent asks (samrajya_restart); mahamantri
// posts the request in a conversation; a trusted person answers there with
// "approve R<n>" or "refuse R<n>"; mahamantri carries it out itself, outside
// any session, and reports how it went. No session can approve its own
// request: an approval counts only as a trusted person's message on a
// surface.

// What a session may ask for.
const (
	// Deploy builds and tests a commit of the codebase, puts it in place
	// and restarts mahamantri on it, rolling back if it does not come up.
	Deploy = "deploy"
	// Restart restarts mahamantri as it is; sessions reattach.
	Restart = "restart"
	// RestartMain replaces the main session with a new one.
	RestartMain = "restart_main"
)

// Approval states.
const (
	Asked     = "asked"
	Approved  = "approved"
	Refused   = "refused"
	Running   = "running"
	Succeeded = "succeeded"
	// Failed (the action outcome, "failed") is reused.
	Expired = "expired"
)

var bApprovals = []byte("approvals")

// Approval is one request to change the running Samrajya.
type Approval struct {
	ID     string `json:"id"` // R<n>
	Kind   string `json:"kind"`
	Ref    string `json:"ref,omitempty"`    // deploy: the exact commit
	Reason string `json:"reason"`           // why, in the asker's words
	Detail string `json:"detail,omitempty"` // what it changes, from the codebase
	Agent  string `json:"agent"`
	TaskID string `json:"taskId,omitempty"`
	// Conversation and ThreadID are where it was asked, and the only place
	// it can be answered.
	Conversation string    `json:"conversation"`
	ThreadID     string    `json:"threadId,omitempty"`
	Status       string    `json:"status"`
	AskedAt      time.Time `json:"askedAt"`
	DecidedAt    time.Time `json:"decidedAt,omitzero"`
	DecidedBy    string    `json:"decidedBy,omitempty"`
	StartedAt    time.Time `json:"startedAt,omitzero"`
	FinishedAt   time.Time `json:"finishedAt,omitzero"`
	Result       string    `json:"result,omitempty"`
}

// Open reports whether the approval is still to be answered or carried out.
func (a Approval) Open() bool {
	return a.Status == Asked || a.Status == Approved || a.Status == Running
}

// SelfUpdater carries out approved changes to the running Samrajya. Start
// must hand the work to something that outlives mahamantri, which the work
// restarts; Result is how it ended, once it has.
type SelfUpdater interface {
	// Describe checks a request before it is asked: for a deploy it
	// resolves ref to a commit and says what it changes.
	Describe(ctx context.Context, kind, ref string) (resolved, detail string, err error)
	Start(a Approval) error
	Result(a Approval) (done, ok bool, text string)
}

const (
	// approvalTTL is how long a request can be answered.
	approvalTTL = 24 * time.Hour
	// runLimit is how long a started change may take before it counts as
	// failed for want of a result.
	runLimit = 45 * time.Minute
)

// askApproval stores a new request with the next id.
func (s *Store) askApproval(a Approval) (Approval, error) {
	err := s.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bApprovals)
		if err != nil {
			return err
		}
		var busy error
		b.ForEach(func(_, v []byte) error {
			var o Approval
			if json.Unmarshal(v, &o) == nil && (o.Status == Approved || o.Status == Running) {
				busy = fmt.Errorf("%s (%s) is being carried out; ask again when it has finished", o.ID, o.Kind)
			}
			return nil
		})
		if busy != nil {
			return busy
		}
		n, _ := b.NextSequence()
		a.ID = fmt.Sprintf("R%d", n)
		a.Status = Asked
		a.AskedAt = s.now()
		return putJSON(b, a.ID, a)
	})
	return a, err
}

// Approval returns one request.
func (s *Store) Approval(id string) (Approval, error) {
	var a Approval
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bApprovals)
		if b == nil {
			return ErrNotFound
		}
		return getJSON(b, id, &a)
	})
	return a, err
}

// Approvals lists requests, open ones only unless all.
func (s *Store) Approvals(all bool) ([]Approval, error) {
	var out []Approval
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bApprovals)
		if b == nil {
			return nil
		}
		return b.ForEach(func(_, v []byte) error {
			var a Approval
			if err := json.Unmarshal(v, &a); err != nil {
				return err
			}
			if all || a.Open() {
				out = append(out, a)
			}
			return nil
		})
	})
	return out, err
}

// updateApproval changes a request if it is in one of the states from.
func (s *Store) updateApproval(id string, from []string, change func(*Approval)) (Approval, error) {
	var a Approval
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bApprovals)
		if b == nil {
			return ErrNotFound
		}
		if err := getJSON(b, id, &a); err != nil {
			return err
		}
		ok := false
		for _, f := range from {
			ok = ok || a.Status == f
		}
		if !ok {
			return fmt.Errorf("%s is %s", id, a.Status)
		}
		change(&a)
		return putJSON(b, id, a)
	})
	return a, err
}

// decisionRE is an answer to a request: alone on a line, so that a quote of
// the request itself never counts.
var decisionRE = regexp.MustCompile(`(?im)^\s*(approve|approved|refuse|refused)\s+(R\d+)\s*[.!]?\s*$`)

// decide applies a trusted person's answer in a message, if it holds one:
// in the conversation the request was asked in, while it can be answered.
// It returns the request and whether it was approved; ok is false when the
// message decides nothing.
func (s *Server) decide(in Inbound) (Approval, bool, bool) {
	if !in.Trusted || in.Kind == "edit" {
		return Approval{}, false, false
	}
	body := in.Body
	// A reply on Telegram starts with what it quotes.
	if strings.HasPrefix(body, "(replying to: ") {
		if i := strings.Index(body, ")\n"); i >= 0 {
			body = body[i+2:]
		}
	}
	m := decisionRE.FindStringSubmatch(body)
	if m == nil {
		return Approval{}, false, false
	}
	id := strings.ToUpper(m[2])
	approve := strings.HasPrefix(strings.ToLower(m[1]), "approve")
	a, err := s.Store.Approval(id)
	if err != nil || a.Conversation != in.Conversation {
		return Approval{}, false, false
	}
	now := s.now()
	if a.Status == Asked && now.Sub(a.AskedAt) > approvalTTL {
		s.Store.updateApproval(id, []string{Asked}, func(a *Approval) { a.Status, a.FinishedAt = Expired, now })
		return a, false, false
	}
	status := Refused
	if approve {
		status = Approved
	}
	a, err = s.Store.updateApproval(id, []string{Asked}, func(a *Approval) {
		a.Status, a.DecidedAt, a.DecidedBy = status, now, in.Author
		if !approve {
			a.FinishedAt = now
		}
	})
	if err != nil {
		return Approval{}, false, false
	}
	return a, approve, true
}

// afterReceive acts on an answer to a request in a message just stored.
func (s *Server) afterReceive(in Inbound) {
	a, approve, ok := s.decide(in)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if !approve {
		s.Logger.Printf("bridge: %s refused by %s", a.ID, a.DecidedBy)
		s.say(ctx, a, "Refused "+a.ID+": nothing changes.")
		s.tellAsker(a, a.ID+" ("+describeKind(a)+") was refused by "+a.DecidedBy+". Nothing was changed.")
		return
	}
	s.start(ctx, a)
}

// start hands an approved request to the SelfUpdater.
func (s *Server) start(ctx context.Context, a Approval) {
	if s.SelfUpdate == nil {
		s.finish(ctx, a, false, "this mahamantri cannot change itself (selfUpdate is not configured)")
		return
	}
	a, err := s.Store.updateApproval(a.ID, []string{Approved}, func(a *Approval) { a.Status, a.StartedAt = Running, s.now() })
	if err != nil {
		s.Logger.Printf("bridge: starting %s: %v", a.ID, err)
		return
	}
	if err := s.SelfUpdate.Start(a); err != nil {
		s.finish(ctx, a, false, "it could not be started: "+err.Error())
		return
	}
	s.Logger.Printf("bridge: %s approved by %s, started", a.ID, a.DecidedBy)
	s.say(ctx, a, "Approved "+a.ID+": "+describeKind(a)+" has started. I'll say here how it went.")
}

// finish records how a request ended and tells the conversation and the
// session that asked.
func (s *Server) finish(ctx context.Context, a Approval, ok bool, text string) {
	status := Failed
	if ok {
		status = Succeeded
	}
	a, err := s.Store.updateApproval(a.ID, []string{Approved, Running}, func(a *Approval) {
		a.Status, a.FinishedAt, a.Result = status, s.now(), truncate(text, 2000)
	})
	if err != nil {
		s.Logger.Printf("bridge: finishing %s: %v", a.ID, err)
		return
	}
	head := a.ID + " (" + describeKind(a) + ") "
	if ok {
		head += "is done."
	} else {
		head += "failed."
	}
	s.Logger.Printf("bridge: %s %s", head, text)
	s.say(ctx, a, head+"\n\n"+text)
	s.tellAsker(a, head+"\n\n"+text)
}

// followApprovals ends requests nobody answered in time and reports those
// that have finished running.
func (s *Server) followApprovals(ctx context.Context) {
	open, err := s.Store.Approvals(false)
	if err != nil {
		s.Logger.Printf("bridge: approvals: %v", err)
		return
	}
	now := s.now()
	for _, a := range open {
		switch a.Status {
		case Asked:
			if now.Sub(a.AskedAt) > approvalTTL {
				s.Store.updateApproval(a.ID, []string{Asked}, func(a *Approval) { a.Status, a.FinishedAt = Expired, now })
				s.tellAsker(a, a.ID+" ("+describeKind(a)+") expired unanswered after a day. Ask again if it is still wanted.")
			}
		case Approved:
			// Approved but never started: mahamantri stopped in between.
			s.start(ctx, a)
		case Running:
			if s.SelfUpdate == nil {
				continue
			}
			if done, ok, text := s.SelfUpdate.Result(a); done {
				s.finish(ctx, a, ok, text)
			} else if now.Sub(a.StartedAt) > runLimit {
				s.finish(ctx, a, false, "no result after "+runLimit.String()+"; check `samrajya status` and the log of "+a.ID)
			}
		}
	}
}

// say posts in the conversation a request was asked in.
func (s *Server) say(ctx context.Context, a Approval, text string) {
	sf, err := s.surface(a.Conversation)
	if err == nil {
		_, err = sf.Deliver(ctx, a.Conversation, a.ThreadID, text)
	}
	if err != nil {
		s.Logger.Printf("bridge: could not post about %s in %s: %v", a.ID, a.Conversation, err)
	}
}

// tellAsker leaves a notice for the agent that asked, or the main session
// when that agent is gone.
func (s *Server) tellAsker(a Approval, text string) {
	to := MainAgent
	if ag, err := s.Store.Agent(a.Agent); err == nil && ag.Role != RoleMain && ag.TaskID == a.TaskID && (ag.State == AgentLive || ag.State == AgentStarting) {
		to = ag.ID
	}
	if err := s.Store.NoticeFor(to, "approval:"+a.ID+":"+a.Status, "approval", text); err != nil {
		s.Logger.Printf("bridge: could not tell %s about %s: %v", to, a.ID, err)
	}
	s.Wake()
}

func describeKind(a Approval) string {
	switch a.Kind {
	case Deploy:
		return "deploy " + shortRef(a.Ref) + " and restart Samrajya"
	case Restart:
		return "restart Samrajya"
	case RestartMain:
		return "replace the main session"
	}
	return a.Kind
}

func shortRef(ref string) string {
	if len(ref) > 10 {
		return ref[:10]
	}
	return ref
}

// askRestart is samrajya_restart: an agent asks to deploy or restart. The
// request is posted in the conversation, where only a trusted person can
// answer it.
func (s *Server) askRestart(w http.ResponseWriter, r *http.Request, agent string) {
	var body struct {
		Kind         string `json:"kind"`
		Ref          string `json:"ref"`
		Reason       string `json:"reason"`
		Conversation string `json:"conversation"`
		ThreadID     string `json:"threadId"`
		TaskID       string `json:"taskId"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Kind != Deploy && body.Kind != Restart && body.Kind != RestartMain {
		writeErr(w, http.StatusBadRequest, "kind is deploy, restart or restart_main")
		return
	}
	if strings.TrimSpace(body.Reason) == "" {
		writeErr(w, http.StatusBadRequest, "say why (reason): the person decides on it")
		return
	}
	if s.SelfUpdate == nil {
		writeErr(w, http.StatusNotImplemented, "this mahamantri cannot change itself (selfUpdate is not configured)")
		return
	}
	if body.Kind == Deploy && strings.TrimSpace(body.Ref) == "" {
		writeErr(w, http.StatusBadRequest, "a deploy needs ref: the commit or branch of the codebase to deploy")
		return
	}
	conv := body.Conversation
	if conv == "" && agent == MainAgent {
		conv = s.Home
	}
	conv, err := s.canonical(r.Context(), conv)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	conv, task, err := s.Store.MayReply(agent, conv)
	if err != nil {
		reply(w, nil, err)
		return
	}
	if body.TaskID == "" {
		body.TaskID = task.ID
	}
	if body.ThreadID == "" && agent != MainAgent {
		body.ThreadID = task.ThreadID
	}
	sf, err := s.surface(conv)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	ref, detail, err := s.SelfUpdate.Describe(ctx, body.Kind, body.Ref)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a, err := s.Store.askApproval(Approval{Kind: body.Kind, Ref: ref, Reason: strings.TrimSpace(body.Reason), Detail: detail,
		Agent: agent, TaskID: body.TaskID, Conversation: conv, ThreadID: body.ThreadID})
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	text := "Permission needed: " + describeKind(a) + ".\n\n" + a.Reason
	if detail != "" {
		text += "\n\n" + detail
	}
	text += "\n\nTo go ahead, send a message that says only: approve " + a.ID +
		"\nTo say no: refuse " + a.ID + "\n(This request lapses in 24 hours.)"
	dctx, dcancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer dcancel()
	posted, err := sf.Deliver(dctx, conv, body.ThreadID, text)
	if err != nil {
		s.Store.updateApproval(a.ID, []string{Asked}, func(x *Approval) {
			x.Status, x.FinishedAt, x.Result = Failed, s.now(), "could not be posted: "+err.Error()
		})
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "the request could not be posted: " + err.Error() + " - check the conversation before asking again"})
		return
	}
	s.Logger.Printf("bridge: %s asked by %s in %s (message %s): %s", a.ID, agent, conv, posted.ID, describeKind(a))
	writeJSON(w, http.StatusOK, map[string]any{"id": a.ID, "status": a.Status, "ref": a.Ref, "conversation": conv,
		"note": "Asked in " + conv + ". Mahamantri carries it out once the person approves, and tells you how it went; do not restart anything yourself."})
}
