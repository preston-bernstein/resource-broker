package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestGenerate_NonOKStatus verifies a non-2xx response from the upstream is
// mapped to an error (not a panic), per the plan's translation table
// ("error: non-2xx status" -> fmt.Errorf("openai upstream: status %d", ...)).
// This exercises Client.Generate without reaching parseSSEStream (Task 5b),
// since a non-OK status returns before any stream parsing is attempted.
func TestGenerate_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	c := New(base, "")

	_, _, err = c.Generate(context.Background(), GenerateRequest{Model: "test-model", Prompt: "hi"}, nil)
	if err == nil {
		t.Fatal("expected an error for a 500 upstream response, got nil")
	}
	const want = "openai upstream: status 500"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

// TestGenerate_URLJoining verifies the outbound request URL is built via
// proper URL joining rather than string concatenation: a base URL with a
// trailing slash must not produce a double slash before "v1/chat/completions".
func TestGenerate_URLJoining(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusInternalServerError) // short-circuits before SSE parsing
	}))
	defer srv.Close()

	// Deliberately construct a base URL WITH a trailing slash, mirroring an
	// operator-supplied UPSTREAM_URL with a trailing slash.
	base, err := url.Parse(srv.URL + "/")
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	c := New(base, "")

	_, _, _ = c.Generate(context.Background(), GenerateRequest{Model: "test-model", Prompt: "hi"}, nil)

	const want = "/v1/chat/completions"
	if gotPath != want {
		t.Fatalf("request path = %q, want %q (no double slash)", gotPath, want)
	}
}

// TestGenerate_AuthorizationHeader verifies Authorization is set only when
// apiKey is non-empty, and never sent as an empty Bearer token.
func TestGenerate_AuthorizationHeader(t *testing.T) {
	var gotAuth string
	sawAuthHeader := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, sawAuthHeader = r.Header["Authorization"]
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}

	// Non-empty key: header must be set to "Bearer <key>".
	c := New(base, "secret-key")
	_, _, _ = c.Generate(context.Background(), GenerateRequest{Model: "m", Prompt: "p"}, nil)
	if want := "Bearer secret-key"; gotAuth != want {
		t.Fatalf("Authorization = %q, want %q", gotAuth, want)
	}

	// Empty key: no Authorization header at all.
	c = New(base, "")
	_, _, _ = c.Generate(context.Background(), GenerateRequest{Model: "m", Prompt: "p"}, nil)
	if sawAuthHeader {
		t.Fatalf("Authorization header present with empty apiKey; want none, got %q", gotAuth)
	}
}

// TestGenerate_OptionsFilterReservedKeys verifies the four reserved keys
// (model, messages, stream, stream_options) in req.Options can never
// override the fields Client.Generate itself controls, while any other
// option key passes through untouched. No prior test ever populated
// GenerateRequest.Options at all, leaving this reserved-key filter
// (client.go's `if k == "model" || k == "messages" || k == "stream" || k
// == "stream_options" { continue }`) completely unexercised.
func TestGenerate_OptionsFilterReservedKeys(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("mock server: decode request: %v", err)
		}
		w.WriteHeader(http.StatusInternalServerError) // short-circuits before SSE parsing
	}))
	defer srv.Close()

	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	c := New(base, "")

	req := GenerateRequest{
		Model:  "real-model",
		Prompt: "hi",
		Options: map[string]any{
			"model":          "SHOULD-BE-IGNORED",
			"messages":       "SHOULD-BE-IGNORED",
			"stream":         "SHOULD-BE-IGNORED",
			"stream_options": "SHOULD-BE-IGNORED",
			"temperature":    0.9,
		},
	}
	_, _, _ = c.Generate(context.Background(), req, nil)

	if gotBody["model"] != "real-model" {
		t.Fatalf("model = %v, want %q (caller Options must not override it)", gotBody["model"], "real-model")
	}
	if gotBody["stream"] != true {
		t.Fatalf("stream = %v, want true (caller Options must not override it)", gotBody["stream"])
	}
	if _, ok := gotBody["stream_options"].(map[string]any); !ok {
		t.Fatalf("stream_options = %v, want a map (caller Options must not override it)", gotBody["stream_options"])
	}
	messages, ok := gotBody["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("messages = %v, want a single-element array (caller Options must not override it)", gotBody["messages"])
	}
	if got := gotBody["temperature"]; got != 0.9 {
		t.Fatalf("temperature = %v, want 0.9 (a non-reserved option must pass through)", got)
	}
}

