package openaicompat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/preston-bernstein/ollama-resource-broker/internal/queue"
)

// readLineWithin mirrors internal/proxy/proxy_test.go's helper of the same
// name: it reads one line with a deadline so a buffered (non-streaming)
// implementation causes a test failure instead of a hang.
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

// TestServeChat_StreamingNotBuffered deterministically proves the handler
// relays each NDJSON line as it is produced, rather than buffering the whole
// response until the upstream finishes (AC-4). The mock upstream sends one
// SSE chunk, blocks until the client has read the corresponding NDJSON line,
// then sends the final chunk + [DONE]. If the handler buffered, the client
// could never read line 1 and the test would time out — mirrors
// internal/proxy/proxy_test.go's TestStreamingNotBuffered.
func TestServeChat_StreamingNotBuffered(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream ResponseWriter is not a Flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n")
		fl.Flush()
		// Bounded, not a bare <-release: close(release) below only runs if
		// the handler actually relays line 1 to the client. A handler bug
		// (or an injected mutant, under mutation testing) that fails before
		// that point must not hang this goroutine forever, or the deferred
		// upstream.Close() (which waits for in-flight handlers) hangs the
		// whole test binary until the outer `go test -timeout` kills it.
		select {
		case <-release:
		case <-time.After(500 * time.Millisecond):
		}
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"completion_tokens\":1}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer upstream.Close()

	base, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	front := httptest.NewServer(NewHandler(base, ""))
	defer front.Close()

	reqBody := `{"model":"test-model","prompt":"hi","stream":true}`
	resp, err := http.Post(front.URL+"/api/generate", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	br := bufio.NewReader(resp.Body)
	line1, err := readLineWithin(br, 2*time.Second)
	if err != nil {
		t.Fatalf("reading NDJSON line 1 (handler likely buffered): %v", err)
	}
	var chunk1 map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(line1)), &chunk1); err != nil {
		t.Fatalf("line1 %q is not valid JSON: %v", line1, err)
	}
	if chunk1["done"] != false {
		t.Fatalf("line1 done = %v, want false", chunk1["done"])
	}
	// line1 carries the actual delta text as it arrives, matching real
	// Ollama's own progressive-streaming convention — a client rendering
	// tokens live sees "Hello" as soon as this line flushes, not only once
	// the whole response is done.
	if chunk1["response"] != "Hello" {
		t.Fatalf("line1 response = %v, want %q (delta should be relayed live, not buffered to the final line)", chunk1["response"], "Hello")
	}

	close(release) // now allow the final chunk

	line2, err := readLineWithin(br, 2*time.Second)
	if err != nil {
		t.Fatalf("reading NDJSON line 2: %v", err)
	}
	var chunk2 map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(line2)), &chunk2); err != nil {
		t.Fatalf("line2 %q is not valid JSON: %v", line2, err)
	}
	if chunk2["done"] != true {
		t.Fatalf("line2 done = %v, want true", chunk2["done"])
	}
	// line2 (the final, done:true line) carries an empty response: "Hello"
	// was already relayed on line1, and repeating it here would double the
	// text for any client that concatenates every line's "response" field.
	if chunk2["response"] != "" {
		t.Fatalf("line2 response = %v, want %q (full text already sent on line1; final line must not repeat it)", chunk2["response"], "")
	}
}

