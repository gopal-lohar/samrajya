package bridge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeChat is a chat surface (like Telegram): a conversation per chat,
// replies to a message by its id.
type fakeChat struct {
	mu   sync.Mutex
	sent []string // "chat replyTo body"
	seen []string
	n    int
	fail bool
}

func (f *fakeChat) Name() string { return "chat" }

func (f *fakeChat) Deliver(_ context.Context, conv, thread, body string) (Posted, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return Posted{}, errors.New("down")
	}
	f.n++
	f.sent = append(f.sent, strings.TrimPrefix(conv, "chat:")+" "+thread+" "+body)
	id := fmt.Sprintf("m%d", 100+f.n)
	return Posted{ID: id, Thread: id}, nil
}

func (f *fakeChat) Received(_ context.Context, e Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, e.ID)
	return nil
}

func (f *fakeChat) Notify(ctx context.Context, e Event, text string) error {
	_, err := f.Deliver(ctx, e.Conversation, e.MessageRef(), text)
	return err
}

func (f *fakeChat) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

var chatPolicy = Policy{NudgeAfter: 2 * time.Minute}

func chatMsg(id, text string) Inbound {
	return Inbound{ID: "chat:1:" + id, Conversation: "chat:1", Title: "Chat with Asha", Thread: id, Ref: id,
		Kind: "message", Author: "Asha", AuthorID: "7", Trusted: true, Body: text}
}

func chatRig(t *testing.T) (*rig, *fakeChat) {
	r := newRig(t)
	chat := &fakeChat{}
	r.s.AddSurface(chat, chatPolicy)
	return r, chat
}

func ownerOf(t *testing.T, s *Store, id string) string {
	t.Helper()
	e, err := s.Event(id)
	if err != nil {
		t.Fatal(err)
	}
	return e.Owner()
}

// A chat message goes to the main; the main dispatches from it without
// giving the chat away: the worker reports in the chat, the chat's next
// message still goes to the main, and a reply to the worker's message goes
// to the worker.
func TestAChatStaysWithTheMainWhileItsWorkersReportThere(t *testing.T) {
	r, chat := chatRig(t)
	r.s.Receive(chatMsg("1", "please write the release notes"))
	if o := ownerOf(t, r.s.Store, "chat:1:1"); o != MainAgent {
		t.Fatalf("chat message went to %s", o)
	}
	r.tick()
	main := r.attach(MainAgent)
	code, task := main.do("POST", "/v1/dispatch", map[string]any{"title": "Release notes", "brief": "write them", "conversation": "chat:1", "threadId": "1",
		"events": []string{"chat:1:1"}})
	if code != http.StatusOK {
		t.Fatalf("dispatch: %d %v", code, task)
	}
	// The message that asked for it is the worker's task's, not left with the main.
	if e, _ := r.s.Store.Event("chat:1:1"); e.AcceptedAt == nil || e.TaskID != task["id"] {
		t.Errorf("asking message %+v", e)
	}
	if tasks, _ := r.s.Store.Tasks(true); len(tasks) != 1 {
		t.Errorf("%d tasks for one piece of work", len(tasks))
	}
	w := task["agent"].(string)
	r.tick()
	if b := r.l.briefs["samrajya-"+w+"-g1"]; !strings.Contains(b, "chat:1") {
		t.Errorf("brief does not say where to report: %q", b)
	}
	wc := r.attach(w)
	code, posted := wc.do("POST", "/v1/reply", map[string]any{"conversation": "chat:1", "threadId": "1", "body": "On it: drafting now."})
	if code != http.StatusOK {
		t.Fatalf("worker reply: %d %v", code, posted)
	}
	if code, _ := wc.do("POST", "/v1/reply", map[string]any{"conversation": "chat:2", "body": "x"}); code != http.StatusForbidden {
		t.Errorf("worker replied in a chat that is not its task's: %d", code)
	}
	r.s.Receive(chatMsg("2", "also, what's the weather?"))
	if o := ownerOf(t, r.s.Store, "chat:1:2"); o != MainAgent {
		t.Errorf("the chat's next message went to %s", o)
	}
	reply := chatMsg("3", "add the API changes too")
	reply.InReplyTo = posted["id"].(string)
	r.s.Receive(reply)
	if o := ownerOf(t, r.s.Store, "chat:1:3"); o != w {
		t.Errorf("a reply to the worker's message went to %s, want %s", o, w)
	}
	if got := chat.got(); len(got) != 1 || got[0] != "1 1 On it: drafting now." {
		t.Errorf("sent %v", got)
	}
}

