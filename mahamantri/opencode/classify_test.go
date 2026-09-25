package opencode

import "testing"

func TestClassifySessionError(t *testing.T) {
	cases := []struct {
		name string
		err  *structuredError
		want Severity
	}{
		{"rate limited", &structuredError{Type: "provider", Message: "too many requests", Status: 429}, Blocking},
		{"payment required", &structuredError{Type: "provider", Message: "quota exceeded", Status: 402}, Blocking},
		{"server error", &structuredError{Type: "provider", Message: "boom", Status: 502}, Blocking},
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
