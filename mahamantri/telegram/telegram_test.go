package telegram

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gopal-lohar/samrajya/mahamantri/bridge"
)

// fakeAPI is the Bot API: it serves queued updates and records calls.
type fakeAPI struct {
	mu        sync.Mutex
	updates   []map[string]any
	calls     []string // "method json"
	limitOnce bool
	next      int64
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	body, _ := io.ReadAll(r.Body)
	f.calls = append(f.calls, method+" "+string(body))
	ok := func(v any) { json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": v}) }
	switch method {
	case "getUpdates":
		var p struct{ Offset int64 }
		json.Unmarshal(body, &p)
		var out []map[string]any
		for _, u := range f.updates {
			if int64(u["update_id"].(int)) >= p.Offset {
				out = append(out, u)
			}
		}
		ok(out)
	case "sendMessage":
		if f.limitOnce {
			f.limitOnce = false
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 429, "description": "Too Many Requests", "parameters": map[string]any{"retry_after": 1}})
			return
		}
		f.next++
		ok(map[string]any{"message_id": 500 + f.next})
	default:
		ok(true)
	}
}

func (f *fakeAPI) called(method string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, method+" ") {
			out = append(out, strings.TrimPrefix(c, method+" "))
		}
	}
	return out
}

func msg(update int, chat, from int64, chatType, text string) map[string]any {
	return map[string]any{"update_id": update, "message": map[string]any{
		"message_id": 10 + update, "date": 1, "text": text,
		"from": map[string]any{"id": from, "first_name": "Asha"},
		"chat": map[string]any{"id": chat, "type": chatType},
	}}
}

func setup(t *testing.T) (*fakeAPI, *Bot, *bridge.Server) {
	t.Helper()
	api := &fakeAPI{}
	ts := httptest.NewServer(api)
	t.Cleanup(ts.Close)
	store, err := bridge.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	quiet := log.New(io.Discard, "", 0)
	bot := &Bot{Token: "T", URL: ts.URL, TrustedUsers: map[int64]bool{7: true}, Chats: map[int64]bool{-100: true},
		ReceivedEmoji: "👀", Logger: quiet, PollSeconds: 1}
	srv := &bridge.Server{Store: store, Token: "x", Logger: quiet}
	srv.AddSurface(bot, bridge.Policy{})
	return api, bot, srv
}

func TestOnlyTrustedPeopleInAllowedChatsAreTaken(t *testing.T) {
	api, bot, srv := setup(t)
	api.updates = []map[string]any{
		msg(1, 7, 7, "private", "hello"),         // the person, in private
		msg(2, 9, 9, "private", "spam"),          // a stranger
		msg(3, -100, 7, "group", "in the group"), // the person, in a listed group
		msg(4, -200, 7, "group", "elsewhere"),    // the person, in another group
		msg(5, -100, 9, "group", "stranger"),     // a stranger, in the listed group
	}
	if err := bot.pollOnce(context.Background(), srv); err != nil {
		t.Fatal(err)
	}
	p, _ := srv.Store.Pending(0)
	if len(p) != 2 || p[0].ID != "telegram:7:11" || p[1].ID != "telegram:-100:13" {
		t.Fatalf("pending %+v", p)
	}
	if p[0].Conversation != "telegram:7" || p[0].ThreadID != "11" || !p[0].Trusted || p[0].Owner() != bridge.MainAgent {
		t.Errorf("event %+v", p[0])
	}
	if srv.Meta(offsetKey) != "6" {
		t.Errorf("offset %q", srv.Meta(offsetKey))
	}
	// A restart that sees the same updates again (the offset was not
	// confirmed to Telegram) keeps nothing twice.
	srv.Store.SetMeta(offsetKey, "0")
	bot.pollOnce(context.Background(), srv)
	if again, _ := srv.Store.Pending(0); len(again) != 2 {
		t.Errorf("redelivery kept twice: %d", len(again))
	}
}

func TestRepliesVoiceAndEditsAreDescribed(t *testing.T) {
	_, bot, _ := setup(t)
	m := &message{MessageID: 20, From: &user{ID: 7, FirstName: "Asha"}, Caption: "listen",
		Voice: &file{FileID: "f-voice", Duration: 12}, Reply: &message{MessageID: 501, Text: "On it: drafting"}}
	m.Chat.ID, m.Chat.Type = 7, "private"
	in, ok, why := bot.inbound(m, false)
	if !ok {
		t.Fatal(why)
	}
	if in.InReplyTo != "501" || len(in.Attachments) != 1 || in.Attachments[0].Kind != "voice" || in.Attachments[0].Seconds != 12 ||
		!strings.Contains(in.Body, "(replying to: On it: drafting)") || !strings.HasSuffix(in.Body, "listen") {
		t.Errorf("inbound %+v", in)
	}
	m.EditDate = 99
	ed, _, _ := bot.inbound(m, true)
	if ed.ID != "telegram:7:20:edit:99" || ed.Kind != "edit" {
		t.Errorf("edit %+v", ed)
	}
	m.Voice, m.Caption, m.Reply = nil, "", nil
	if _, ok, _ := bot.inbound(m, false); ok {
		t.Error("an empty message was taken")
	}
}