// TestServeChat_ContextCancellationMidStream verifies that canceling the
// CLIENT's request context partway through a streaming /api/generate
// response — simulating a client disconnect — makes the handler exit
// cleanly: no panic, and no leaked goroutine. This is distinct from a
// Yield-triggered cancellation, which TestGateOpenAIHandler_* already covers
// via queue.Gate/ServeContext(); here nothing sits between the raw handler
// and the client, so it isolates the handler's own context handling. The
// mock upstream blocks after its first chunk (via a release channel) until
// the test has canceled and observed the torn-down connection, then is
// released so its own handler goroutine exits and httptest.Server.Close()
// (via defer) doesn't hang.
func TestServeChat_ContextCancellationMidStream(t *testing.T) {
	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(prevLogger)

	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream ResponseWriter is not a Flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n")
		fl.Flush()
		// Bounded, not a bare <-release: close(release) below only runs if
		// the test's main goroutine successfully reads line 1 first. A
		// handler bug (or an injected mutant) that fails before that point
		// must not hang this goroutine forever, or the deferred
		// upstream.Close() (which waits for in-flight handlers) hangs the
		// whole test binary until the outer `go test -timeout` kills it.
		select {
		case <-release:
		case <-time.After(500 * time.Millisecond):
		}
		// A write after the front-to-upstream connection was torn down by the
		// canceled context is expected to be a no-op/error, never a panic.
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"world\"}}]}\n\n")
		fl.Flush()
	}))
	// upstream.Close() and front.Close() are called explicitly below, BEFORE
	// the goroutine-count assertion, rather than only via defer (which would
	// run after the test function — and its assertion — has already
	// returned). See the comment at the goroutine-count check for why this
	// ordering matters. The defers remain as a safety net for early-return
	// paths (e.g. t.Fatalf above); httptest.Server.Close is idempotent, so
	// calling it again here is a no-op.
	defer upstream.Close()

	base, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	front := httptest.NewServer(NewHandler(base, ""))
	defer front.Close()

	baseGoroutines := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	reqBody := `{"model":"test-model","prompt":"hi","stream":true}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, front.URL+"/api/generate", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	br := bufio.NewReader(resp.Body)
	line1, err := readLineWithin(br, 2*time.Second)
	if err != nil {
		t.Fatalf("reading NDJSON line 1: %v", err)
	}
	var chunk1 map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(line1)), &chunk1); err != nil {
		t.Fatalf("line1 %q is not valid JSON: %v", line1, err)
	}
	if chunk1["done"] != false {
		t.Fatalf("line1 done = %v, want false", chunk1["done"])
	}

	cancel()       // simulate the client disconnecting mid-stream
	close(release) // let the mock upstream's handler goroutine exit

	// The connection must be torn down promptly (not hang) once the client's
	// context is canceled.
	if _, err := readLineWithin(br, 2*time.Second); err == nil {
		t.Fatal("expected the connection to be torn down after context cancellation, got a successful read")
	}

	// The handler must not have recovered from a panic — a clean early return
	// via ctx cancellation, not a bad-write panic.
	if strings.Contains(logBuf.String(), "panic in openai handler stream write") {
		t.Fatalf("handler recovered from a panic on context cancellation, want a clean exit: %q", logBuf.String())
	}

	// No goroutine leak: the handler's own goroutine unwinds promptly on ctx
	// cancellation, but the *connection* it used (front's outbound call to
	// upstream) is pooled and kept alive by the shared, package-level
	// proxy.Transport (see internal/proxy/proxy.go's IdleConnTimeout comment)
	// so its net/http.(*persistConn).readLoop goroutine can legitimately stay
	// parked in "IO wait" for up to that transport's IdleConnTimeout — this
	// is deliberate connection-pooling behavior, not a leak, and no amount of
	// polling runtime.NumGoroutine() alone will make it go away on any
	// bounded deadline. httptest.Server.Close() force-closes idle/new
	// server-side connections (see its doc comment) and blocks until
	// in-flight requests finish, which reliably tears down that pooled
	// connection from the server side and lets the client-side readLoop
	// observe EOF and exit — so, unlike the deferred Close() calls above
	// (which only run after this test function itself returns, too late to
	// affect this assertion), close both servers here, synchronously, before
	// checking the goroutine count.
	upstream.Close()
	front.Close()

	// A short poll remains as a bounded wait for any remaining, genuinely
	// asynchronous teardown (e.g. the runtime scheduling the now-unblocked
	// goroutines' exits) rather than a bare immediate check.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseGoroutines+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseGoroutines+2 {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Fatalf("goroutine count = %d, want <= %d (possible leak after context cancellation)\n%s", got, baseGoroutines+2, buf[:n])
	}
}

// TestServeChat_StreamTrueValidNDJSON verifies a stream:true request against
// a fast (non-blocking) mock upstream produces multiple NDJSON lines, each
// independently valid JSON.
func TestServeChat_StreamTrueValidNDJSON(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"completion_tokens\":2}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	base, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	front := httptest.NewServer(NewHandler(base, ""))
	defer front.Close()

	reqBody := `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}`
	resp, err := http.Post(front.URL+"/api/chat", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) < 2 {
		t.Fatalf("got %d NDJSON lines, want at least 2 (got %q)", len(lines), string(body))
	}
	var sawDoneTrue bool
	var concatenated strings.Builder
	for _, line := range lines {
		var chunk map[string]any
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			t.Fatalf("line %q is not valid JSON: %v", line, err)
		}
		if resp, ok := chunk["response"].(string); ok {
			concatenated.WriteString(resp)
		}
		if chunk["done"] == true {
			sawDoneTrue = true
		}
	}
	if !sawDoneTrue {
		t.Fatalf("no NDJSON line with done:true in %q", string(body))
	}
	// The mock upstream sends "Hel" then "lo" as two separate delta chunks
	// (matching real Ollama's per-chunk streaming convention): a client
	// that concatenates every line's "response" field, live as lines
	// arrive, must end up with the byte-correct full text.
	if got := concatenated.String(); got != "Hello" {
		t.Fatalf("concatenated response across all NDJSON lines = %q, want %q", got, "Hello")
	}
}

// TestServeChat_StreamFalse verifies stream:false produces a single buffered
// JSON object (not NDJSON) — AC-18.
func TestServeChat_StreamFalse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"completion_tokens\":1}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	base, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	front := httptest.NewServer(NewHandler(base, ""))
	defer front.Close()

	reqBody := `{"model":"test-model","prompt":"hi","stream":false}`
	resp, err := http.Post(front.URL+"/api/generate", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	trimmed := strings.TrimSpace(string(body))
	if strings.Contains(trimmed, "\n") {
		t.Fatalf("stream:false body has more than one line (NDJSON leaked through): %q", trimmed)
	}
	var chunk map[string]any
	if err := json.Unmarshal([]byte(trimmed), &chunk); err != nil {
		t.Fatalf("body %q is not valid JSON: %v", trimmed, err)
	}
	if chunk["done"] != true {
		t.Fatalf("done = %v, want true", chunk["done"])
	}
	if chunk["response"] != "Hello" {
		t.Fatalf("response = %v, want %q", chunk["response"], "Hello")
	}
	if chunk["eval_count"] != float64(1) {
		t.Fatalf("eval_count = %v, want 1", chunk["eval_count"])
	}
}

// TestServeChat_StreamDefaultTrue verifies an absent "stream" field defaults
// to true (streamed NDJSON), matching Ollama's own default (FR-25).
func TestServeChat_StreamDefaultTrue(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"completion_tokens\":1}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	base, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	front := httptest.NewServer(NewHandler(base, ""))
	defer front.Close()

	reqBody := `{"model":"test-model","prompt":"hi"}`
	resp, err := http.Post(front.URL+"/api/generate", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("Content-Type = %q, want application/x-ndjson (stream should default to true)", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) < 2 {
		t.Fatalf("got %d NDJSON lines, want at least 2 when stream is omitted", len(lines))
	}
}

// TestServeEmbed verifies /api/embed reaches the embed translation and
// returns an Ollama-shaped embeddings response (AC-5).
func TestServeEmbed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("upstream path = %q, want /v1/embeddings", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"embed-model","data":[{"embedding":[0.1,0.2],"index":0},{"embedding":[0.3,0.4],"index":1}]}`)
	}))
	defer upstream.Close()

	base, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	front := httptest.NewServer(NewHandler(base, ""))
	defer front.Close()

	reqBody := `{"model":"embed-model","input":["a","b"]}`
	resp, err := http.Post(front.URL+"/api/embed", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 (body: %s)", resp.StatusCode, body)
	}

	var got EmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Model != "embed-model" {
		t.Fatalf("model = %q, want embed-model", got.Model)
	}
	if len(got.Embeddings) != 2 || got.Embeddings[0][0] != 0.1 || got.Embeddings[1][0] != 0.3 {
		t.Fatalf("embeddings = %v, want order-preserved [[0.1,0.2],[0.3,0.4]]", got.Embeddings)
	}
}