// TestGenerate_SSEStreaming_Progressive verifies onTokens is called
// progressively as each SSE chunk arrives — not buffered until the stream
// ends. The mock server blocks after each flushed chunk until the client's
// onTokens callback for that chunk has actually fired (via the proceed
// channel round-trip), which is only possible if the client is decoding and
// dispatching chunks as they're read rather than after the full body is
// received. It also verifies token counts are correct and the final text is
// the correct concatenation, preferring the usage.completion_tokens field
// present on the final chunk.
func TestGenerate_SSEStreaming_Progressive(t *testing.T) {
	proceed := make(chan struct{})
	contents := []string{"Hello", ", ", "world"}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("test server ResponseWriter does not support flushing")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for i, c := range contents {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", c)
			flusher.Flush()
			if i < len(contents)-1 {
				// Bounded, not a bare <-proceed: if a client bug (or an
				// injected mutant, under mutation testing) never reaches the
				// onTokens callback that sends to proceed, this goroutine
				// must still exit so the deferred srv.Close() below (which
				// waits for in-flight handlers) doesn't hang the whole test
				// binary until the outer `go test -timeout` kills it.
				select {
				case <-proceed:
				case <-time.After(500 * time.Millisecond):
				}
			}
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{}}],\"usage\":{\"completion_tokens\":3}}\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	c := New(base, "")

	var gotCounts []int
	onTokens := func(n int) {
		gotCounts = append(gotCounts, n)
		select {
		case proceed <- struct{}{}:
		default:
			// Final chunk: the server isn't waiting on proceed anymore.
		}
	}

	text, tokens, err := c.Generate(context.Background(), GenerateRequest{Model: "m", Prompt: "p"}, onTokens)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "Hello, world"; text != want {
		t.Fatalf("text = %q, want %q", text, want)
	}
	if tokens != 3 {
		t.Fatalf("tokens = %d, want 3 (from usage.completion_tokens)", tokens)
	}
	wantCounts := []int{1, 2, 3}
	if len(gotCounts) != len(wantCounts) {
		t.Fatalf("onTokens calls = %v, want %v", gotCounts, wantCounts)
	}
	for i, want := range wantCounts {
		if gotCounts[i] != want {
			t.Fatalf("onTokens calls = %v, want %v", gotCounts, wantCounts)
		}
	}
}

// TestGenerate_SSEStreaming_ErrorMidStream verifies that when the upstream
// sends valid chunks followed by an in-band {"error":...} SSE event (rather
// than a clean [DONE]), the chunks that arrived before the error were still
// processed via onTokens, and Generate returns a non-nil error rather than a
// silently-truncated success.
func TestGenerate_SSEStreaming_ErrorMidStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"foo\"}}]}\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"bar\"}}]}\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: {\"error\":{\"message\":\"upstream exploded\"}}\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	c := New(base, "")

	var gotCounts []int
	onTokens := func(n int) { gotCounts = append(gotCounts, n) }

	_, _, err = c.Generate(context.Background(), GenerateRequest{Model: "m", Prompt: "p"}, onTokens)
	if err == nil {
		t.Fatal("expected an error for a mid-stream {\"error\":...} event, got nil")
	}
	wantCounts := []int{1, 2}
	if len(gotCounts) != len(wantCounts) || gotCounts[0] != wantCounts[0] || gotCounts[1] != wantCounts[1] {
		t.Fatalf("onTokens calls = %v, want %v (valid chunks processed before the error)", gotCounts, wantCounts)
	}
}