// A chat routed to a task goes back to the main once the task is finished,
// unlike a Linear issue, whose task is reopened.
func TestARoutedChatGoesBackToTheMainWhenItsTaskIsDone(t *testing.T) {
	r, _ := chatRig(t)
	r.tick()
	main := r.attach(MainAgent)
	code, task := main.do("POST", "/v1/dispatch", map[string]any{"brief": "pair with Asha on the migration", "conversation": "chat:1", "route": true})
	if code != http.StatusOK {
		t.Fatalf("dispatch: %d %v", code, task)
	}
	w := task["agent"].(string)
	r.s.Receive(chatMsg("1", "the table is users"))
	if o := ownerOf(t, r.s.Store, "chat:1:1"); o != w {
		t.Fatalf("routed chat went to %s", o)
	}
	r.s.Store.UpdateTaskAs(w, task["id"].(string), TaskUpdate{Status: Done, Result: "migrated; checked row counts"})
	r.s.Receive(chatMsg("2", "thanks! now something else"))
	if o := ownerOf(t, r.s.Store, "chat:1:2"); o != MainAgent {
		t.Errorf("after the task was done the chat went to %s", o)
	}
	if t2, _ := r.s.Store.Task(task["id"].(string)); t2.Status != Done {
		t.Errorf("the finished task was reopened: %s", t2.Status)
	}
	// The worker may still report there.
	if c, _, err := r.s.Store.MayReply(w, "chat:1"); err != nil || c != "chat:1" {
		t.Errorf("worker may no longer report in the chat: %v", err)
	}
}

// Dispatch by a Linear issue's key meets pings on that issue (its UUID).
func TestDispatchNamesConversationsTheWayTheyArrive(t *testing.T) {
	r := newRig(t)
	r.s.Surfaces["linear"] = canonFake{r.fake}
	r.tick()
	main := r.attach(MainAgent)
	_, task := main.do("POST", "/v1/dispatch", map[string]any{"brief": "fix it", "conversation": "linear:ENG-1"})
	w := r.ping("ENG-1", "c1")
	if w != task["agent"] {
		t.Errorf("ping went to %s, dispatch made %v", w, task["agent"])
	}
}

type canonFake struct{ *fakeLinear }

func (c canonFake) Canonical(_ context.Context, conv string) (string, error) {
	return strings.Replace(conv, "linear:ENG-", "linear:uuid-ENG-", 1), nil
}

// A main session that cannot take new messages is reported to the person
// in the home conversation, not to itself.
func TestAStuckMainIsReportedToThePerson(t *testing.T) {
	r, chat := chatRig(t)
	r.s.Home = "chat:1"
	r.tick()
	r.attach(MainAgent)
	a := mustAgent(t, r, MainAgent)
	r.cli[MainAgent].do("POST", "/v1/turn", map[string]any{"agent": MainAgent, "secret": a.Secret, "state": "start"})
	r.advance(time.Minute)
	r.s.Receive(chatMsg("1", "are you there?"))
	r.advance(5 * time.Minute)
	r.keepAlive()
	r.tick()
	if got := chat.got(); len(got) != 0 {
		t.Fatalf("reported too early: %v", got)
	}
	r.advance(6 * time.Minute)
	r.keepAlive()
	r.tick()
	r.tick()
	got := chat.got()
	if len(got) != 1 || !strings.Contains(got[0], "main session has been in one step") {
		t.Errorf("sent %v", got)
	}
}

// A chat message left waiting is answered on its own surface, sooner than
// Linear's; an untrusted one is not.
func TestWaitingMessagesAreAnsweredOnTheirOwnSurface(t *testing.T) {
	s, fake, _ := linearServer(t)
	chat := &fakeChat{}
	s.AddSurface(chat, chatPolicy)
	now := time.Now()
	s.now = func() time.Time { return now }
	s.Store.now = s.now
	s.Receive(chatMsg("1", "hello?"))
	stranger := chatMsg("2", "hi")
	stranger.Trusted = false
	s.Receive(stranger)
	s.ingestPing(ping("c1", "c1"), "d1")
	now = now.Add(3 * time.Minute)
	s.nudge(context.Background())
	if got := chat.got(); len(got) != 1 || !strings.HasPrefix(got[0], "1 1 I've received this.") {
		t.Errorf("chat nudges %v", got)
	}
	if _, _, cm := fake.got(); len(cm) != 0 {
		t.Errorf("linear nudged after 3 minutes: %v", cm)
	}
}