// TestServeHTTP_UnknownPath404 verifies any path other than /api/generate,
// /api/chat, /api/embed receives 404 with a JSON error body (FR-28, AC-21).
func TestServeHTTP_UnknownPath404(t *testing.T) {
	base, err := url.Parse("http://127.0.0.1:1") // never dialed — 404 short-circuits first
	if err != nil {
		t.Fatalf("parse base URL: %v", err)
	}
	front := httptest.NewServer(NewHandler(base, ""))
	defer front.Close()

	resp, err := http.Get(front.URL + "/api/tags")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if _, ok := body["error"]; !ok {
		t.Fatalf("body %v has no \"error\" field", body)
	}
}

// TestServeChat_MalformedJSONBody400 verifies a request body that fails to
// decode as JSON is rejected with 400 and a "decode request: ..." message,
// rather than reaching the upstream. No prior test ever posted a malformed
// body to /api/generate or /api/chat — every fixture was valid JSON —
// leaving handler.go's `if err := json.NewDecoder(r.Body).Decode(&req);
// err != nil { writeJSONError(...) }` decode-error branch unexercised.
func TestServeChat_MalformedJSONBody400(t *testing.T) {
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
	}))
	defer upstream.Close()

	base, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	front := httptest.NewServer(NewHandler(base, ""))
	defer front.Close()

	resp, err := http.Post(front.URL+"/api/generate", "application/json", strings.NewReader(`{not valid json`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	errMsg, ok := body["error"].(string)
	if !ok || !strings.Contains(errMsg, "decode request") {
		t.Fatalf("error body = %v, want a message containing \"decode request\"", body)
	}
	if upstreamCalls != 0 {
		t.Fatalf("upstream was called %d times, want 0 (a malformed body must never be forwarded)", upstreamCalls)
	}
}

// TestServeChat_AuthorizationHeaderSetWhenAPIKeyConfigured verifies the
// handler's outbound chat request (both stream:true and stream:false, since
// both funnel through Client.openChatStream) carries "Authorization: Bearer
// <key>" when NewHandler was configured with a non-empty apiKey. Every
// other handler_test.go test uses NewHandler(base, "") — zero prior
// coverage exercised a non-empty apiKey at the handler level (the
// equivalent check on Client.Generate's own request path is covered
// separately by TestGenerate_AuthorizationHeader), leaving
// Client.openChatStream's own `if c.apiKey != "" { ... }` completely
// unexercised.
func TestServeChat_AuthorizationHeaderSetWhenAPIKeyConfigured(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	base, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	front := httptest.NewServer(NewHandler(base, "secret-key"))
	defer front.Close()

	reqBody := `{"model":"test-model","prompt":"hi","stream":false}`
	resp, err := http.Post(front.URL+"/api/generate", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if want := "Bearer secret-key"; gotAuth != want {
		t.Fatalf("Authorization = %q, want %q", gotAuth, want)
	}
}

// TestServeChat_OptionsFilterReservedKeys verifies a /api/chat request's
// "options" object can't override the fields the handler itself controls
// (model, messages, stream, stream_options) when relayed to the upstream,
// while a non-reserved option key passes through. No prior test ever sent
// an "options" field at all, leaving handler.go's openChatStream reserved-
// key filter (`if k == "model" || k == "messages" || k == "stream" || k ==
// "stream_options" { continue }`) completely unexercised at the handler
// level (the equivalent check on Client.Generate's own options is covered
// separately by TestGenerate_OptionsFilterReservedKeys).
func TestServeChat_OptionsFilterReservedKeys(t *testing.T) {
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("mock server: decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	base, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	front := httptest.NewServer(NewHandler(base, ""))
	defer front.Close()

	reqBody := `{"model":"real-model","prompt":"hi","stream":false,"options":{` +
		`"model":"SHOULD-BE-IGNORED","messages":"SHOULD-BE-IGNORED",` +
		`"stream":"SHOULD-BE-IGNORED","stream_options":"SHOULD-BE-IGNORED",` +
		`"temperature":0.7}}`
	resp, err := http.Post(front.URL+"/api/generate", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if gotBody["model"] != "real-model" {
		t.Fatalf("model = %v, want %q (an \"options\" entry must not override it)", gotBody["model"], "real-model")
	}
	if gotBody["stream"] != true {
		t.Fatalf("stream = %v, want true (an \"options\" entry must not override it)", gotBody["stream"])
	}
	if _, ok := gotBody["stream_options"].(map[string]any); !ok {
		t.Fatalf("stream_options = %v, want a map (an \"options\" entry must not override it)", gotBody["stream_options"])
	}
	messages, ok := gotBody["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("messages = %v, want a single-element array (an \"options\" entry must not override it)", gotBody["messages"])
	}
	if got := gotBody["temperature"]; got != 0.7 {
		t.Fatalf("temperature = %v, want 0.7 (a non-reserved option must pass through)", got)
	}
}

// TestServeChat_ImagesStringField verifies the unsupported-"images"-field
// check handles the string form of isEmptyFieldValue correctly: an empty
// string is treated as absent (request succeeds), a non-empty string is
// treated as present (request rejected) — exactly like the array form
// already covered for empty/non-empty. No prior test ever sent "images" as
// a JSON string (only arrays), leaving isEmptyFieldValue's `case string:
// return t == ""` branch completely unexercised.
func TestServeChat_ImagesStringField(t *testing.T) {
	t.Run("empty string accepted", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
			io.WriteString(w, "data: [DONE]\n\n")
		}))
		defer upstream.Close()

		base, err := url.Parse(upstream.URL)
		if err != nil {
			t.Fatalf("parse upstream URL: %v", err)
		}
		front := httptest.NewServer(NewHandler(base, ""))
		defer front.Close()

		reqBody := `{"model":"test-model","messages":[{"role":"user","content":"hi","images":""}],"stream":false}`
		resp, err := http.Post(front.URL+"/api/chat", "application/json", strings.NewReader(reqBody))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 200 (an empty string \"images\" must be treated as absent, body: %s)", resp.StatusCode, body)
		}
	})

	t.Run("non-empty string rejected", func(t *testing.T) {
		upstreamCalls := 0
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upstreamCalls++
		}))
		defer upstream.Close()

		base, err := url.Parse(upstream.URL)
		if err != nil {
			t.Fatalf("parse upstream URL: %v", err)
		}
		front := httptest.NewServer(NewHandler(base, ""))
		defer front.Close()

		reqBody := `{"model":"test-model","messages":[{"role":"user","content":"hi","images":"base64blob"}],"stream":false}`
		resp, err := http.Post(front.URL+"/api/chat", "application/json", strings.NewReader(reqBody))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (a non-empty string \"images\" must be rejected)", resp.StatusCode)
		}
		if upstreamCalls != 0 {
			t.Fatalf("upstream was called %d times, want 0", upstreamCalls)
		}
	})
}

