package linear

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// envelope covers the common shape of a Linear webhook payload - enough
// fields to build a short natural-language sentence, not a full mirror of
// Linear's schema (Senapati has its own Linear MCP access for full detail).
type envelope struct {
	Action string `json:"action"`
	Type   string `json:"type"`
	Data   struct {
		Identifier string `json:"identifier"`
		Title      string `json:"title"`
		State      struct {
			Name string `json:"name"`
		} `json:"state"`
		AssigneeID string `json:"assigneeId"`
		Assignee   struct {
			Name string `json:"name"`
		} `json:"assignee"`
		PriorityLabel string `json:"priorityLabel"`
		Body          string `json:"body"`
		IssueID       string `json:"issueId"`
		Issue         struct {
			Identifier string `json:"identifier"`
			Title      string `json:"title"`
		} `json:"issue"`
	} `json:"data"`
	// UpdatedFrom holds the previous value of every field an update changed;
	// only its keys matter here, to say what changed.
	UpdatedFrom map[string]json.RawMessage `json:"updatedFrom"`
	Actor       struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"actor"`
	URL string `json:"url"`
}

// noiseFields change on nearly every update and say nothing about intent.
var noiseFields = map[string]bool{
	"updatedAt": true, "sortOrder": true, "boardOrder": true, "subIssueSortOrder": true,
	"prioritySortOrder": true, "subscriberIds": true,
}

// describeChanges turns updatedFrom's keys into short phrases using the
// issue's current values, so an assignee change isn't reported as a state.
func describeChanges(env envelope) []string {
	var out []string
	for field := range env.UpdatedFrom {
		switch field {
		case "stateId":
			out = append(out, fmt.Sprintf("state changed to %q", env.Data.State.Name))
		case "assigneeId":
			if env.Data.Assignee.Name == "" {
				out = append(out, "unassigned")
			} else {
				out = append(out, "assigned to "+env.Data.Assignee.Name)
			}
		case "priority":
			out = append(out, fmt.Sprintf("priority changed to %q", env.Data.PriorityLabel))
		case "title":
			out = append(out, fmt.Sprintf("title changed to %q", env.Data.Title))
		default:
			if !noiseFields[field] {
				out = append(out, field+" changed")
			}
		}
	}
	sort.Strings(out)
	return out
}

// Identity is the Linear user Senapati acts as. With it, events are worded
// from Senapati's point of view - "assigned to you", "mentioned you" - and
// the handle in a comment like "@senapati-bot" is recognised as Senapati
// itself rather than some other person. The zero value means unknown, and
// summaries fall back to neutral wording.
type Identity struct {
	UserID string
	Name   string
	Handle string
}

func (id Identity) known() bool { return id.UserID != "" }

// mentionedIn reports whether text @-mentions this user by handle or name.
func (id Identity) mentionedIn(text string) bool {
	text = strings.ToLower(text)
	for _, n := range []string{id.Handle, id.Name} {
		if n != "" && strings.Contains(text, "@"+strings.ToLower(n)) {
			return true
		}
	}
	return false
}

func (id Identity) label() string {
	if id.Name != "" {
		return "you (" + id.Name + ")"
	}
	return "you"
}

// previousAssignee is the assignee an update replaced, from updatedFrom.
func previousAssignee(env envelope) string {
	var prev string
	json.Unmarshal(env.UpdatedFrom["assigneeId"], &prev)
	return prev
}

// Summarize extracts a short natural-language description of a Linear
// webhook payload for Senapati's prompt inbox. Events that call for action
// say so plainly - an assignment to Senapati is an instruction to start
// work, and a bare "assigned to Senapati" reads as a notification to
// acknowledge. This never replaces the full log written by logDelivery, and
// never errors: an unrecognised payload still produces a line.
func Summarize(eventType string, body []byte, self Identity) string {
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Sprintf("Linear webhook received (event=%s, unparseable payload)", eventType)
	}

	who := env.Actor.Name
	if who == "" {
		who = "someone"
	}
	d := env.Data
	mine := self.known() && d.AssigneeID == self.UserID

	switch env.Type {
	case "Issue":
		if d.Identifier == "" {
			break
		}
		switch env.Action {
		case "create":
			if mine {
				return fmt.Sprintf("%s created issue %s (%q) and assigned it to %s. Take it on now, following your instructions.", who, d.Identifier, d.Title, self.label())
			}
			return fmt.Sprintf("%s created issue %s: %q", who, d.Identifier, d.Title)
		case "update":
			_, reassigned := env.UpdatedFrom["assigneeId"]
			switch {
			case reassigned && mine:
				return fmt.Sprintf("%s assigned issue %s (%q) to %s. Take it on now, following your instructions.", who, d.Identifier, d.Title, self.label())
			case reassigned && self.known() && previousAssignee(env) == self.UserID:
				now := "unassigned"
				if d.Assignee.Name != "" {
					now = "assigned to " + d.Assignee.Name
				}
				return fmt.Sprintf("%s took issue %s (%q) away from you - it is now %s. Stop any work you started on it.", who, d.Identifier, d.Title, now)
			}
			changes := describeChanges(env)
			switch {
			case len(changes) > 0 && mine:
				return fmt.Sprintf("%s updated issue %s (%q), which is assigned to you: %s.", who, d.Identifier, d.Title, strings.Join(changes, "; "))
			case len(changes) > 0:
				return fmt.Sprintf("%s updated issue %s (%q): %s", who, d.Identifier, d.Title, strings.Join(changes, "; "))
			}
			return fmt.Sprintf("%s updated issue %s: %q", who, d.Identifier, d.Title)
		case "remove":
			return fmt.Sprintf("%s removed issue %s: %q", who, d.Identifier, d.Title)
		}
	case "Comment":
		// Comment payloads embed the issue as {id,title}; the human-readable
		// identifier isn't guaranteed there, so fall back rather than drop it.
		issue := firstNonEmpty(d.Issue.Identifier, d.Issue.Title, d.IssueID)
		if issue == "" {
			break
		}
		if env.Action == "create" {
			if self.mentionedIn(d.Body) {
				return fmt.Sprintf("%s commented on issue %s and mentioned you: %q - respond to it or act on it.", who, issue, truncate(d.Body, 300))
			}
			return fmt.Sprintf("%s commented on issue %s: %q", who, issue, truncate(d.Body, 200))
		}
	}

	return fmt.Sprintf("%s: action=%s type=%s on Linear (event=%s)", who, env.Action, env.Type, eventType)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