func TestDeliverRepliesSplitsAndWaitsOutARateLimit(t *testing.T) {
	api, bot, _ := setup(t)
	api.limitOnce = true
	long := strings.Repeat("word ", 1000) // 5000 bytes: two messages
	p, err := bot.Deliver(context.Background(), "telegram:7", "11", long)
	if err != nil {
		t.Fatal(err)
	}
	sent := api.called("sendMessage")
	if len(sent) != 3 { // the limited try, then two parts
		t.Fatalf("sent %d: %v", len(sent), sent)
	}
	if p.ID != "501" || !strings.Contains(sent[1], `"reply_parameters":{"allow_sending_without_reply":true,"message_id":11}`) ||
		strings.Contains(sent[2], "reply_parameters") {
		t.Errorf("posted %+v, sent %v", p, sent)
	}
	for _, part := range split(long, maxText) {
		if len(part) > maxText {
			t.Errorf("part of %d bytes", len(part))
		}
	}
	if _, err := bot.Deliver(context.Background(), "linear:x", "", "hi"); err == nil {
		t.Error("delivered to another surface's conversation")
	}
}

func TestReceivedReactsToTheMessage(t *testing.T) {
	api, bot, _ := setup(t)
	bot.Received(context.Background(), bridge.Event{Conversation: "telegram:7", Ref: "11"})
	if r := api.called("setMessageReaction"); len(r) != 1 || !strings.Contains(r[0], `"message_id":11`) || !strings.Contains(r[0], "👀") {
		t.Errorf("reactions %v", r)
	}
}

func TestReactSetsTheReaction(t *testing.T) {
	api, bot, _ := setup(t)
	if err := bot.React(context.Background(), "telegram:7", "110", "👍"); err != nil {
		t.Fatal(err)
	}
	r := api.called("setMessageReaction")
	if len(r) != 1 || !strings.Contains(r[0], `"chat_id":"7"`) || !strings.Contains(r[0], `"message_id":110`) || !strings.Contains(r[0], `"emoji":"👍"`) {
		t.Errorf("reactions %v", r)
	}
	if bot.React(context.Background(), "linear:x", "110", "👍") == nil || bot.React(context.Background(), "telegram:7", "abc", "👍") == nil {
		t.Error("reacted to something that is not a Telegram message")
	}
}

func TestErrorsNeverCarryTheToken(t *testing.T) {
	bot := &Bot{Token: "SECRET-TOKEN", URL: "http://127.0.0.1:1", Logger: log.New(io.Discard, "", 0)}
	_, err := bot.Deliver(context.Background(), "telegram:7", "", "hi")
	if err == nil || strings.Contains(err.Error(), "SECRET-TOKEN") {
		t.Errorf("error %v", err)
	}
}

// uploadAPI records multipart sends.
type uploadAPI struct {
	mu    sync.Mutex
	sends []string // "method field=filename caption=... reply=..."
	n     int64
}

func (u *uploadAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	r.ParseMultipartForm(1 << 20)
	desc := method
	for field, fh := range r.MultipartForm.File {
		data, _ := io.ReadAll(must(fh[0].Open()))
		desc += " " + field + "=" + fh[0].Filename + ":" + string(data)
	}
	desc += " caption=" + r.FormValue("caption") + " reply=" + r.FormValue("reply_parameters")
	u.mu.Lock()
	u.sends = append(u.sends, desc)
	u.n++
	n := u.n
	u.mu.Unlock()
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": 700 + n}})
}

func must[T any](v T, _ error) T { return v }

func TestFilesGoAsPhotosOrDocumentsWithTheCaption(t *testing.T) {
	api := &uploadAPI{}
	ts := httptest.NewServer(api)
	defer ts.Close()
	bot := &Bot{Token: "T", URL: ts.URL, Logger: log.New(io.Discard, "", 0)}
	dir := t.TempDir()
	shot, notes := filepath.Join(dir, "shot.PNG"), filepath.Join(dir, "notes.txt")
	os.WriteFile(shot, []byte("png"), 0o600)
	os.WriteFile(notes, []byte("txt"), 0o600)
	files := []bridge.File{{Path: shot, Name: "shot.PNG", Size: 3}, {Path: notes, Name: "notes.txt", Size: 3}}
	p, err := bot.DeliverFiles(context.Background(), "telegram:7", "11", "Here it is", files)
	if err != nil || p.ID != "701" {
		t.Fatalf("posted %+v %v", p, err)
	}
	if len(api.sends) != 2 ||
		!strings.HasPrefix(api.sends[0], "sendPhoto photo=shot.PNG:png caption=Here it is reply={\"allow_sending_without_reply\":true,\"message_id\":11}") ||
		!strings.HasPrefix(api.sends[1], "sendDocument document=notes.txt:txt caption= reply=") {
		t.Errorf("sends %q", api.sends)
	}
}