// TestServeChat_PreResponseUpstream500_Returns502 verifies a pre-response
// failure (the upstream returns a non-2xx status before any streaming
// begins) is handled by proxy.WriteUpstreamError, and — since a plain
// upstream 500 doesn't match context.Canceled/DeadlineExceeded, so
// WriteUpstreamError declines it (returns false) — that the handler writes
// its own 502 JSON fallback rather than silently returning with nothing
// written (which net/http would turn into an empty 200 OK).
func TestServeChat_PreResponseUpstream500_Returns502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	base, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	front := httptest.NewServer(NewHandler(base, ""))
	defer front.Close()

	reqBody := `{"model":"test-model","prompt":"hi","stream":true}`
	resp, err := http.Post(front.URL+"/api/generate", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if _, ok := body["error"]; !ok {
		t.Fatalf("body %v has no \"error\" field", body)
	}
}

// TestServeChat_MidStreamError_FinalNDJSONErrorLine verifies that once NDJSON
// chunks have already been flushed to the client for a stream:true request,
// an in-band upstream failure (here, a malformed SSE data event) is surfaced
// as one final Ollama-shaped NDJSON line carrying an "error" field — never a
// second HTTP status — mirroring ollama.Client.Generate's chunk.Error
// convention (plan.md's "Mid-stream error handling").
func TestServeChat_MidStreamError_FinalNDJSONErrorLine(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n")
		io.WriteString(w, "data: not-valid-json\n\n")
	}))
	defer upstream.Close()

	base, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	front := httptest.NewServer(NewHandler(base, ""))
	defer front.Close()

	reqBody := `{"model":"test-model","prompt":"hi","stream":true}`
	resp, err := http.Post(front.URL+"/api/generate", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a mid-stream failure can't change an already-committed status)", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d NDJSON lines, want 3 (2 valid + 1 final error line) (body: %q)", len(lines), string(body))
	}
	for i := 0; i < 2; i++ {
		var chunk map[string]any
		if err := json.Unmarshal([]byte(lines[i]), &chunk); err != nil {
			t.Fatalf("line %d %q is not valid JSON: %v", i, lines[i], err)
		}
		if chunk["done"] != false {
			t.Fatalf("line %d done = %v, want false", i, chunk["done"])
		}
		if _, ok := chunk["error"]; ok {
			t.Fatalf("line %d unexpectedly has an \"error\" field: %v", i, chunk)
		}
	}
	var last map[string]any
	if err := json.Unmarshal([]byte(lines[2]), &last); err != nil {
		t.Fatalf("final line %q is not valid JSON: %v", lines[2], err)
	}
	errMsg, ok := last["error"]
	if !ok {
		t.Fatalf("final line %v has no \"error\" field", last)
	}
	if s, ok := errMsg.(string); !ok || s == "" {
		t.Fatalf("final line \"error\" field = %v, want a non-empty string", errMsg)
	}
}

