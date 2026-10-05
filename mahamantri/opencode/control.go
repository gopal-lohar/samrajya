package opencode

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// Interrupt stops whatever the session is doing right now - a running tool
// call included - and leaves it idle with its context intact. Verified live:
// a running `sleep` was killed within a second, and a prompt sent right
// after was answered in seconds. Messages alone cannot do this: `steer`
// delivery still waits for the tool call to finish.
func (c *Client) Interrupt(ctx context.Context, sessionID string) error {
	return c.doJSON(ctx, http.MethodPost, "/api/session/"+sessionID+"/interrupt", nil, nil)
}

// Proxy returns a handler that forwards requests to the opencode server with
// this client's current credentials added, after removing stripPrefix from
// the path. Whoever calls it gets the whole opencode API - every operation,
// not a curated subset - without ever holding the password. Streaming
// responses (the event feed) are flushed as they arrive.
func (c *Client) Proxy(stripPrefix string) http.Handler {
	target, err := url.Parse(c.BaseURL)
	if err != nil {
		panic(fmt.Sprintf("opencode: bad base URL %q: %v", c.BaseURL, err))
	}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, stripPrefix)
			pr.Out.URL.RawPath = ""
			pr.Out.Header.Del("Authorization")
			pr.Out.SetBasicAuth("opencode", c.password())
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "opencode server unreachable: "+err.Error(), http.StatusBadGateway)
		},
	}
}
