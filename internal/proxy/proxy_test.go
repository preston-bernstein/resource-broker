package proxy

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestStreamingNotBuffered deterministically proves the proxy relays each
// upstream write immediately instead of buffering the whole response. The
// upstream blocks before its second write until the client has read the first
// line; if the proxy buffered, the client could never read line 1 and the test
// would time out.
func TestStreamingNotBuffered(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream ResponseWriter is not a Flusher")
			return
		}
		io.WriteString(w, "line1\n")
		fl.Flush()
		<-release // do not produce line2 until the client has read line1
		io.WriteString(w, "line2\n")
		fl.Flush()
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	front := httptest.NewServer(New(target))
	defer front.Close()

	resp, err := http.Get(front.URL + "/api/generate")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	br := bufio.NewReader(resp.Body)
	got1, err := readLineWithin(br, 2*time.Second)
	if err != nil {
		t.Fatalf("reading line1 (proxy likely buffered): %v", err)
	}
	if strings.TrimSpace(got1) != "line1" {
		t.Fatalf("line1 = %q, want line1", got1)
	}

	close(release) // now allow line2
	got2, err := readLineWithin(br, 2*time.Second)
	if err != nil {
		t.Fatalf("reading line2: %v", err)
	}
	if strings.TrimSpace(got2) != "line2" {
		t.Fatalf("line2 = %q, want line2", got2)
	}
}

// TestForwardsRequest checks method, path, query and body reach the upstream.
func TestForwardsRequest(t *testing.T) {
	var gotMethod, gotPath, gotQuery, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	front := httptest.NewServer(New(target))
	defer front.Close()

	resp, err := http.Post(front.URL+"/api/generate?stream=true", "application/json", strings.NewReader(`{"model":"x"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/generate" {
		t.Errorf("path = %q, want /api/generate", gotPath)
	}
	if gotQuery != "stream=true" {
		t.Errorf("query = %q, want stream=true", gotQuery)
	}
	if gotBody != `{"model":"x"}` {
		t.Errorf("body = %q", gotBody)
	}
}

func readLineWithin(br *bufio.Reader, d time.Duration) (string, error) {
	type res struct {
		s   string
		err error
	}
	ch := make(chan res, 1)
	go func() {
		s, err := br.ReadString('\n')
		ch <- res{s, err}
	}()
	select {
	case r := <-ch:
		return r.s, r.err
	case <-time.After(d):
		return "", io.EOF
	}
}