// TestServeChat_UnsupportedImagesField400 verifies a /api/chat request whose
// message includes a non-empty "images" field is rejected with HTTP 400 and
// a descriptive JSON error body naming the field, and — critically — that no
// request is ever forwarded to the upstream (FR-27, AC-20). The mock
// upstream increments a call counter so the test can assert it was never hit.
func TestServeChat_UnsupportedImagesField400(t *testing.T) {
	var upstreamCalls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	base, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	front := httptest.NewServer(NewHandler(base, ""))
	defer front.Close()

	reqBody := `{"model":"test-model","messages":[{"role":"user","content":"describe this","images":["base64data"]}],"stream":false}`
	resp, err := http.Post(front.URL+"/api/chat", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	errMsg, ok := body["error"].(string)
	if !ok || !strings.Contains(errMsg, "images") {
		t.Fatalf("error body = %v, want a descriptive message naming \"images\"", body)
	}
	if upstreamCalls != 0 {
		t.Fatalf("upstream was called %d times, want 0 (request must never be forwarded)", upstreamCalls)
	}
}

// TestServeChat_NoUnsupportedFields_StillSucceeds is a regression check
// verifying a valid /api/chat request with no unsupported fields (an empty
// "images" array, which some Ollama clients send even absent an actual
// image) still succeeds normally.
func TestServeChat_NoUnsupportedFields_StillSucceeds(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"completion_tokens\":1}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	base, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	front := httptest.NewServer(NewHandler(base, ""))
	defer front.Close()

	reqBody := `{"model":"test-model","messages":[{"role":"user","content":"hi","images":[]}],"stream":false}`
	resp, err := http.Post(front.URL+"/api/chat", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 (body: %s)", resp.StatusCode, body)
	}
	var chunk map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&chunk); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if chunk["response"] != "Hi" {
		t.Fatalf("response = %v, want %q", chunk["response"], "Hi")
	}
}

