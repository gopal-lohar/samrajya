package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopal-lohar/samrajya/mahamantri/opencode"
)

// The test binary doubles as a fake `claude`: it speaks the stream-json
// protocol with the shapes captured from the real one (2.1.29x).
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_CLAUDE") == "1" {
		fakeClaude(os.Args[1:])
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeClaude(args []string) {
	if len(args) > 0 && args[0] == "agents" {
		data, err := os.ReadFile(os.Getenv("FAKE_AGENTS"))
		if err != nil {
			data = []byte("[]")
		}
		os.Stdout.Write(data)
		return
	}
	if log := os.Getenv("FAKE_LOG"); log != "" {
		f, _ := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		data, _ := json.Marshal(args)
		fmt.Fprintf(f, "%s\n", data)
		f.Close()
	}
	id, model := "", ""
	for i := 0; i < len(args)-1; i++ {
		switch args[i] {
		case "--session-id", "--resume":
			id = args[i+1]
		case "--model":
			model = args[i+1]
		}
	}
	emit := func(v map[string]any) {
		v["session_id"] = id
		data, _ := json.Marshal(v)
		os.Stdout.Write(append(data, '\n'))
	}
	result := func(text string, isErr bool) {
		emit(map[string]any{"type": "result", "subtype": "success", "is_error": isErr, "result": text,
			// Totals summed over the turn's API calls, as the real CLI reports;
			// the last call is in iterations.
			"usage": map[string]any{"input_tokens": 30, "output_tokens": 15, "cache_read_input_tokens": 3000,
				"iterations": []map[string]int{{"input_tokens": 10, "output_tokens": 5, "cache_read_input_tokens": 1000}}},
			"modelUsage": map[string]any{model: map[string]int{"contextWindow": 200000}}})
	}
	lines := make(chan map[string]any)
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 1<<20), 1<<24)
		for sc.Scan() {
			var m map[string]any
			json.Unmarshal(sc.Bytes(), &m)
			lines <- m
		}
		close(lines)
	}()
	text := func(m map[string]any) string {
		msg, _ := m["message"].(map[string]any)
		s, _ := msg["content"].(string)
		return s
	}
	emit(map[string]any{"type": "system", "subtype": "init", "model": model})
	for m := range lines {
		if m["type"] != "user" {
			continue
		}
		t := text(m)
		emit(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": t}})
		switch {
		case t == "CRASH":
			os.Exit(3)
		case t == "LIMIT":
			emit(map[string]any{"type": "rate_limit_event", "rate_limit_info": map[string]any{
				"status": "rejected", "resetsAt": time.Now().Add(2 * time.Hour).Unix(),
				"unifiedWindows": map[string]any{"five_hour": map[string]any{"utilization": 1.0, "resetsAt": time.Now().Add(2 * time.Hour).Unix()}}}})
			emit(map[string]any{"type": "assistant", "error": "rate_limit", "message": map[string]any{"model": "<synthetic>"}})
			result("You've hit your session limit · resets 2pm (UTC)", true)
		case t == "FAIL":
			emit(map[string]any{"type": "assistant", "error": "server_error", "message": map[string]any{"model": "<synthetic>"}})
			result("API Error: 529 Overloaded", true)
		case strings.HasPrefix(t, "SLOW"):
			// A long tool call: ends on an interrupt request, or after 20s.
			deadline := time.After(20 * time.Second)
		wait:
			for {
				select {
				case c, ok := <-lines:
					if !ok {
						return
					}
					if c["type"] == "control_request" {
						emit(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": c["request_id"]}})
						emit(map[string]any{"type": "user", "message": map[string]any{"content": "[Request interrupted by user for tool use]"}})
						emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true})
						break wait
					}
				case <-deadline:
					result("SLOW-DONE", false)
					break wait
				}
			}
		case strings.HasPrefix(t, "WAIT"):
			// A turn that takes a while; messages arriving meanwhile are
			// folded into it, as the real CLI does ("steer").
			got := []string{t}
			end := time.After(800 * time.Millisecond)
		collect:
			for {
				select {
				case c := <-lines:
					if c["type"] == "user" {
						got = append(got, text(c))
					}
				case <-end:
					break collect
				}
			}
			result("GOT:"+strings.Join(got, "|"), false)
		case t == "PROXY":
			// A Codex model through a proxy: per-call usage on each assistant
			// message, a summed result, and no iterations (captured live).
			for _, in := range []int{66230, 66446, 66470} {
				emit(map[string]any{"type": "assistant", "message": map[string]any{"model": model,
					"usage": map[string]int{"input_tokens": in}, "content": []any{map[string]any{"type": "text", "text": "x"}}}})
			}
			emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "PROXY-OK",
				"usage":      map[string]any{"input_tokens": 55452, "output_tokens": 83, "cache_read_input_tokens": 109568, "iterations": []any{}},
				"modelUsage": map[string]any{model: map[string]int{"contextWindow": 200000}}})
		default:
			emit(map[string]any{"type": "assistant", "message": map[string]any{"model": model, "content": []any{map[string]any{"type": "text", "text": "ECHO:" + t}}}})
			emit(map[string]any{"type": "rate_limit_event", "rate_limit_info": map[string]any{"status": "allowed", "resetsAt": 1791471600,
				"unifiedWindows": map[string]any{"five_hour": map[string]any{"utilization": 0.11, "resetsAt": 1791471600}}}})
			result("ECHO:"+t, false)
		}
	}
}

