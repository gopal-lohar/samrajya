// Package telegram is Telegram as a Samrajya surface: a bot the person
// writes to. Each chat is a conversation (telegram:<chat id>); a reply goes
// to the message it answers. Updates are long-polled (no public webhook is
// needed) and stored together with the read position, so a restart neither
// loses nor repeats a message.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gopal-lohar/samrajya/mahamantri/bridge"
)

const apiURL = "https://api.telegram.org"

// maxText is Telegram's limit for one message; longer replies are split.
const maxText = 4096

// Receiver is where messages go: the bridge server.
type Receiver interface {
	ReceiveBatch(ins []bridge.Inbound, metaKey, metaValue string) error
	Meta(key string) string
}

// Bot is a Telegram bot as a surface.
type Bot struct {
	Token string
	// URL replaces Telegram's Bot API base (for testing).
	URL string
	// TrustedUsers are the user ids whose messages are taken; others are
	// dropped (and logged), so strangers cannot spend the account's usage.
	TrustedUsers map[int64]bool
	// Chats are group chats messages are also taken from (from trusted
	// users only). Private chats with a trusted user always are.
	Chats map[int64]bool
	// ReceivedEmoji is the reaction a stored message gets; empty: none.
	ReceivedEmoji string
	Logger        *log.Logger
	Client        *http.Client
	// PollSeconds is getUpdates' long-poll timeout. Default 25.
	PollSeconds int
}

func (b *Bot) Name() string { return "telegram" }

const offsetKey = "telegram.offset"

type apiError struct {
	Code       int
	Desc       string
	RetryAfter int
}

func (e *apiError) Error() string { return fmt.Sprintf("telegram: %d %s", e.Code, e.Desc) }

