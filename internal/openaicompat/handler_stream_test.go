package openaicompat

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
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

// captureLogs routes the default logger into the returned buffer until the
// test ends.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// holdingUpstream sends one "Hello" chunk, waits (bounded) for release, then
// sends final and [DONE]. The wait is bounded, not a bare <-release: close(release)
// only runs if the handler actually relays line 1 to the client. A handler bug
// (or an injected mutant, under mutation testing) that fails before that point
// must not hang this goroutine forever, or the upstream's Close (which waits
// for in-flight handlers) hangs the whole test binary until the outer
// `go test -timeout` kills it.
func holdingUpstream(t *testing.T, release <-chan struct{}, final ...string) *httptest.Server {
	t.Helper()
	return startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream ResponseWriter is not a Flusher")
			return
		}
		writeSSE(w, sseDelta("Hello"))
		fl.Flush()
		select {
		case <-release:
		case <-time.After(500 * time.Millisecond):
		}
		writeSSE(w, final...)
		fl.Flush()
	}))
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
	front := frontFor(t, holdingUpstream(t, release, sseStop(1), sseDone), "")

	withLivePost(t, front.URL+"/api/generate", `{"model":"test-model","prompt":"hi","stream":true}`, func(resp *http.Response) {
		br := bufio.NewReader(resp.Body)
		line1, err := readLineWithin(br, 2*time.Second)
		if err != nil {
			t.Fatalf("reading NDJSON line 1 (handler likely buffered): %v", err)
		}
		chunk1 := parseLine(t, line1)
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
		chunk2 := parseLine(t, line2)
		if chunk2["done"] != true {
			t.Fatalf("line2 done = %v, want true", chunk2["done"])
		}
		// line2 (the final, done:true line) carries an empty response: "Hello"
		// was already relayed on line1, and repeating it here would double the
		// text for any client that concatenates every line's "response" field.
		if chunk2["response"] != "" {
			t.Fatalf("line2 response = %v, want %q (full text already sent on line1; final line must not repeat it)", chunk2["response"], "")
		}
	})
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
// doesn't hang.
func TestServeChat_ContextCancellationMidStream(t *testing.T) {
	logBuf := captureLogs(t)

	release := make(chan struct{})
	// A write after the front-to-upstream connection was torn down by the
	// canceled context is expected to be a no-op/error, never a panic.
	upstream := holdingUpstream(t, release, sseDelta("world"))
	front := frontFor(t, upstream, "")

	baseGoroutines := runtime.NumGoroutine()

	withCancelablePost(t, front.URL+"/api/generate", `{"model":"test-model","prompt":"hi","stream":true}`, func(cancel context.CancelFunc, resp *http.Response) {
		br := bufio.NewReader(resp.Body)
		line1, err := readLineWithin(br, 2*time.Second)
		if err != nil {
			t.Fatalf("reading NDJSON line 1: %v", err)
		}
		if chunk1 := parseLine(t, line1); chunk1["done"] != false {
			t.Fatalf("line1 done = %v, want false", chunk1["done"])
		}

		cancel()       // simulate the client disconnecting mid-stream
		close(release) // let the mock upstream's handler goroutine exit

		// The connection must be torn down promptly (not hang) once the
		// client's context is canceled.
		if _, err := readLineWithin(br, 2*time.Second); err == nil {
			t.Fatal("expected the connection to be torn down after context cancellation, got a successful read")
		}
	})

	// The handler must not have recovered from a panic — a clean early return
	// via ctx cancellation, not a bad-write panic.
	if strings.Contains(logBuf.String(), "panic in openai handler stream write") {
		t.Fatalf("handler recovered from a panic on context cancellation, want a clean exit: %q", logBuf.String())
	}

	// No goroutine leak. The pooled connection front used to call upstream
	// (see proxy.Transport's IdleConnTimeout) can keep a readLoop goroutine
	// parked, which is deliberate pooling, not a leak. Closing both servers
	// here, before counting, tears it down; the startServer cleanup would
	// run too late. Close is idempotent.
	upstream.Close()
	front.Close()
	requireNoGoroutineLeak(t, baseGoroutines)
}

// requireNoGoroutineLeak waits a bounded time for the goroutine count to drop
// back near base: a short poll for genuinely asynchronous teardown, not a bare
// immediate check.
func requireNoGoroutineLeak(t *testing.T, base int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > base+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > base+2 {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Fatalf("goroutine count = %d, want <= %d (possible leak after context cancellation)\n%s", got, base+2, buf[:n])
	}
}