// TestServeGenerate_ContextFieldAccepted verifies an /api/generate request
// including a "context" field is accepted (not 400) and produces normal
// output — the field has no OpenAI-compatible equivalent and is simply
// ignored, never forwarded (FR-29, AC-23).
func TestServeGenerate_ContextFieldAccepted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "context") {
			t.Errorf("outbound request unexpectedly forwarded \"context\": %s", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"completion_tokens\":1}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	base, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	front := httptest.NewServer(NewHandler(base, ""))
	defer front.Close()

	reqBody := `{"model":"test-model","prompt":"hi","stream":false,"context":[1,2,3]}`
	resp, err := http.Post(front.URL+"/api/generate", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 (body: %s)", resp.StatusCode, body)
	}
	var chunk map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&chunk); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if chunk["response"] != "Hi" {
		t.Fatalf("response = %v, want %q", chunk["response"], "Hi")
	}
}

// TestServeGenerate_SystemFieldMappedToSystemMessage verifies an
// /api/generate request including a "system" field produces a translated
// upstream request whose messages array includes a system-role message
// prepended before the prompt-derived user message (FR-29, AC-23).
func TestServeGenerate_SystemFieldMappedToSystemMessage(t *testing.T) {
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode outbound request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"completion_tokens\":1}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	base, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	front := httptest.NewServer(NewHandler(base, ""))
	defer front.Close()

	reqBody := `{"model":"test-model","prompt":"hi","stream":false,"system":"be terse"}`
	resp, err := http.Post(front.URL+"/api/generate", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 (body: %s)", resp.StatusCode, body)
	}

	messages, ok := gotBody["messages"].([]any)
	if !ok || len(messages) < 2 {
		t.Fatalf("outbound messages = %v, want at least 2 entries (system + user)", gotBody["messages"])
	}
	first, ok := messages[0].(map[string]any)
	if !ok || first["role"] != "system" || first["content"] != "be terse" {
		t.Fatalf("messages[0] = %v, want {role:system, content:\"be terse\"}", messages[0])
	}
	second, ok := messages[1].(map[string]any)
	if !ok || second["role"] != "user" || second["content"] != "hi" {
		t.Fatalf("messages[1] = %v, want {role:user, content:\"hi\"}", messages[1])
	}
}