// call calls a Bot API method with a JSON body.
func (b *Bot) call(ctx context.Context, method string, params any, out any) error {
	body, _ := json.Marshal(params)
	base := b.URL
	if base == "" {
		base = apiURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/bot"+b.Token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := b.Client
	if client == nil {
		client = &http.Client{Timeout: time.Duration(b.poll()+15) * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		// The URL holds the token: never let it reach a log.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("telegram %s: %w", method, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	var reply struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return fmt.Errorf("telegram %s: %s", method, res.Status)
	}
	if !reply.OK {
		return &apiError{Code: reply.ErrorCode, Desc: reply.Description, RetryAfter: reply.Parameters.RetryAfter}
	}
	if out != nil {
		return json.Unmarshal(reply.Result, out)
	}
	return nil
}

func (b *Bot) poll() int {
	if b.PollSeconds <= 0 {
		return 25
	}
	return b.PollSeconds
}

type user struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

func (u user) name() string {
	n := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if n == "" {
		n = u.Username
	}
	return n
}

type file struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	Duration int    `json:"duration"`
}

type message struct {
	MessageID int64 `json:"message_id"`
	From      *user `json:"from"`
	Chat      struct {
		ID        int64  `json:"id"`
		Type      string `json:"type"`
		Title     string `json:"title"`
		FirstName string `json:"first_name"`
		Username  string `json:"username"`
	} `json:"chat"`
	Date     int64    `json:"date"`
	EditDate int64    `json:"edit_date"`
	Text     string   `json:"text"`
	Caption  string   `json:"caption"`
	Reply    *message `json:"reply_to_message"`
	Voice    *file    `json:"voice"`
	Audio    *file    `json:"audio"`
	Video    *file    `json:"video"`
	Document *file    `json:"document"`
	Photo    []file   `json:"photo"`
}

type update struct {
	UpdateID      int64    `json:"update_id"`
	Message       *message `json:"message"`
	EditedMessage *message `json:"edited_message"`
}

// Run polls for messages until ctx ends. A failing poll is retried with
// backoff; a 409 (another poller on this token) is logged loudly, since two
// pollers steal each other's messages.
func (b *Bot) Run(ctx context.Context, r Receiver) {
	// A webhook set on the bot would make getUpdates fail.
	if err := b.call(ctx, "deleteWebhook", map[string]any{"drop_pending_updates": false}, nil); err != nil {
		b.Logger.Printf("telegram: deleteWebhook: %v", err)
	}
	backoff := time.Second
	for ctx.Err() == nil {
		err := b.pollOnce(ctx, r)
		if err == nil {
			backoff = time.Second
			continue
		}
		if ctx.Err() != nil {
			return
		}
		var ae *apiError
		wait := backoff
		switch {
		case errors.As(err, &ae) && ae.Code == http.StatusConflict:
			b.Logger.Printf("telegram: another program is polling this bot (%v); give each mahamantri its own bot token", err)
			wait = time.Minute
		case errors.As(err, &ae) && ae.RetryAfter > 0:
			wait = time.Duration(ae.RetryAfter) * time.Second
		default:
			b.Logger.Printf("telegram: polling: %v (retrying in %s)", err, backoff)
			backoff = min(2*backoff, time.Minute)
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return
		}
	}
}

func (b *Bot) pollOnce(ctx context.Context, r Receiver) error {
	offset, _ := strconv.ParseInt(r.Meta(offsetKey), 10, 64)
	var ups []update
	err := b.call(ctx, "getUpdates", map[string]any{
		"offset": offset, "timeout": b.poll(), "allowed_updates": []string{"message", "edited_message"},
	}, &ups)
	if err != nil || len(ups) == 0 {
		return err
	}
	var ins []bridge.Inbound
	next := offset
	for _, u := range ups {
		next = max(next, u.UpdateID+1)
		m, edited := u.Message, false
		if m == nil {
			m, edited = u.EditedMessage, true
		}
		if m == nil {
			continue
		}
		in, ok, why := b.inbound(m, edited)
		if !ok {
			b.Logger.Printf("telegram: update %d not taken - %s", u.UpdateID, why)
			continue
		}
		ins = append(ins, in)
	}
	// The messages and the new offset are stored together: after a crash
	// either both are kept or the updates are fetched again (and kept
	// once, by id).
	return r.ReceiveBatch(ins, offsetKey, strconv.FormatInt(next, 10))
}

// inbound turns a message into an Inbound, or says why it is not taken.
func (b *Bot) inbound(m *message, edited bool) (bridge.Inbound, bool, string) {
	switch {
	case m.From == nil || m.From.IsBot:
		return bridge.Inbound{}, false, "not from a person"
	case !b.TrustedUsers[m.From.ID]:
		return bridge.Inbound{}, false, fmt.Sprintf("from user %d, who is not trusted (telegram.trustedUserIDs)", m.From.ID)
	case m.Chat.Type != "private" && !b.Chats[m.Chat.ID]:
		return bridge.Inbound{}, false, fmt.Sprintf("in chat %d, which is not listed (telegram.chatIDs)", m.Chat.ID)
	}
	chat := strconv.FormatInt(m.Chat.ID, 10)
	ref := strconv.FormatInt(m.MessageID, 10)
	title := m.Chat.Title
	if title == "" {
		title = "Telegram chat with " + m.From.name()
	}
	in := bridge.Inbound{
		ID: "telegram:" + chat + ":" + ref, Conversation: "telegram:" + chat, Title: title,
		Thread: ref, Kind: "message", Author: m.From.name(), AuthorID: strconv.FormatInt(m.From.ID, 10),
		Trusted: true, Ref: ref,
	}
	body := m.Text
	if body == "" {
		body = m.Caption
	}
	if edited {
		in.ID += ":edit:" + strconv.FormatInt(m.EditDate, 10)
		in.Kind = "edit"
		body = "(edited an earlier message to:)\n" + body
	}
	if m.Reply != nil {
		in.InReplyTo = strconv.FormatInt(m.Reply.MessageID, 10)
		if q := m.Reply.Text; q != "" {
			if len(q) > 300 {
				q = q[:300] + "..."
			}
			body = "(replying to: " + strings.ReplaceAll(q, "\n", " ") + ")\n" + body
		}
	}
	add := func(kind string, f *file) {
		if f != nil {
			in.Attachments = append(in.Attachments, bridge.Attachment{Kind: kind, Ref: f.FileID, Name: f.FileName, Seconds: f.Duration})
		}
	}
	add("voice", m.Voice)
	add("audio", m.Audio)
	add("video", m.Video)
	add("document", m.Document)
	if len(m.Photo) > 0 {
		add("photo", &m.Photo[len(m.Photo)-1]) // the largest size
	}
	if body == "" && len(in.Attachments) == 0 {
		return bridge.Inbound{}, false, "nothing in it Samrajya reads (a sticker, a poll...)"
	}
	in.Body = body
	return in, true, ""
}

// Deliver sends body to the chat, as a reply to thread (a message id) if
// given, split into Telegram-sized messages. It returns the first one sent.
// A rate limit is waited out (briefly) rather than reported uncertain.
func (b *Bot) Deliver(ctx context.Context, conv, thread, body string) (bridge.Posted, error) {
	chat, ok := strings.CutPrefix(conv, "telegram:")
	if !ok || chat == "" {
		return bridge.Posted{}, fmt.Errorf("%q is not a Telegram conversation (telegram:<chat id>)", conv)
	}
	var first bridge.Posted
	for i, part := range split(body, maxText) {
		params := map[string]any{"chat_id": chat, "text": part, "link_preview_options": map[string]any{"is_disabled": true}}
		if thread != "" && i == 0 {
			if id, err := strconv.ParseInt(thread, 10, 64); err == nil {
				params["reply_parameters"] = map[string]any{"message_id": id, "allow_sending_without_reply": true}
			}
		}
		var sent struct {
			MessageID int64 `json:"message_id"`
		}
		err := b.call(ctx, "sendMessage", params, &sent)
		var ae *apiError
		if errors.As(err, &ae) && ae.RetryAfter > 0 && ae.RetryAfter <= 30 {
			select {
			case <-time.After(time.Duration(ae.RetryAfter) * time.Second):
			case <-ctx.Done():
				return first, ctx.Err()
			}
			err = b.call(ctx, "sendMessage", params, &sent)
		}
		if err != nil {
			if i > 0 {
				return first, fmt.Errorf("sent %d of the reply's parts, then: %w", i, err)
			}
			return first, err
		}
		if i == 0 {
			id := strconv.FormatInt(sent.MessageID, 10)
			first = bridge.Posted{ID: id, Thread: id}
		}
	}
	return first, nil
}

// split cuts s into pieces of at most n bytes, at a line break or space
// where it can, never inside a character.
func split(s string, n int) []string {
	var out []string
	for len(s) > n {
		cut := n
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		if i := strings.LastIndex(s[:cut], "\n"); i > n/2 {
			cut = i + 1
		} else if i := strings.LastIndex(s[:cut], " "); i > n/2 {
			cut = i + 1
		}
		out = append(out, s[:cut])
		s = s[cut:]
	}
	if s != "" || len(out) == 0 {
		out = append(out, s)
	}
	return out
}

// Received reacts to the message.
func (b *Bot) Received(ctx context.Context, e bridge.Event) error {
	if b.ReceivedEmoji == "" || e.MessageRef() == "" {
		return nil
	}
	chat, _ := strings.CutPrefix(e.Conversation, "telegram:")
	id, err := strconv.ParseInt(e.MessageRef(), 10, 64)
	if err != nil {
		return nil
	}
	return b.call(ctx, "setMessageReaction", map[string]any{
		"chat_id": chat, "message_id": id, "reaction": []map[string]string{{"type": "emoji", "emoji": b.ReceivedEmoji}},
	}, nil)
}

// Notify replies to the message.
func (b *Bot) Notify(ctx context.Context, e bridge.Event, text string) error {
	_, err := b.Deliver(ctx, e.Conversation, e.MessageRef(), text)
	return err
}

// Fetch downloads a file a message carried (its file id), for an agent to
// read.
func (b *Bot) Fetch(ctx context.Context, ref string) (string, []byte, error) {
	var f struct {
		FilePath string `json:"file_path"`
	}
	if err := b.call(ctx, "getFile", map[string]any{"file_id": ref}, &f); err != nil {
		return "", nil, err
	}
	base := b.URL
	if base == "" {
		base = apiURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/file/bot"+b.Token+"/"+f.FilePath, nil)
	if err != nil {
		return "", nil, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return "", nil, fmt.Errorf("telegram file: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("telegram file: %s", res.Status)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, 20<<20))
	name := f.FilePath
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return name, data, err
}
