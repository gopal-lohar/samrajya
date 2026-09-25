package opencode

import (
	"encoding/json"
	"fmt"
)

// structuredError mirrors the real v2.0.11 schema (Session.StructuredError):
// {type, message, status?}. There is no dedicated rate-limit/quota error
// type server-side - status is a plain HTTP-status-shaped integer, and we
// infer throttling/quota exhaustion from it.
type structuredError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Status  int    `json:"status"`
}

type sessionScoped struct {
	SessionID string `json:"sessionID"`
}

// sessionLifecycle mirrors the real session.created/session.updated event
// payload (verified live): {"sessionID":"...", ...}. Note this is a
// slimmer, event-specific shape - not the full Session.Info REST object,
// which keys the session by "id" instead.
type sessionLifecycle struct {
	SessionID string `json:"sessionID"`
	ParentID  string `json:"parentID"`
}

type permissionAsked struct {
	SessionID string   `json:"sessionID"`
	Action    string   `json:"action"`
	Resources []string `json:"resources"`
}

type formCreated struct {
	SessionID string `json:"sessionID"`
	Title     string `json:"title"`
}

type sessionErrorEvent struct {
	SessionID string           `json:"sessionID"`
	Error     *structuredError `json:"error"`
}

// classify never drops an event: unknown types fall through to Info with
// summary = the type string. sessions is updated as a side effect for
// session.created/updated so later events can be tagged as subagent-origin.
func classify(env envelope, sessions *sessionTracker) Event {
	ev := Event{
		ID:   env.ID,
		Type: env.Type,
		Raw:  env.Data,
	}

	var scoped sessionScoped
	json.Unmarshal(env.Data, &scoped)
	ev.SessionID = scoped.SessionID

	switch env.Type {
	case "session.created", "session.updated":
		var s sessionLifecycle
		json.Unmarshal(env.Data, &s)
		sessions.observe(s.SessionID, s.ParentID)
		ev.SessionID = s.SessionID
		ev.Severity = Info
		ev.Summary = fmt.Sprintf("%s: %s", env.Type, s.SessionID)

	case "permission.asked":
		var p permissionAsked
		json.Unmarshal(env.Data, &p)
		ev.Severity = Blocking
		ev.Summary = fmt.Sprintf("permission requested: %s on %v", p.Action, p.Resources)

	case "form.created":
		var f formCreated
		json.Unmarshal(env.Data, &f)
		ev.Severity = Blocking
		ev.Summary = fmt.Sprintf("form awaiting reply: %q", f.Title)

	case "session.error":
		var e sessionErrorEvent
		json.Unmarshal(env.Data, &e)
		ev.Severity, ev.Summary = classifySessionError(e.Error)

	case "session.tool.failed", "session.step.failed", "session.execution.failed":
		ev.Severity = Warning
		ev.Summary = env.Type

	case "session.retry.scheduled":
		ev.Severity = Warning
		ev.Summary = env.Type

	case "permission.replied", "permission.rejected", "form.replied", "form.cancelled":
		ev.Severity = Info
		ev.Summary = env.Type

	case "session.text.delta", "session.reasoning.delta", "session.compaction.delta":
		ev.Severity = Info
		ev.Summary = env.Type

	default:
		ev.Severity = Info
		ev.Summary = env.Type
	}

	if ev.Severity >= Warning && ev.SessionID != "" && sessions.isSubagent(ev.SessionID) {
		ev.Subagent = true
		ev.Summary = "[subagent] " + ev.Summary
	}
	return ev
}

func classifySessionError(err *structuredError) (Severity, string) {
	if err == nil {
		return Info, "session.error"
	}
	if err.Status == 429 || err.Status == 402 || err.Status >= 500 {
		return Blocking, fmt.Sprintf("session error (%s, status %d): %s", err.Type, err.Status, err.Message)
	}
	return Warning, fmt.Sprintf("session error (%s): %s", err.Type, err.Message)
}
