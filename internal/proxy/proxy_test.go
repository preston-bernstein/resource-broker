package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestSharedTransportConfig pins the shared Transport's tuned constants — the
// 60s IdleConnTimeout (proxy.go's own comment explains why this must not
// silently drift back to http.DefaultTransport's 90s) and the 2-retries/
// 500ms-backoff connection-retry policy — since no other test exercises the
// package-level var's literal values directly.
func TestSharedTransportConfig(t *testing.T) {
	rt, ok := Transport.(*retryTransport)
	if !ok {
		t.Fatalf("Transport = %T, want *retryTransport", Transport)
	}
	if rt.retries != 2 {
		t.Errorf("retries = %d, want 2", rt.retries)
	}
	if rt.backoff != 500*time.Millisecond {
		t.Errorf("backoff = %v, want 500ms", rt.backoff)
	}
	base, ok := rt.base.(*http.Transport)
	if !ok {
		t.Fatalf("base = %T, want *http.Transport", rt.base)
	}
	if base.IdleConnTimeout != 60*time.Second {
		t.Errorf("IdleConnTimeout = %v, want 60s", base.IdleConnTimeout)
	}
}

// TestNewSetsNoBufferFlushInterval proves New()'s ReverseProxy sets
// FlushInterval: -1 (flush every write immediately) — the specific setting
// TestStreamingNotBuffered proves the *effect* of end-to-end, but nothing
// asserted the literal field value itself.
func TestNewSetsNoBufferFlushInterval(t *testing.T) {
	target, _ := url.Parse("http://127.0.0.1:0")
	rp, ok := New(target).(*httputil.ReverseProxy)
	if !ok {
		t.Fatalf("New() = %T, want *httputil.ReverseProxy", New(target))
	}
	if rp.FlushInterval != -1 {
		t.Errorf("FlushInterval = %v, want -1", rp.FlushInterval)
	}
}

// TestNewEmbedSetsNoBufferFlushInterval is NewEmbed's analog to
// TestNewSetsNoBufferFlushInterval — same field, separate ReverseProxy value.
func TestNewEmbedSetsNoBufferFlushInterval(t *testing.T) {
	target, _ := url.Parse("http://127.0.0.1:0")
	rp, ok := NewEmbed(target).(*httputil.ReverseProxy)
	if !ok {
		t.Fatalf("NewEmbed() = %T, want *httputil.ReverseProxy", NewEmbed(target))
	}
	if rp.FlushInterval != -1 {
		t.Errorf("FlushInterval = %v, want -1", rp.FlushInterval)
	}
}

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
		// Bounded, not a bare <-release: if the test fails/returns before
		// close(release) (e.g. line1 never arrives), an unbounded read here
		// blocks this handler goroutine forever, and the deferred
		// httptest.Server.Close() (which waits for in-flight connections)
		// hangs the entire test binary until the outer `go test -timeout`
		// kills it — a real robustness gap, not just a slow test.
		select {
		case <-release: // do not produce line2 until the client has read line1
		case <-time.After(2 * time.Second):
			return
		}
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

// TestErrorHandlerDeadlineExceededMatches503Shape pins ADR-0013's client-
// facing contract: when a Gate upstreamTimeout fires (context.DeadlineExceeded,
// currently only set on the embed lane), the client must see the same 503 +
// Retry-After + X-Broker-Status: deferred shape Gate's own deferRequest uses
// for every other deferral ("GPU busy: wait budget exceeded", "yielding
// GPU") — not an opaque 502, which would read as a genuine upstream fault
// rather than a broker-side bound.
func TestErrorHandlerDeadlineExceededMatches503Shape(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/embeddings", nil)

	errorHandler(rec, req, context.DeadlineExceeded)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if got := rec.Header().Get("X-Broker-Status"); got != "deferred" {
		t.Errorf("X-Broker-Status = %q, want %q", got, "deferred")
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("missing Retry-After header")
	}
}

// TestErrorHandlerCanceledMatches503Shape pins the pre-existing yield/
// disconnect path still carries the same shape after adding the
// DeadlineExceeded branch alongside it.
func TestErrorHandlerCanceledMatches503Shape(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/generate", nil)

	errorHandler(rec, req, context.Canceled)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if got := rec.Header().Get("X-Broker-Status"); got != "deferred" {
		t.Errorf("X-Broker-Status = %q, want %q", got, "deferred")
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("missing Retry-After header")
	}
}

// TestErrorHandlerOtherErrorStays502 pins that a genuine upstream fault
// (connection refused, DNS failure, etc.) still surfaces as 502, not the
// broker-deferral 503 shape — that distinction is what lets an operator
// tell "the broker deferred this on purpose" from "Infinity/Ollama is
// actually down" in the response status alone.
func TestErrorHandlerOtherErrorStays502(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/generate", nil)

	errorHandler(rec, req, errors.New("dial tcp: connection refused"))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
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
