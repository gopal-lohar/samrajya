package opencode

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestInterruptPostsToTheInterruptEndpoint(t *testing.T) {
	var got string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { got = r.Method + " " + r.URL.Path })
	if err := c.Interrupt(context.Background(), "ses_1"); err != nil {
		t.Fatal(err)
	}
	if got != "POST /api/session/ses_1/interrupt" {
		t.Errorf("request = %q", got)
	}
}

// The gateway is the whole point: callers get the entire opencode API and
// never see the password, and a password change applies without restarting.
func TestProxyAddsCredentialsAndStripsThePrefix(t *testing.T) {
	var gotPath, gotUser, gotPass, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotUser, gotPass, _ = r.BasicAuth()
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Write([]byte("upstream-ok"))
	}))
	defer upstream.Close()
	c := New(upstream.URL, "real-pw")
	gw := httptest.NewServer(c.Proxy("/opencode"))
	defer gw.Close()

	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/opencode/api/session/ses_1/fork", strings.NewReader(`{"a":1}`))
	req.SetBasicAuth("caller", "caller-supplied-junk") // must be replaced, not forwarded
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "upstream-ok" || gotPath != "/api/session/ses_1/fork" || gotBody != `{"a":1}` {
		t.Errorf("body=%q path=%q upstreamBody=%q", body, gotPath, gotBody)
	}
	if gotUser != "opencode" || gotPass != "real-pw" {
		t.Errorf("upstream saw %q/%q, want opencode/real-pw", gotUser, gotPass)
	}

	c.SetPassword("rotated-pw")
	resp, _ = http.Get(gw.URL + "/opencode/api/model")
	resp.Body.Close()
	if gotPass != "rotated-pw" {
		t.Errorf("after SetPassword upstream saw %q", gotPass)
	}
}

func TestProxyReportsAnUnreachableServerClearly(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	url := upstream.URL
	upstream.Close()
	gw := httptest.NewServer(New(url, "pw").Proxy("/opencode"))
	defer gw.Close()
	resp, err := http.Get(gw.URL + "/opencode/api/model")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(b), "unreachable") {
		t.Errorf("status=%d body=%q", resp.StatusCode, b)
	}
}

func TestProxyStreamsEventsAsTheyArrive(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: first\n\n"))
		w.(http.Flusher).Flush()
		<-release
		w.Write([]byte("data: second\n\n"))
	}))
	defer upstream.Close()
	defer close(release)
	gw := httptest.NewServer(New(upstream.URL, "pw").Proxy("/opencode"))
	defer gw.Close()

	resp, err := http.Get(gw.URL + "/opencode/api/event")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 64)
	got := make(chan string, 1)
	go func() { n, _ := resp.Body.Read(buf); got <- string(buf[:n]) }()
	select {
	case s := <-got:
		if !strings.Contains(s, "first") {
			t.Errorf("first chunk = %q", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the first event was buffered instead of streamed")
	}
}
