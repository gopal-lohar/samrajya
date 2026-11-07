package bridge

import (
	"context"
	"sync"
	"time"
)

// tping is a Linear ping as the Linear adapter turns it into a message.
type tping struct {
	Kind, Issue, IssueID, IssueTitle, Author, Body, URL, CommentID, ThreadID, AuthorID string
}

// trusted lists, per store, the authors the test's Linear adapter trusts;
// none listed: everyone.
var trusted sync.Map // *Store -> map[string]bool

func setTrusted(s *Store, ids ...string) {
	m := map[string]bool{}
	for _, id := range ids {
		m[id] = true
	}
	trusted.Store(s, m)
}

func (p tping) inbound(s *Store, delivery string) Inbound {
	m, _ := trusted.Load(s)
	ok := m == nil || len(m.(map[string]bool)) == 0 || m.(map[string]bool)[p.AuthorID]
	title := p.Issue
	if p.IssueTitle != "" {
		title += ": " + p.IssueTitle
	}
	var links []string
	if p.Issue != "" {
		links = []string{p.Issue}
	}
	return Inbound{ID: "linear:" + p.CommentID, Conversation: "linear:" + p.IssueID, Title: title, Thread: p.ThreadID,
		Kind: p.Kind, Author: p.Author, AuthorID: p.AuthorID, Trusted: ok, Body: p.Body, URL: p.URL,
		Delivery: delivery, Ref: p.CommentID, Links: links}
}

func (s *Store) ingestPing(p tping, delivery string) error {
	_, err := s.Ingest(p.inbound(s, delivery))
	return err
}

func (s *Server) ingestPing(p tping, delivery string) error {
	return s.Receive(p.inbound(s.Store, delivery))
}

// linearPolicy is the Linear adapter's policy in mode claude.
var linearPolicy = Policy{StartsWork: true, Reopen: true, NudgeAfter: 15 * time.Minute}

// servers lets a test client attach as the main of the server at its URL.
var servers sync.Map // url -> *Server

// attach attaches the client as the main agent, as the main's plugin does.
func (c *client) attach(bool) int {
	v, ok := servers.Load(c.url)
	if !ok {
		c.t.Fatal("no server registered for " + c.url)
	}
	s := v.(*Server)
	s.Store.EnsureMain()
	a, err := s.Store.Launching(MainAgent, "samrajya")
	if err != nil {
		c.t.Fatal(err)
	}
	code, out := c.do("POST", "/v1/attach", map[string]any{"client": "test", "agent": MainAgent, "secret": a.Secret, "protocol": ProtocolVersion})
	if code == 200 {
		c.attachment = out["attachment"].(string)
	}
	return code
}

var _ = context.Background
