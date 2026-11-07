package linear

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gopal-lohar/samrajya/mahamantri/bridge"
)

// Upload puts a file in Linear's storage and returns its asset URL, which a
// comment can link or show.
func (a *API) Upload(ctx context.Context, f bridge.File) (string, error) {
	ctype := mime.TypeByExtension(strings.ToLower(filepath.Ext(f.Name)))
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	var out struct {
		FileUpload struct {
			Success    bool `json:"success"`
			UploadFile struct {
				UploadURL string `json:"uploadUrl"`
				AssetURL  string `json:"assetUrl"`
				Headers   []struct {
					Key   string `json:"key"`
					Value string `json:"value"`
				} `json:"headers"`
			} `json:"uploadFile"`
		} `json:"fileUpload"`
	}
	err := a.do(ctx, `mutation($contentType: String!, $filename: String!, $size: Int!) { fileUpload(contentType: $contentType, filename: $filename, size: $size) { success uploadFile { uploadUrl assetUrl headers { key value } } } }`,
		map[string]any{"contentType": ctype, "filename": f.Name, "size": f.Size}, &out)
	if err == nil && (!out.FileUpload.Success || out.FileUpload.UploadFile.UploadURL == "") {
		err = errors.New("linear api: no upload URL")
	}
	if err != nil {
		return "", err
	}
	up := out.FileUpload.UploadFile
	file, err := os.Open(f.Path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, up.UploadURL, file)
	if err != nil {
		return "", err
	}
	req.ContentLength = f.Size
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("Cache-Control", "public, max-age=31536000")
	for _, h := range up.Headers {
		req.Header.Set(h.Key, h.Value)
	}
	res, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return "", fmt.Errorf("uploading %s: %w", f.Name, err)
	}
	res.Body.Close()
	if res.StatusCode/100 != 2 {
		return "", fmt.Errorf("uploading %s: %s", f.Name, res.Status)
	}
	return up.AssetURL, nil
}

// DeliverFiles uploads the files to Linear and posts the comment with them
// linked: images shown, other files as links.
func (s *Surface) DeliverFiles(ctx context.Context, conv, thread, body string, files []bridge.File) (bridge.Posted, error) {
	api, err := s.api()
	if err != nil {
		return bridge.Posted{}, err
	}
	var b strings.Builder
	b.WriteString(strings.TrimSpace(body))
	for _, f := range files {
		asset, err := api.Upload(ctx, f)
		if err != nil {
			return bridge.Posted{}, err // nothing posted yet
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		name := strings.NewReplacer("[", "(", "]", ")").Replace(f.Name)
		if strings.HasPrefix(mime.TypeByExtension(strings.ToLower(filepath.Ext(f.Name))), "image/") {
			fmt.Fprintf(&b, "![%s](%s)", name, asset)
		} else {
			fmt.Fprintf(&b, "[%s](%s)", name, asset)
		}
	}
	return s.Deliver(ctx, conv, thread, b.String())
}