type rig struct {
	t      *testing.T
	h      *Harness
	events chan opencode.Event
	log    string
	agents string
	dir    string
	state  string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	dir := t.TempDir()
	r := &rig{t: t, events: make(chan opencode.Event, 256), log: filepath.Join(dir, "calls.log"),
		agents: filepath.Join(dir, "agents.json"), dir: dir, state: filepath.Join(dir, "state.json")}
	t.Setenv("FAKE_CLAUDE", "1")
	t.Setenv("FAKE_LOG", r.log)
	t.Setenv("FAKE_AGENTS", r.agents)
	r.h = r.open()
	t.Cleanup(r.h.Shutdown)
	return r
}

func (r *rig) open() *Harness {
	h, err := New(Options{Binary: os.Args[0], StateFile: r.state, IdleExit: time.Hour}, r.events)
	if err != nil {
		r.t.Fatal(err)
	}
	return h
}

func (r *rig) create(spec Spec) string {
	r.t.Helper()
	if spec.Dir == "" {
		spec.Dir = r.dir
	}
	id, err := r.h.Create(spec)
	if err != nil {
		r.t.Fatal(err)
	}
	return id
}

// next returns the next event of a type that matters, skipping "started".
func (r *rig) next() opencode.Event {
	r.t.Helper()
	for {
		select {
		case ev := <-r.events:
			if ev.Type == "session.execution.started" {
				continue
			}
			return ev
		case <-time.After(10 * time.Second):
			r.t.Fatal("no event within 10s")
		}
	}
}

func (r *rig) none(d time.Duration) {
	r.t.Helper()
	deadline := time.After(d)
	for {
		select {
		case ev := <-r.events:
			if ev.Type != "session.execution.started" {
				r.t.Fatalf("unexpected event %s: %s", ev.Type, ev.Summary)
			}
		case <-deadline:
			return
		}
	}
}

// spawns returns the argument lists the fake claude was started with.
func (r *rig) spawns() [][]string {
	data, _ := os.ReadFile(r.log)
	var out [][]string
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if l == "" {
			continue
		}
		var args []string
		json.Unmarshal([]byte(l), &args)
		out = append(out, args)
	}
	return out
}