// TestServeGenerate_TemplateFieldIgnored verifies an /api/generate request
// including a "template" field is accepted and ignored without error — it
// has no OpenAI-compatible equivalent (FR-29, AC-23).
func TestServeGenerate_TemplateFieldIgnored(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "template") {
			t.Errorf("outbound request unexpectedly forwarded \"template\": %s", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"completion_tokens\":1}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	base, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	front := httptest.NewServer(NewHandler(base, ""))
	defer front.Close()

	reqBody := `{"model":"test-model","prompt":"hi","stream":false,"template":"{{ .Prompt }}"}`
	resp, err := http.Post(front.URL+"/api/generate", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 (body: %s)", resp.StatusCode, body)
	}
	var chunk map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&chunk); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if chunk["response"] != "Hi" {
		t.Fatalf("response = %v, want %q", chunk["response"], "Hi")
	}
}

// panicResponseWriter is a minimal http.ResponseWriter that also implements
// http.Flusher (so the handler's streaming path, not the "streaming
// unsupported" fallback, is reached) whose Write always panics — simulating
// a bad/closed connection (e.g. the client disconnected mid-write).
type panicResponseWriter struct {
	header http.Header
}

func (p *panicResponseWriter) Header() http.Header {
	if p.header == nil {
		p.header = make(http.Header)
	}
	return p.header
}

func (p *panicResponseWriter) Write([]byte) (int, error) {
	panic("simulated write failure")
}

func (p *panicResponseWriter) WriteHeader(int) {}

func (p *panicResponseWriter) Flush() {}

// TestServeChatStreaming_PanicRecovery verifies that a panic while writing to
// the ResponseWriter during the streaming write loop is recovered and logged
// rather than crashing the handler (and, in production, the whole broker
// process) — plan.md's Architecture section, "Panic recovery".
func TestServeChatStreaming_PanicRecovery(t *testing.T) {
	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(prevLogger)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"completion_tokens\":1}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	base, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	h := NewHandler(base, "")

	pw := &panicResponseWriter{}
	req := httptest.NewRequest(http.MethodPost, "/api/generate", strings.NewReader(`{"model":"test-model","prompt":"hi","stream":true}`))

	// This must not panic the test process — the handler is required to
	// recover internally rather than let a bad ResponseWriter crash it.
	h.ServeHTTP(pw, req)

	if !strings.Contains(logBuf.String(), "panic in openai handler stream write") {
		t.Fatalf("expected a panic-recovery log line, got: %q", logBuf.String())
	}
}

// alwaysServeAdm is a minimal queue.Admission that never yields — the happy
// path for TestGateOpenAIHandler_SuccessStreaming_HeadersAndTrailer, mirroring
// internal/queue's own unexported alwaysServe test type (which this package
// cannot reach directly since it lives in queue's _test.go files).
type alwaysServeAdm struct{}

func (alwaysServeAdm) Yielding() (bool, string)      { return false, "" }
func (alwaysServeAdm) ServeContext() context.Context { return context.Background() }