// Messages and a surface's read position are stored together.
func TestABatchAndItsReadPositionAreStoredTogether(t *testing.T) {
	s, _ := openStore(t)
	bad := chatMsg("2", "x")
	bad.Conversation = "nowhere"
	if _, err := s.IngestBatch([]Inbound{chatMsg("1", "a"), bad}, "chat.offset", "3"); err == nil {
		t.Fatal("a bad batch was stored")
	}
	if p, _ := s.Pending(0); len(p) != 0 || s.Meta("chat.offset") != "" {
		t.Errorf("half a batch kept: %v offset %q", p, s.Meta("chat.offset"))
	}
	created, err := s.IngestBatch([]Inbound{chatMsg("1", "a"), chatMsg("2", "b")}, "chat.offset", "3")
	if err != nil || !created[0] || !created[1] || s.Meta("chat.offset") != "3" {
		t.Fatalf("batch: %v %v offset %q", created, err, s.Meta("chat.offset"))
	}
	again, _ := s.IngestBatch([]Inbound{chatMsg("2", "b")}, "chat.offset", "3")
	if again[0] {
		t.Error("a redelivered message was kept twice")
	}
}

// Work started before conversations existed keeps its follow-ups.
func TestOpenTasksAreLinkedToTheirIssuesOnUpgrade(t *testing.T) {
	s := workerStore(t)
	s.SetPolicy("linear", Policy{Reopen: true}) // as single-session mode stored them
	s.ingestPing(issuePing("ENG-1", "uuid-1", "c1", "c1"), "")
	task, _ := s.Accept([]string{"linear:c1"}, "", &NewTask{Title: "Fix"}, "fix")
	s.SetPolicy("linear", linearPolicy)
	if n, err := s.LinkOpenIssues(); err != nil || n != 1 {
		t.Fatalf("linked %d: %v", n, err)
	}
	if n, _ := s.LinkOpenIssues(); n != 0 {
		t.Error("linking twice")
	}
	s.ingestPing(issuePing("ENG-1", "uuid-1", "c2", "c1"), "")
	if o := owner(t, s, "c2"); o != MainAgent {
		t.Errorf("follow-up went to %s, not the task's owner", o)
	}
	if e, _ := s.Event("linear:c2"); e.Conversation != "linear:uuid-1" {
		t.Errorf("conversation %q", e.Conversation)
	}
	if tasks, _ := s.Tasks(true); len(tasks) != 1 {
		t.Errorf("%d tasks: the follow-up started new work", len(tasks))
	}
	_ = task
}

func (f *fakeChat) DeliverFiles(ctx context.Context, conv, thread, body string, files []File) (Posted, error) {
	names := []string{}
	for _, fl := range files {
		names = append(names, fl.Name)
	}
	return f.Deliver(ctx, conv, thread, body+" +"+strings.Join(names, ","))
}

// A reply can carry files, on a surface that takes them; bad paths are
// refused before anything is posted.
func TestARepliesFilesAreCheckedAndSent(t *testing.T) {
	r, chat := chatRig(t)
	r.tick()
	main := r.attach(MainAgent)
	dir := t.TempDir()
	shot := filepath.Join(dir, "shot.png")
	os.WriteFile(shot, []byte("png"), 0o600)
	for name, files := range map[string][]string{
		"relative": {"shot.png"}, "missing": {filepath.Join(dir, "nope.png")}, "directory": {dir},
	} {
		if code, _ := main.do("POST", "/v1/reply", map[string]any{"conversation": "chat:1", "body": "x", "files": files}); code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, code)
		}
	}
	if code, out := main.do("POST", "/v1/reply", map[string]any{"conversation": "chat:1", "body": "look", "files": []string{shot}}); code != http.StatusOK {
		t.Fatalf("reply with a file: %d %v", code, out)
	}
	if got := chat.got(); len(got) != 1 || got[0] != "1  look +shot.png" {
		t.Errorf("sent %q", got)
	}
	if code, _ := main.do("POST", "/v1/reply", map[string]any{"conversation": "linear:x", "body": "x", "files": []string{shot}}); code != http.StatusBadRequest {
		t.Errorf("files to a surface that cannot take them: %d", code)
	}
}
