package linear

import (
	"context"
	"fmt"
	"strings"

	"github.com/gopal-lohar/samrajya/mahamantri/bridge"
)

// Surface is Linear as a Samrajya surface. A conversation is an issue
// (linear:<issue uuid>), a thread is a comment thread (its top comment's
// id). It is the webhook Handler's Sink: each ping becomes a message in the
// bridge's inbox, stored before the delivery is acknowledged.
type Surface struct {
	API      *API
	Receiver interface{ Receive(bridge.Inbound) error }
	// Trusted are the users whose comments may ask for work; empty:
	// everyone.
	Trusted map[string]bool
	// ReceivedEmoji marks each ping when stored; empty: none.
	ReceivedEmoji string
}

func (s *Surface) Name() string { return "linear" }

// Ingest stores a ping (Sink).
func (s *Surface) Ingest(p Ping, delivery string) error {
	id := "linear:" + p.CommentID
	if p.CommentID == "" {
		if delivery == "" {
			return fmt.Errorf("ping has neither a comment id nor a delivery id")
		}
		id = "linear-delivery:" + delivery
	}
	conv := "linear:" + p.IssueID
	if p.IssueID == "" {
		conv = "linear:" + p.Issue
	}
	title := p.Issue
	if p.IssueTitle != "" {
		title += ": " + p.IssueTitle
	}
	return s.Receiver.Receive(bridge.Inbound{
		ID: id, Conversation: conv, Title: title, Thread: p.ThreadID, Kind: p.Kind,
		Author: p.Author, AuthorID: p.AuthorID, Trusted: len(s.Trusted) == 0 || s.Trusted[p.AuthorID],
		Body: p.Body, URL: p.URL, Delivery: delivery, Ref: p.CommentID, Links: links(p.Issue),
	})
}

func (s *Surface) api() (*API, error) {
	if s.API == nil {
		return nil, fmt.Errorf("mahamantri has no Linear API key (linear.apiKey), so it cannot post on Linear")
	}
	return s.API, nil
}

// Deliver posts a comment as Senapati on the conversation's issue.
func (s *Surface) Deliver(ctx context.Context, conv, thread, body string) (bridge.Posted, error) {
	api, err := s.api()
	if err != nil {
		return bridge.Posted{}, err
	}
	issue, ok := strings.CutPrefix(conv, "linear:")
	if !ok || issue == "" {
		return bridge.Posted{}, fmt.Errorf("%q is not a Linear conversation (linear:<issue>)", conv)
	}
	p, err := api.Comment(ctx, issue, thread, body)
	if err != nil {
		return bridge.Posted{}, err
	}
	return bridge.Posted{ID: p.ID, Thread: p.ThreadID, URL: p.URL}, nil
}

// Canonical turns linear:ENG-1 into linear:<issue uuid>, the key pings
// arrive under, so a dispatch by identifier and a ping on the issue meet.
func (s *Surface) Canonical(ctx context.Context, conv string) (string, error) {
	issue, ok := strings.CutPrefix(conv, "linear:")
	if !ok || issue == "" || isUUID(issue) {
		return conv, nil
	}
	api, err := s.api()
	if err != nil {
		return "", err
	}
	id, err := api.IssueID(ctx, issue)
	if err != nil {
		return "", fmt.Errorf("issue %s: %w", issue, err)
	}
	return "linear:" + id, nil
}

// Received reacts to the ping's comment.
func (s *Surface) Received(ctx context.Context, e bridge.Event) error {
	if s.API == nil || s.ReceivedEmoji == "" || e.MessageRef() == "" {
		return nil
	}
	return s.API.React(ctx, e.MessageRef(), s.ReceivedEmoji)
}

// React adds emoji to a comment (message is its id) as Senapati.
func (s *Surface) React(ctx context.Context, _, message, emoji string) error {
	api, err := s.api()
	if err != nil {
		return err
	}
	return api.React(ctx, message, emoji)
}

// Notify replies in the ping's thread, mentioning its author.
func (s *Surface) Notify(ctx context.Context, e bridge.Event, text string) error {
	api, err := s.api()
	if err != nil {
		return err
	}
	if e.AuthorID != "" {
		if h, err := api.Handle(ctx, e.AuthorID); err == nil && h != "" {
			text = "@" + h + " " + text
		}
	}
	_, err = s.Deliver(ctx, e.Conversation, e.ThreadID, text)
	return err
}

// ShowStatus sets Senapati's Linear status.
func (s *Surface) ShowStatus(ctx context.Context, st bridge.Status) error {
	if s.API == nil {
		return nil
	}
	return s.API.SetStatus(ctx, st.Emoji, st.Label)
}

func links(issue string) []string {
	if issue == "" {
		return nil
	}
	return []string{issue}
}