// TestGenerate_SSEStreaming_UsageOmitted verifies that when the final chunk
// omits the usage field entirely (vLLM's default behavior even when
// stream_options.include_usage was requested), the running per-chunk
// counter is used as the token count instead.
func TestGenerate_SSEStreaming_UsageOmitted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, c := range []string{"a", "b", "c", "d"} {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", c)
			flusher.Flush()
		}
		// Final chunk carries no usage field at all.
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{}}]}\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	c := New(base, "")

	text, tokens, err := c.Generate(context.Background(), GenerateRequest{Model: "m", Prompt: "p"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "abcd"; text != want {
		t.Fatalf("text = %q, want %q", text, want)
	}
	if tokens != 4 {
		t.Fatalf("tokens = %d, want 4 (per-chunk fallback, usage omitted)", tokens)
	}
}

// TestGenerate_StreamOptionsIncludeUsageInRequestBody verifies the outbound
// /v1/chat/completions request body always includes
// stream_options:{include_usage:true} (AC-19, second half) — the mechanism
// that asks upstreams supporting it for a final usage block, backstopped by
// TestGenerate_SSEStreaming_UsageOmitted's per-chunk fallback for upstreams
// (like vLLM by default) that don't honor the request.
func TestGenerate_StreamOptionsIncludeUsageInRequestBody(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode outbound request body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	c := New(base, "")

	if _, _, err := c.Generate(context.Background(), GenerateRequest{Model: "m", Prompt: "p"}, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	so, ok := gotBody["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("outbound request body stream_options = %v, want a map", gotBody["stream_options"])
	}
	if so["include_usage"] != true {
		t.Fatalf("stream_options.include_usage = %v, want true", so["include_usage"])
	}
}

// TestGenerate_ContextCancellationMidStream verifies that canceling the
// caller-supplied context partway through a streaming response causes
// Generate to return promptly with a context-cancellation error rather than
// hanging or returning a silently-truncated success — the Job-path analogue
// of a client disconnect (see handler_test.go's
// TestServeChat_ContextCancellationMidStream for the Synchronous-path
// analogue), and distinct from a Yield-triggered cancellation, which Task 8
// already covers at the queue.Gate layer. Mirrors Client.Generate's own doc
// comment: "including ctx.Err() short-circuiting on cancellation."
func TestGenerate_ContextCancellationMidStream(t *testing.T) {
	proceed := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"foo\"}}]}\n\n")
		flusher.Flush()
		// Bounded, not a bare <-proceed: onTokens (which closes proceed) only
		// fires if the client actually parses this chunk. A client bug (or an
		// injected mutant) that never gets there must not hang this goroutine
		// forever, or defer srv.Close() below hangs the whole test binary.
		select {
		case <-proceed:
		case <-time.After(500 * time.Millisecond):
		}
		// Write more after cancellation; since the request context is now
		// canceled, the underlying connection is torn down and this write is
		// expected to be a no-op/error, never a panic.
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"bar\"}}]}\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	c := New(base, "")

	ctx, cancel := context.WithCancel(context.Background())
	onTokens := func(n int) {
		cancel()       // simulate the caller (e.g. the Job worker) canceling mid-stream
		close(proceed) // let the mock upstream proceed, so its handler goroutine exits promptly
	}

	done := make(chan struct{})
	var genErr error
	go func() {
		_, _, genErr = c.Generate(ctx, GenerateRequest{Model: "m", Prompt: "p"}, onTokens)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Generate did not return after context cancellation (possible hang)")
	}

	if genErr == nil {
		t.Fatal("expected a context-cancellation error, got nil")
	}
	if !errors.Is(genErr, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled (or wrapping it)", genErr)
	}
}

// TestParseSSEStream_MultiLineDataFolding verifies that consecutive data:
// lines are accumulated into one logical SSE event (joined by "\n", per the
// SSE spec) before being parsed as JSON, rather than each data: line being
// treated as an independent event.
func TestParseSSEStream_MultiLineDataFolding(t *testing.T) {
	raw := "data: {\n" +
		"data: \"choices\":[{\"delta\":{\"content\":\"hi\"}}]\n" +
		"data: }\n" +
		"\n" +
		"data: [DONE]\n\n"

	var got []int
	text, tokens, err := parseSSEStream(context.Background(), strings.NewReader(raw), func(n int) { got = append(got, n) })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "hi" {
		t.Fatalf("text = %q, want %q", text, "hi")
	}
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("onTokens calls = %v, want [1] (one logical event folded from three data: lines)", got)
	}
	if tokens != 1 {
		t.Fatalf("tokens = %d, want 1", tokens)
	}
}

// TestParseSSEStream_AbruptEndWithoutDone verifies that a stream which ends
// (EOF) without ever sending a [DONE] sentinel is treated as an error rather
// than a silently-truncated success, mirroring ollama.Client.Generate's
// "stream ended without done" guard.
func TestParseSSEStream_AbruptEndWithoutDone(t *testing.T) {
	raw := "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"
	_, _, err := parseSSEStream(context.Background(), strings.NewReader(raw), nil)
	if err == nil {
		t.Fatal("expected an error for a stream that ends without [DONE], got nil")
	}
}

// TestParseSSEStream_ErrorWithEmptyMessageDefaultsToUnknown verifies an
// in-band {"error":{"message":""}} SSE event (an empty, not absent, message
// string) falls back to "unknown error" rather than surfacing a blank error
// message. No prior test ever sent an empty chunk.Error.Message — the one
// mid-stream-error test (TestGenerate_SSEStreaming_ErrorMidStream) always
// used a non-empty message and never asserted its exact text — so
// stream.go's `if msg == "" { msg = "unknown error" }` fallback was
// completely unexercised.
func TestParseSSEStream_ErrorWithEmptyMessageDefaultsToUnknown(t *testing.T) {
	raw := "data: {\"error\":{\"message\":\"\"}}\n\n"
	_, _, err := parseSSEStream(context.Background(), strings.NewReader(raw), nil)
	if err == nil {
		t.Fatal("expected an error for an in-band {\"error\":...} event, got nil")
	}
	if !strings.Contains(err.Error(), "unknown error") {
		t.Fatalf("err = %q, want it to contain %q (empty message must fall back)", err.Error(), "unknown error")
	}
}