func has(args []string, flag, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func (r *rig) waitState(id, want string) Info {
	r.t.Helper()
	for i := 0; i < 200; i++ {
		info, _ := r.h.Info(id)
		if info.State == want {
			return info
		}
		time.Sleep(25 * time.Millisecond)
	}
	info, _ := r.h.Info(id)
	r.t.Fatalf("state = %q, want %q", info.State, want)
	return info
}

func TestTurnsShareOneProcessAndReportTheReply(t *testing.T) {
	r := newRig(t)
	id := r.create(Spec{Title: "sainik-ENG-1-x", Model: "claude-haiku-4-5", Args: []string{"--dangerously-skip-permissions"}})
	if err := r.h.Prompt(id, "hello", ""); err != nil {
		t.Fatal(err)
	}
	ev := r.next()
	if ev.Type != "session.execution.succeeded" || ev.SessionID != id {
		t.Fatalf("event = %+v", ev)
	}
	if reply, _ := r.h.LastReply(context.Background(), id); reply != "ECHO:hello" {
		t.Errorf("reply = %q", reply)
	}
	r.h.Prompt(id, "again", "")
	r.next()
	sp := r.spawns()
	if len(sp) != 1 {
		t.Fatalf("spawned %d processes, want 1 kept for both turns", len(sp))
	}
	a := sp[0]
	if !has(a, "--session-id", id) || !has(a, "--model", "claude-haiku-4-5") || !has(a, "-n", "sainik-ENG-1-x") ||
		!has(a, "--input-format", "stream-json") || a[len(a)-1] != "--dangerously-skip-permissions" {
		t.Errorf("args = %v", a)
	}
	// The context in use is the last API call's, not the turn's sum.
	if tokens, window, model, ok := r.h.Usage(id); !ok || tokens != 1015 || window != 200000 || model != "claude-haiku-4-5" {
		t.Errorf("usage = %d %d %q %v", tokens, window, model, ok)
	}
	if l := r.h.Limits(); len(l) != 1 || l[0].Provider != "anthropic" || l[0].Windows["five_hour"].Utilization != 0.11 {
		t.Errorf("limits = %+v", l)
	}
}

// "queue" means after the current turn - and, as on opencode, the queued work
// runs in the same execution, so one completion covers both.
func TestQueuedMessageRunsAfterTheTurnWithOneCompletion(t *testing.T) {
	r := newRig(t)
	id := r.create(Spec{Title: "t"})
	r.h.Prompt(id, "WAIT", "")
	time.Sleep(200 * time.Millisecond)
	r.h.Prompt(id, "second", "queue")
	if info, _ := r.h.Info(id); info.State != "running" || info.Queued != 1 {
		t.Errorf("while busy: %+v", info)
	}
	ev := r.next()
	if ev.Type != "session.execution.succeeded" {
		t.Fatalf("event = %+v", ev)
	}
	if reply, _ := r.h.LastReply(context.Background(), id); reply != "ECHO:second" {
		t.Errorf("reply = %q - the queued message must not have been folded into the first turn", reply)
	}
	r.none(300 * time.Millisecond)
}

func TestSteerFoldsIntoTheRunningTurn(t *testing.T) {
	r := newRig(t)
	id := r.create(Spec{Title: "t"})
	r.h.Prompt(id, "WAIT", "")
	time.Sleep(200 * time.Millisecond)
	r.h.Prompt(id, "extra", "steer")
	r.next()
	if reply, _ := r.h.LastReply(context.Background(), id); reply != "GOT:WAIT|extra" {
		t.Errorf("reply = %q", reply)
	}
}

// Senapati's own interrupt produces no notice: it says what comes next itself.
func TestInterruptStopsTheTurnAndTheSessionContinues(t *testing.T) {
	r := newRig(t)
	id := r.create(Spec{Title: "t"})
	r.h.Prompt(id, "SLOW", "")
	r.waitState(id, "running")
	start := time.Now()
	if err := r.h.Interrupt(id); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("interrupt took %s", time.Since(start))
	}
	r.waitState(id, "idle")
	r.none(300 * time.Millisecond)
	r.h.Prompt(id, "after", "")
	if ev := r.next(); ev.Type != "session.execution.succeeded" {
		t.Fatalf("event = %+v", ev)
	}
	if len(r.spawns()) != 1 {
		t.Error("the same process should carry on after an interrupt")
	}
	if err := r.h.Interrupt(id); err != nil {
		t.Error("interrupting an idle session is a no-op, not an error")
	}
}

