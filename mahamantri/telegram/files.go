package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gopal-lohar/samrajya/mahamantri/bridge"
)

// Telegram's limits for a caption and for a photo; a bigger image goes as a
// document.
const (
	maxCaption = 1024
	maxPhoto   = 10 << 20
)

var photoExt = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".webp": true}

// DeliverFiles sends files to the chat: images as photos, anything else as
// documents, each a reply to thread. A short body goes as the first file's
// caption; a longer one is sent first as a message. It returns the first
// message sent.
func (b *Bot) DeliverFiles(ctx context.Context, conv, thread, body string, files []bridge.File) (bridge.Posted, error) {
	chat, ok := strings.CutPrefix(conv, "telegram:")
	if !ok || chat == "" {
		return bridge.Posted{}, fmt.Errorf("%q is not a Telegram conversation (telegram:<chat id>)", conv)
	}
	var first bridge.Posted
	caption := ""
	body = strings.TrimSpace(body)
	if len(body) <= maxCaption {
		caption = body
	} else {
		p, err := b.Deliver(ctx, conv, thread, body)
		if err != nil {
			return first, err
		}
		first = p
	}
	for i, f := range files {
		method, field := "sendDocument", "document"
		if photoExt[strings.ToLower(filepath.Ext(f.Name))] && f.Size <= maxPhoto {
			method, field = "sendPhoto", "photo"
		}
		fields := map[string]string{"chat_id": chat}
		if i == 0 && caption != "" {
			fields["caption"] = caption
		}
		if id, err := strconv.ParseInt(thread, 10, 64); err == nil {
			rp, _ := json.Marshal(map[string]any{"message_id": id, "allow_sending_without_reply": true})
			fields["reply_parameters"] = string(rp)
		}
		var sent struct {
			MessageID int64 `json:"message_id"`
		}
		err := b.upload(ctx, method, fields, field, f, &sent)
		var ae *apiError
		if errors.As(err, &ae) && ae.RetryAfter > 0 && ae.RetryAfter <= 30 {
			select {
			case <-time.After(time.Duration(ae.RetryAfter) * time.Second):
			case <-ctx.Done():
				return first, ctx.Err()
			}
			err = b.upload(ctx, method, fields, field, f, &sent)
		}
		if err != nil {
			if first.ID != "" || i > 0 {
				return first, fmt.Errorf("sent part of the reply, then %s: %w", f.Name, err)
			}
			return first, fmt.Errorf("%s: %w", f.Name, err)
		}
		if first.ID == "" {
			id := strconv.FormatInt(sent.MessageID, 10)
			first = bridge.Posted{ID: id, Thread: id}
		}
	}
	return first, nil
}

// upload calls a Bot API method with fields and one file, as a multipart
// form.
func (b *Bot) upload(ctx context.Context, method string, fields map[string]string, field string, f bridge.File, out any) error {
	file, err := os.Open(f.Path)
	if err != nil {
		return err
	}
	defer file.Close()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	part, err := mw.CreateFormFile(field, f.Name)
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, file); err != nil {
		return err
	}
	mw.Close()
	base := b.URL
	if base == "" {
		base = apiURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/bot"+b.Token+"/"+method, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	res, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the URL holds the token
		}
		return fmt.Errorf("telegram %s: %w", method, err)
	}
	defer res.Body.Close()
	return decodeReply(method, res, out)
}