// TestServeChat_StreamTrueValidNDJSON verifies a stream:true request against
// a fast (non-blocking) mock upstream produces multiple NDJSON lines, each
// independently valid JSON.
func TestServeChat_StreamTrueValidNDJSON(t *testing.T) {
	front := frontFor(t, sseUpstream(t, sseDelta("Hel"), sseDelta("lo"), sseStop(2), sseDone), "")

	resp := postChat(t, front, "/api/chat", `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	lines := ndjsonLines(t, resp)
	if len(lines) < 2 {
		t.Fatalf("got %d NDJSON lines, want at least 2 (got %q)", len(lines), lines)
	}
	var sawDoneTrue bool
	var concatenated strings.Builder
	for _, line := range lines {
		chunk := parseLine(t, line)
		if text, ok := chunk["response"].(string); ok {
			concatenated.WriteString(text)
		}
		if chunk["done"] == true {
			sawDoneTrue = true
		}
	}
	if !sawDoneTrue {
		t.Fatalf("no NDJSON line with done:true in %q", lines)
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
	front := frontFor(t, sseUpstream(t, sseDelta("Hello"), sseStop(1), sseDone), "")

	resp := postChat(t, front, "/api/generate", `{"model":"test-model","prompt":"hi","stream":false}`)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	trimmed := strings.TrimSpace(string(body))
	if strings.Contains(trimmed, "\n") {
		t.Fatalf("stream:false body has more than one line (NDJSON leaked through): %q", trimmed)
	}
	chunk := parseLine(t, trimmed)
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
	front := frontFor(t, sseUpstream(t, sseDelta("Hi"), sseStop(1), sseDone), "")

	resp := postChat(t, front, "/api/generate", `{"model":"test-model","prompt":"hi"}`)
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("Content-Type = %q, want application/x-ndjson (stream should default to true)", ct)
	}
	if lines := ndjsonLines(t, resp); len(lines) < 2 {
		t.Fatalf("got %d NDJSON lines, want at least 2 when stream is omitted", len(lines))
	}
}

// TestServeChat_MidStreamError_FinalNDJSONErrorLine verifies that once NDJSON
// chunks have already been flushed to the client for a stream:true request,
// an in-band upstream failure (here, a malformed SSE data event) is surfaced
// as one final Ollama-shaped NDJSON line carrying an "error" field — never a
// second HTTP status — mirroring ollama.Client.Generate's chunk.Error
// convention (plan.md's "Mid-stream error handling").
func TestServeChat_MidStreamError_FinalNDJSONErrorLine(t *testing.T) {
	front := frontFor(t, sseUpstream(t, sseDelta("Hel"), sseDelta("lo"), "data: not-valid-json\n\n"), "")

	resp := postChat(t, front, "/api/generate", `{"model":"test-model","prompt":"hi","stream":true}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a mid-stream failure can't change an already-committed status)", resp.StatusCode)
	}
	lines := ndjsonLines(t, resp)
	if len(lines) != 3 {
		t.Fatalf("got %d NDJSON lines, want 3 (2 valid + 1 final error line) (body: %q)", len(lines), lines)
	}
	for i := 0; i < 2; i++ {
		chunk := parseLine(t, lines[i])
		if chunk["done"] != false {
			t.Fatalf("line %d done = %v, want false", i, chunk["done"])
		}
		if _, ok := chunk["error"]; ok {
			t.Fatalf("line %d unexpectedly has an \"error\" field: %v", i, chunk)
		}
	}
	last := parseLine(t, lines[2])
	errMsg, ok := last["error"]
	if !ok {
		t.Fatalf("final line %v has no \"error\" field", last)
	}
	if s, ok := errMsg.(string); !ok || s == "" {
		t.Fatalf("final line \"error\" field = %v, want a non-empty string", errMsg)
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
	logBuf := captureLogs(t)

	upstream := sseUpstream(t, sseDelta("Hi"), sseStop(1), sseDone)
	h := NewHandler(mustParseURL(t, upstream.URL), "")

	pw := &panicResponseWriter{}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/generate", strings.NewReader(`{"model":"test-model","prompt":"hi","stream":true}`))

	// This must not panic the test process — the handler is required to
	// recover internally rather than let a bad ResponseWriter crash it.
	h.ServeHTTP(pw, req)

	if !strings.Contains(logBuf.String(), "panic in openai handler stream write") {
		t.Fatalf("expected a panic-recovery log line, got: %q", logBuf.String())
	}
}
