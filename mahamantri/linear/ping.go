package linear

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// envelope is the part of a Linear webhook payload mahamantri reads: enough
// to decide whether a delivery is addressed to Senapati and to say what it
// is, not a mirror of Linear's schema (Senapati reads the issue itself
// through its Linear tools).
type envelope struct {
	Action string `json:"action"`
	Type   string `json:"type"`
	Data   struct {
		ID       string          `json:"id"`
		Body     string          `json:"body"`
		IssueID  string          `json:"issueId"`
		ParentID string          `json:"parentId"`
		UserID   string          `json:"userId"`
		BotActor json.RawMessage `json:"botActor"`
		User     struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"user"`
		Issue struct {
			Identifier string `json:"identifier"`
			Title      string `json:"title"`
		} `json:"issue"`
	} `json:"data"`
	Actor struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"actor"`
	URL string `json:"url"`
}

func parse(body []byte) (envelope, bool) {
	var env envelope
	return env, json.Unmarshal(body, &env) == nil
}

// Identity is the Linear user Senapati acts as. UserID is how its own
// comments are recognised (and their threads remembered); Handle - and Name,
// when it is a single word - is what a comment @-mentions it as.
type Identity struct {
	UserID string
	Name   string
	Handle string
}

// mentionedIn reports whether text @-mentions this user: "@handle" (or
// "@Name") as a whole word, or a link to its Linear profile.
func (id Identity) mentionedIn(text string) bool {
	text = strings.ToLower(text)
	for _, n := range []string{id.Handle, id.Name} {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" || strings.ContainsAny(n, " \t\n") {
			continue
		}
		if containsWord(text, "@"+n) || containsWord(text, "/profiles/"+n) {
			return true
		}
	}
	return false
}

// containsWord finds needle not followed by more of a name, so "@sena" does
// not match "@senapati". A dot ends the name unless a letter or digit
// follows it ("@asha.rao" is one handle; "@senapati." ends a sentence).
func containsWord(text, needle string) bool {
	for from := 0; ; {
		i := strings.Index(text[from:], needle)
		if i < 0 {
			return false
		}
		end := from + i + len(needle)
		if !continuesName(text[end:]) {
			return true
		}
		from = from + i + 1
	}
}

func continuesName(rest string) bool {
	r, size := utf8.DecodeRuneInString(rest)
	switch {
	case size == 0:
		return false
	case r == '.':
		next, _ := utf8.DecodeRuneInString(rest[size:])
		return unicode.IsLetter(next) || unicode.IsDigit(next)
	default:
		return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-'
	}
}

// ThreadIndex answers whether Senapati has written in a comment thread.
// Linear threads are one level deep: a reply's parentId is the thread's top
// comment, whoever wrote the comment being answered.
type ThreadIndex interface {
	Has(threadID string) bool
}

// Ping is a Linear comment addressed to Senapati.
type Ping struct {
	Kind       string // "mention" | "reply"
	Issue      string // identifier, e.g. ENG-31
	IssueTitle string
	Author     string
	Body       string
	URL        string

	// What identifies the comment for good, where the fields above are for
	// reading: a durable inbox deduplicates on CommentID and replies in
	// ThreadID (the thread's top comment; the comment itself if it starts
	// one).
	IssueID   string
	CommentID string
	ThreadID  string
	AuthorID  string
}

// Classify decides what a webhook delivery is to Senapati. Only two things
// are pings: a person's new comment that @-mentions it, and a person's new
// reply in a thread Senapati has commented in. Everything else - issue
// edits of any kind (labels, status, assignment, priority...), comment edits
// and deletions, integrations' comments, Senapati's own comments, comments
// not addressed to it - is not, and why says so for the log.
func Classify(eventType string, body []byte, self Identity, threads ThreadIndex) (p Ping, ok bool, why string) {
	env, parsed := parse(body)
	switch {
	case !parsed:
		return Ping{}, false, "unparseable payload"
	case env.Type != "Comment":
		return Ping{}, false, fmt.Sprintf("%s %s - only comments addressed to Senapati are forwarded", env.Type, env.Action)
	case env.Action != "create":
		return Ping{}, false, "comment " + env.Action + " - only new comments are forwarded"
	case isSelf(env, self):
		return Ping{}, false, "Senapati's own comment"
	case len(env.Data.BotActor) > 0 && string(env.Data.BotActor) != "null", env.Actor.Type != "" && env.Actor.Type != "user":
		return Ping{}, false, "comment by an integration, not a person"
	}

	d := env.Data
	p = Ping{
		Issue:      firstNonEmpty(d.Issue.Identifier, d.IssueID),
		IssueTitle: d.Issue.Title,
		Author:     firstNonEmpty(d.User.Name, env.Actor.Name, "someone"),
		Body:       d.Body,
		URL:        env.URL,
		IssueID:    d.IssueID,
		CommentID:  d.ID,
		ThreadID:   firstNonEmpty(d.ParentID, d.ID),
		AuthorID:   firstNonEmpty(d.User.ID, d.UserID, env.Actor.ID),
	}
	switch {
	case self.mentionedIn(d.Body):
		p.Kind = "mention"
	case d.ParentID != "" && threads != nil && threads.Has(d.ParentID):
		p.Kind = "reply"
	default:
		return Ping{}, false, "comment does not mention Senapati and is not a reply in its thread"
	}
	return p, true, ""
}

func isSelf(env envelope, self Identity) bool {
	return self.UserID != "" && (env.Data.UserID == self.UserID || env.Actor.ID == self.UserID)
}

// ownComment reports the thread a comment by Senapati belongs to, so replies
// in it are recognised later.
func ownComment(body []byte, self Identity) (threadID, issue string, ok bool) {
	env, parsed := parse(body)
	if !parsed || env.Type != "Comment" || env.Action != "create" || !isSelf(env, self) || env.Data.ID == "" {
		return "", "", false
	}
	return firstNonEmpty(env.Data.ParentID, env.Data.ID), firstNonEmpty(env.Data.Issue.Identifier, env.Data.IssueID), true
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