func TestFailedTurnIsReported(t *testing.T) {
	r := newRig(t)
	id := r.create(Spec{Title: "t"})
	r.h.Prompt(id, "FAIL", "")
	ev := r.next()
	if ev.Type != "session.execution.failed" || !strings.Contains(ev.Summary, "529") {
		t.Fatalf("event = %+v", ev)
	}
	r.waitState(id, "idle")
}

// A process that dies mid-turn is a failed turn; the next prompt resumes the
// session in a new process.
func TestCrashIsReportedAndTheSessionResumes(t *testing.T) {
	r := newRig(t)
	id := r.create(Spec{Title: "t"})
	r.h.Prompt(id, "CRASH", "")
	ev := r.next()
	if ev.Type != "session.execution.failed" || !strings.Contains(ev.Summary, "exited mid-turn") {
		t.Fatalf("event = %+v", ev)
	}
	r.h.Prompt(id, "back", "")
	if ev := r.next(); ev.Type != "session.execution.succeeded" {
		t.Fatalf("event = %+v", ev)
	}
	sp := r.spawns()
	if len(sp) != 2 || !has(sp[1], "--resume", id) {
		t.Errorf("spawns = %v", sp)
	}
}

func TestIdleProcessExitsAndTheNextTurnResumes(t *testing.T) {
	r := newRig(t)
	r.h.opt.IdleExit = 50 * time.Millisecond
	id := r.create(Spec{Title: "t"})
	r.h.Prompt(id, "one", "")
	r.next()
	time.Sleep(100 * time.Millisecond)
	r.h.Tick(context.Background())
	time.Sleep(100 * time.Millisecond)
	r.h.Prompt(id, "two", "")
	r.next()
	sp := r.spawns()
	if len(sp) != 2 || !has(sp[0], "--session-id", id) || !has(sp[1], "--resume", id) {
		t.Errorf("spawns = %v", sp)
	}
}

func TestModelChangeTakesEffectOnTheNextTurn(t *testing.T) {
	r := newRig(t)
	id := r.create(Spec{Title: "t", Model: "claude-haiku-4-5"})
	r.h.Prompt(id, "one", "")
	r.next()
	r.h.SetModel(id, "gpt-6-luna")
	r.h.Prompt(id, "two", "")
	r.next()
	sp := r.spawns()
	if len(sp) != 2 || !has(sp[1], "--model", "gpt-6-luna") || !has(sp[1], "--resume", id) {
		t.Errorf("spawns = %v", sp)
	}
}

// A usage limit pauses the session - nothing more is sent to it - until the
// reset, when it is told to continue and then gets what was held.
func TestUsageLimitPausesThenResumes(t *testing.T) {
	r := newRig(t)
	id := r.create(Spec{Title: "t", Model: "claude-haiku-4-5"})
	r.h.Prompt(id, "LIMIT", "")
	ev := r.next()
	if ev.Type != "session.execution.failed" || !strings.Contains(ev.Summary, "usage limit") || !strings.Contains(string(ev.Raw), `"limitedUntil"`) {
		t.Fatalf("event = %+v %s", ev, ev.Raw)
	}
	info := r.waitState(id, "limited")
	if info.LimitedUntil == nil || info.LimitedUntil.Before(time.Now().Add(90*time.Minute)) {
		t.Errorf("limitedUntil = %v, want the reset from the rate_limit_event (~2h)", info.LimitedUntil)
	}
	r.h.Prompt(id, "held", "")
	r.none(300 * time.Millisecond)
	if info, _ := r.h.Info(id); info.Queued != 1 {
		t.Errorf("queued = %d", info.Queued)
	}
	r.h.ResumeLimited(id)
	if ev := r.next(); ev.Type != "session.execution.succeeded" {
		t.Fatalf("event = %+v", ev)
	}
	if reply, _ := r.h.LastReply(context.Background(), id); reply != "ECHO:held" {
		t.Errorf("reply = %q: continue, then the held message, in one execution", reply)
	}
}