// alwaysYieldAdm is a minimal queue.Admission that always reports yielding,
// forcing queue.Gate's deferral path (Interactive class never parks, so this
// always resolves via deferRequest — FR-10) for
// TestGateOpenAIHandler_DeferredYield_Headers.
type alwaysYieldAdm struct{}

func (alwaysYieldAdm) Yielding() (bool, string)      { return true, "test forced yield" }
func (alwaysYieldAdm) ServeContext() context.Context { return context.Background() }

// TestGateOpenAIHandler_SuccessStreaming_HeadersAndTrailer is the FR-15/AC-10
// integration proof that a real queue.Gate, wired around the openai backend's
// NewHandler as `next` (exactly as cmd/broker/main.go composes it in
// production), still carries the X-Broker-Request-Id/Wait-Ms/Status contract
// correctly for a streamed request — both as headers (set optimistically
// before next runs, per gate.go) and, since this is a chunked NDJSON
// response, as the authoritative final-outcome trailer. Gate's trailer logic
// reads context/serve state, not what next wrote, so this proves the
// composition, not new behavior in either package (steps.md's "no new
// production code expected" premise for this task).
func TestGateOpenAIHandler_SuccessStreaming_HeadersAndTrailer(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"completion_tokens\":1}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	base, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	s := queue.New()
	gated := s.Gate(queue.Interactive, 2*time.Second, 0, alwaysServeAdm{}, nil, NewHandler(base, ""))
	front := httptest.NewServer(gated)
	defer front.Close()

	reqBody := `{"model":"test-model","prompt":"hi","stream":true}`
	resp, err := http.Post(front.URL+"/api/generate", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if reqID := resp.Header.Get("X-Broker-Request-Id"); reqID == "" {
		t.Fatal("missing X-Broker-Request-Id header")
	}
	waitMs := resp.Header.Get("X-Broker-Wait-Ms")
	if waitMs == "" {
		t.Fatal("missing X-Broker-Wait-Ms header")
	}
	if n, err := strconv.Atoi(waitMs); err != nil || n < 0 {
		t.Fatalf("X-Broker-Wait-Ms = %q, want a valid non-negative number", waitMs)
	}
	if got := resp.Header.Get("X-Broker-Status"); got != "served" {
		t.Fatalf("X-Broker-Status header = %q, want served", got)
	}

	// The trailer carries the authoritative final outcome and is only
	// populated on resp.Trailer once the chunked body has been fully read.
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("drain body: %v", err)
	}
	if got := resp.Trailer.Get("X-Broker-Status"); got != "served" {
		t.Fatalf("X-Broker-Status trailer = %q, want served", got)
	}
}

// TestGateOpenAIHandler_DeferredYield_Headers is the deferred-path half of
// the FR-15/AC-10 integration proof: with an Admission that always reports
// yielding, queue.Gate defers the request (Interactive never parks, FR-10)
// before ever invoking the openai handler's next.ServeHTTP — proven by the
// upstream call counter staying at zero — and the response still carries
// X-Broker-Request-Id and X-Broker-Status: deferred, matching the same
// deferral shape internal/proxy's ollama-backed Gate composition uses (see
// internal/proxy/proxy_test.go's TestErrorHandlerDeadlineExceededMatches503Shape
// for the sibling contract on the other backend).
func TestGateOpenAIHandler_DeferredYield_Headers(t *testing.T) {
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	base, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	s := queue.New()
	gated := s.Gate(queue.Interactive, 2*time.Second, 0, alwaysYieldAdm{}, nil, NewHandler(base, ""))
	front := httptest.NewServer(gated)
	defer front.Close()

	reqBody := `{"model":"test-model","prompt":"hi","stream":true}`
	resp, err := http.Post(front.URL+"/api/generate", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if reqID := resp.Header.Get("X-Broker-Request-Id"); reqID == "" {
		t.Fatal("missing X-Broker-Request-Id header")
	}
	if got := resp.Header.Get("X-Broker-Status"); got != "deferred" {
		t.Fatalf("X-Broker-Status = %q, want deferred", got)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("missing Retry-After header")
	}
	io.Copy(io.Discard, resp.Body)

	if upstreamCalls != 0 {
		t.Fatalf("upstream called %d times, want 0 (Gate must defer before reaching the openai handler)", upstreamCalls)
	}
}