// TestParseSSEStream_EmptyChoicesSkipped verifies a chunk whose "choices"
// array is present but empty (a plausible heartbeat/keepalive shape some
// OpenAI-compatible upstreams send) is skipped without panicking and without
// affecting the accumulated text, rather than indexing choices[0] on an
// empty slice. No prior test ever sent an empty "choices" array — every
// existing chunk fixture carries at least one entry — leaving the
// `len(chunk.Choices) > 0` guard's boundary (0 items) unexercised.
func TestParseSSEStream_EmptyChoicesSkipped(t *testing.T) {
	raw := "data: {\"choices\":[]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: [DONE]\n\n"
	text, _, err := parseSSEStream(context.Background(), strings.NewReader(raw), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "hi" {
		t.Fatalf("text = %q, want %q (an empty-choices chunk must be a no-op, not a panic)", text, "hi")
	}
}

// TestParseSSEStream_ZeroUsageTokensPreferredOverRunningCounter verifies
// that when the upstream reports usage.completion_tokens:0 on the final
// chunk (a legitimate value, not "absent"), the returned token count is 0
// — the explicit usage value always wins over the running per-chunk
// counter, even when that value is the zero value. No prior test ever sent
// completion_tokens:0 (only a positive count, or the field omitted
// entirely), leaving the `usageTokens >= 0` boundary (the exact point where
// "usage present but zero" and "usage absent" diverge) unexercised.
func TestParseSSEStream_ZeroUsageTokensPreferredOverRunningCounter(t *testing.T) {
	raw := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{}}],\"usage\":{\"completion_tokens\":0}}\n\n" +
		"data: [DONE]\n\n"
	_, tokens, err := parseSSEStream(context.Background(), strings.NewReader(raw), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tokens != 0 {
		t.Fatalf("tokens = %d, want 0 (usage.completion_tokens:0 must win over the running counter)", tokens)
	}
}

// TestParseSSEStream_DoneWithoutTrailingBlankLine_UsesUsageTokens verifies
// the same usage-tokens-win-when-present rule holds on the "trailing flush"
// path — when the upstream closes immediately after the [DONE] line with no
// following blank line, so [DONE] is only ever observed by the function's
// post-loop trailing-flush logic rather than the main loop's blank-line
// branch. No prior test ever ended a stream on [DONE] without a trailing
// blank line (TestParseSSEStream_AbruptEndWithoutDone covers the sibling
// "no DONE at all" case), leaving this second, duplicated usageTokens>=0
// check entirely uncovered.
func TestParseSSEStream_DoneWithoutTrailingBlankLine_UsesUsageTokens(t *testing.T) {
	raw := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{}}],\"usage\":{\"completion_tokens\":0}}\n\n" +
		"data: [DONE]\n" // deliberately no trailing blank line after [DONE]
	text, tokens, err := parseSSEStream(context.Background(), strings.NewReader(raw), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "hi" {
		t.Fatalf("text = %q, want %q", text, "hi")
	}
	if tokens != 0 {
		t.Fatalf("tokens = %d, want 0 (usage.completion_tokens:0 must win over the running counter on the trailing-flush path too)", tokens)
	}
}

// errReader is an io.Reader that always fails with a fixed, non-EOF error —
// used to prove a genuine scan error (not a context cancellation) is
// propagated as-is rather than masked.
type errReader struct {
	err error
}

func (r *errReader) Read(p []byte) (int, error) {
	return 0, r.err
}

// TestParseSSEStream_ReadErrorPropagatedWithoutCtxCancellation verifies
// that a genuine body-read error, with an UNcanceled context, is returned
// as that read error — not silently swallowed. stream.go's
// `if err := sc.Err(); err != nil { if ctx.Err() != nil { return ctx.Err() }
// return err }` guard exists specifically to prefer ctx.Err() only when the
// context actually is canceled; every prior scan-error test canceled the
// context (TestGenerate_ContextCancellationMidStream,
// TestServeChat_ContextCancellationMidStream), so the "genuine read error,
// context still fine" branch — where ctx.Err() is nil and must NOT be
// returned in place of the real error — was never exercised.
func TestParseSSEStream_ReadErrorPropagatedWithoutCtxCancellation(t *testing.T) {
	wantErr := errors.New("mock read failure")
	_, _, err := parseSSEStream(context.Background(), &errReader{err: wantErr}, nil)
	if err == nil {
		t.Fatal("expected the mock read error to be returned, got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want it to wrap/equal %v", err, wantErr)
	}
}
