package opencode

import "testing"

func TestClassifySessionError(t *testing.T) {
	cases := []struct {
		name string
		err  *structuredError
		want Severity
	}{
		// opencode retries these itself; a turn it gives up on ends in
		// session.execution.failed, which is what gets reported.
		{"rate limited", &structuredError{Type: "provider", Message: "too many requests", Status: 429}, Warning},
		{"payment required", &structuredError{Type: "provider", Message: "quota exceeded", Status: 402}, Warning},
		{"server error", &structuredError{Type: "provider", Message: "boom", Status: 502}, Warning},
		{"bad request", &structuredError{Type: "provider", Message: "bad input", Status: 400}, Warning},
		{"no status", &structuredError{Type: "unknown", Message: "aborted"}, Warning},
		{"nil error", nil, Info},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := classifySessionError(c.err)
			if got != c.want {
				t.Errorf("classifySessionError(%+v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

func TestClassifySubagentTagging(t *testing.T) {
	sessions := newSessionTracker()
	sessions.observe("ses_child", "ses_parent")

	env := envelope{ID: "evt_1", Type: "permission.asked", Data: []byte(`{"sessionID":"ses_child","action":"bash","resources":["*"]}`)}
	ev := classify(env, sessions)

	if !ev.Subagent {
		t.Errorf("expected event from child session to be tagged Subagent")
	}
	if ev.Severity != Blocking {
		t.Errorf("expected permission.asked to be Blocking, got %v", ev.Severity)
	}
}

// Shapes captured from a live 2.0.18 server.
func TestClassifyPermissionAndQuestionCarryTheirSessionAndID(t *testing.T) {
	sessions := newSessionTracker()
	perm := classify(envelope{Type: "permission.asked", Data: []byte(`{"id":"per_1","sessionID":"ses_a","action":"shell","resources":["echo hi"],"save":["echo *"]}`)}, sessions)
	if perm.SessionID != "ses_a" || perm.Severity != Blocking || perm.Summary != "permission request per_1: shell on [echo hi]" {
		t.Errorf("permission = %+v", perm)
	}
	// Regression: the form is wrapped, and its session was read from the top
	// level - so no question was ever attributed to a sainik.
	form := classify(envelope{Type: "form.created", Data: []byte(`{"form":{"id":"frm_1","sessionID":"ses_b","title":"Questions","fields":[{"key":"q0","title":"Color"}]}}`)}, sessions)
	if form.SessionID != "ses_b" || form.Severity != Blocking || form.Summary != `question frm_1 awaiting an answer: "Questions"` {
		t.Errorf("form = %+v", form)
	}
}
