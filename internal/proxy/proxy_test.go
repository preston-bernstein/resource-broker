package proxy

import (
	"bufio"
	"context"
	"errors"
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

// TestEmbedRewritesEmbeddingsPath checks the embed lane maps the OpenAI
// /embeddings (and /v1/embeddings) route to Infinity's /embeddings_image while
// passing the body through and leaving other paths (e.g. /health) untouched.
func TestEmbedRewritesEmbeddingsPath(t *testing.T) {
	var gotPath, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	front := httptest.NewServer(NewEmbed(target))
	defer front.Close()

	cases := []struct{ in, want string }{
		{"/embeddings", "/embeddings_image"},
		{"/v1/embeddings", "/embeddings_image"},
		{"/health", "/health"},
		{"/models", "/models"},
	}
	for _, c := range cases {
		resp, err := http.Post(front.URL+c.in, "application/json", strings.NewReader(`{"input":["x"]}`))
		if err != nil {
			t.Fatalf("post %s: %v", c.in, err)
		}
		resp.Body.Close()
		if gotPath != c.want {
			t.Errorf("%s -> upstream path %q, want %q", c.in, gotPath, c.want)
		}
		if c.in == "/embeddings" && gotBody != `{"input":["x"]}` {
			t.Errorf("body not passed through: %q", gotBody)
		}
	}
}

// fakeRoundTripper fails with err for the first failUntil calls (per the
// shared counter), then delegates to ok. Also records each request body it
// saw, so tests can confirm a retried request replays the same body.
type fakeRoundTripper struct {
	failUntil int
	err       error
	calls     int
	bodies    []string
}

func (f *fakeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	f.calls++
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		f.bodies = append(f.bodies, string(b))
	} else {
		f.bodies = append(f.bodies, "")
	}
	if f.calls <= f.failUntil {
		return nil, f.err
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: make(http.Header)}, nil
}

func TestRetryTransportRetriesOnConnError(t *testing.T) {
	fake := &fakeRoundTripper{failUntil: 2, err: errors.New("server disconnected without sending a response")}
	rt := &retryTransport{base: fake, retries: 2, backoff: time.Millisecond}

	req := httptest.NewRequest(http.MethodPost, "/api/embed", strings.NewReader(`{"input":["a","b"]}`))
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip returned error after successful retry: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if fake.calls != 3 {
		t.Errorf("calls = %d, want 3 (2 failures + 1 success)", fake.calls)
	}
	for i, b := range fake.bodies {
		if b != `{"input":["a","b"]}` {
			t.Errorf("attempt %d body = %q, want original body replayed", i, b)
		}
	}
}

func TestRetryTransportGivesUpAfterMaxRetries(t *testing.T) {
	wantErr := errors.New("server disconnected without sending a response")
	fake := &fakeRoundTripper{failUntil: 99, err: wantErr}
	rt := &retryTransport{base: fake, retries: 2, backoff: time.Millisecond}

	req := httptest.NewRequest(http.MethodPost, "/api/embed", strings.NewReader(`{}`))
	_, err := rt.RoundTrip(req)
	if err == nil {
		t.Fatal("expected error after exhausting retries, got nil")
	}
	if fake.calls != 3 {
		t.Errorf("calls = %d, want 3 (1 initial + 2 retries)", fake.calls)
	}
}

func TestRetryTransportDoesNotRetryNonConnError(t *testing.T) {
	fake := &fakeRoundTripper{failUntil: 99, err: errors.New("some unrelated application error")}
	rt := &retryTransport{base: fake, retries: 2, backoff: time.Millisecond}

	req := httptest.NewRequest(http.MethodPost, "/api/embed", strings.NewReader(`{}`))
	_, err := rt.RoundTrip(req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if fake.calls != 1 {
		t.Errorf("calls = %d, want 1 (non-retryable error must not retry)", fake.calls)
	}
}

func TestRetryTransportDoesNotRetryAfterCancellation(t *testing.T) {
	fake := &fakeRoundTripper{failUntil: 99, err: errors.New("server disconnected without sending a response")}
	rt := &retryTransport{base: fake, retries: 2, backoff: time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/embed", strings.NewReader(`{}`)).WithContext(ctx)
	_, err := rt.RoundTrip(req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if fake.calls != 1 {
		t.Errorf("calls = %d, want 1 (cancelled context must not retry)", fake.calls)
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