func TestUsageLimitResetIsReadFromTheMessageWhenNotReported(t *testing.T) {
	h := &Harness{limits: map[string]Limit{}, now: func() time.Time { return time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC) }}
	for text, want := range map[string]string{
		"You've hit your session limit · resets 2pm (UTC)":     "2026-10-09 14:00",
		"You've hit your session limit · resets 9am (UTC)":     "2026-10-10 09:00",
		"You've hit your session limit · resets 10:04am (UTC)": "2026-10-09 10:04",
		"something else": "2026-10-09 11:00",
	} {
		if got := h.resetTimeLocked("anthropic", text).Format("2006-01-02 15:04"); got != want {
			t.Errorf("%q -> %s, want %s", text, got, want)
		}
	}
}

func TestUsageLimitSwitchesToTheFallbackModelAndContinues(t *testing.T) {
	r := newRig(t)
	id := r.create(Spec{Title: "t", Model: "claude-haiku-4-5", FallbackModel: "gpt-6-luna"})
	r.h.Prompt(id, "LIMIT", "")
	if ev := r.next(); ev.Type != "session.model.fallback" || !strings.Contains(ev.Summary, "gpt-6-luna") {
		t.Fatalf("event = %+v", ev)
	}
	if ev := r.next(); ev.Type != "session.execution.succeeded" {
		t.Fatalf("event = %+v", ev)
	}
	sp := r.spawns()
	if len(sp) != 2 || !has(sp[1], "--model", "gpt-6-luna") || !has(sp[1], "--resume", id) {
		t.Errorf("spawns = %v", sp)
	}
	if reply, _ := r.h.LastReply(context.Background(), id); reply != "ECHO:"+ContinueAfterLimit {
		t.Errorf("reply = %q", reply)
	}
}

// A person opening the session with `claude --resume` takes it over: work is
// held, our process lets go, and it is handed back when they close it.
func TestManualTakeoverHoldsWorkAndHandsBack(t *testing.T) {
	r := newRig(t)
	id := r.create(Spec{Title: "t"})
	r.h.Prompt(id, "one", "")
	r.next()
	var ourPID int
	r.h.mu.Lock()
	for pid := range r.h.ours {
		ourPID = pid
	}
	r.h.mu.Unlock()

	// Our own process is listed too - that is not a person.
	os.WriteFile(r.agents, []byte(fmt.Sprintf(`[{"sessionId":%q,"pid":%d,"kind":"interactive"}]`, id, ourPID)), 0o644)
	r.h.Tick(context.Background())
	r.none(200 * time.Millisecond)

	os.WriteFile(r.agents, []byte(fmt.Sprintf(`[{"sessionId":%q,"pid":%d},{"sessionId":%q,"pid":424242,"kind":"interactive"}]`, id, ourPID, id)), 0o644)
	r.h.Tick(context.Background())
	if ev := r.next(); ev.Type != "session.manual.started" || ev.Summary != "pid 424242" {
		t.Fatalf("event = %+v", ev)
	}
	r.waitState(id, "manual")
	r.h.Prompt(id, "held", "")
	r.none(300 * time.Millisecond)

	// What the person did is only in the transcript; the hand-back carries it.
	TranscriptRoot = filepath.Join(r.dir, "projects")
	t.Cleanup(func() { TranscriptRoot = "" })
	os.MkdirAll(filepath.Join(TranscriptRoot, "-some-project"), 0o755)
	os.WriteFile(filepath.Join(TranscriptRoot, "-some-project", id+".jsonl"), []byte(
		`{"type":"user","message":{"content":"x"}}`+"\n"+
			`{"type":"assistant","message":{"content":[{"type":"text","text":"PERSON-DONE"}]}}`+"\n"+
			`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash"}]}}`+"\n"), 0o644)
	os.WriteFile(r.agents, []byte(`[]`), 0o644)
	r.h.Tick(context.Background())
	if ev := r.next(); ev.Type != "session.manual.ended" || !strings.Contains(string(ev.Raw), "PERSON-DONE") {
		t.Fatalf("event = %+v %s", ev, ev.Raw)
	}
	if ev := r.next(); ev.Type != "session.execution.succeeded" {
		t.Fatalf("event = %+v", ev)
	}
	sp := r.spawns()
	if len(sp) != 2 || !has(sp[1], "--resume", id) {
		t.Errorf("after the hand-back the session resumes in a new process: %v", sp)
	}
}

// Sessions survive a restart of mahamantri; one cut off mid-turn is reported.
func TestSessionsSurviveRestart(t *testing.T) {
	r := newRig(t)
	id := r.create(Spec{Title: "t", Model: "claude-haiku-4-5", Role: "ROLE"})
	r.h.Prompt(id, "one", "")
	r.next()
	r.h.Prompt(id, "SLOW", "")
	r.waitState(id, "running")
	r.h.Shutdown()

	h2 := r.open()
	t.Cleanup(h2.Shutdown)
	if !h2.Has(id) {
		t.Fatal("session forgotten across restart")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h2.Run(ctx)
	ev := r.next()
	if ev.Type != "session.execution.failed" || !strings.Contains(ev.Summary, "shut down") {
		t.Fatalf("event = %+v", ev)
	}
	h2.Prompt(id, "two", "")
	if ev := r.next(); ev.Type != "session.execution.succeeded" {
		t.Fatalf("event = %+v", ev)
	}
	last := r.spawns()[len(r.spawns())-1]
	if !has(last, "--resume", id) || !has(last, "--append-system-prompt", "ROLE") || !has(last, "--model", "claude-haiku-4-5") {
		t.Errorf("args = %v", last)
	}
}

func TestDeleteForgetsTheSession(t *testing.T) {
	r := newRig(t)
	id := r.create(Spec{Title: "t"})
	r.h.Prompt(id, "SLOW", "")
	r.waitState(id, "running")
	if err := r.h.Delete(id); err != nil {
		t.Fatal(err)
	}
	if r.h.Has(id) || r.h.Prompt(id, "x", "") != ErrNotFound {
		t.Error("deleted session still present")
	}
}

func TestCreateRejectsAMissingDirectory(t *testing.T) {
	r := newRig(t)
	if _, err := r.h.Create(Spec{Title: "t", Dir: filepath.Join(r.dir, "nope")}); err == nil {
		t.Error("want an error")
	}
}

func TestProviderOfModel(t *testing.T) {
	for model, want := range map[string]string{"claude-haiku-4-5": "anthropic", "opus": "anthropic", "": "anthropic",
		"gpt-6-luna": "openai", "gpt-5.3-codex": "openai", "grok-4.5": "grok", "kimi-k3": "kimi"} {
		if got := provider(model); got != want {
			t.Errorf("provider(%q) = %q, want %q", model, got, want)
		}
	}
}

// Regression: Senapati on gpt-6-sol rotated after every message - the turn's
// summed totals (165k) were read as its context (66k), against the 200k
// Claude Code assumes for proxied models.
func TestContextIsTheLastCallsUsageAndConfiguredWindowsWin(t *testing.T) {
	r := newRig(t)
	r.h.opt.ContextWindows = map[string]int{"gpt-6-sol": 272000}
	id := r.create(Spec{Title: "t", Model: "gpt-6-sol"})
	r.h.Prompt(id, "PROXY", "")
	r.next()
	tokens, window, model, ok := r.h.Usage(id)
	if !ok || tokens != 66470 || window != 272000 || model != "gpt-6-sol" {
		t.Errorf("usage = %d / %d (%s, %v), want 66470 / 272000", tokens, window, model, ok)
	}
}

func TestContextUseIsNotCarriedAcrossRestart(t *testing.T) {
	r := newRig(t)
	id := r.create(Spec{Title: "t"})
	r.h.Prompt(id, "one", "")
	r.next()
	if _, _, _, ok := r.h.Usage(id); !ok {
		t.Fatal("measured after a turn")
	}
	r.h.Shutdown()
	h2 := r.open()
	t.Cleanup(h2.Shutdown)
	if _, _, _, ok := h2.Usage(id); ok {
		t.Error("a restarted harness must re-measure, not trust a saved figure")
	}
}
