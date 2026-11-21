package openaicompat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/preston-bernstein/resource-broker/internal/testhttp"
)

const sseDone = "data: [DONE]\n\n"

// sseDelta is one SSE event carrying a content delta.
func sseDelta(content string) string {
	return `data: {"choices":[{"delta":{"content":"` + content + `"}}]}` + "\n\n"
}

// sseStop is the final SSE event: a stop reason plus the usage count.
func sseStop(tokens int) string {
	return fmt.Sprintf(`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"completion_tokens":%d}}`+"\n\n", tokens)
}

// writeSSE starts an event-stream response and writes each event to w.
func writeSSE(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, e := range events {
		_, _ = io.WriteString(w, e)
	}
}

// startServer runs h as a test server that closes with the test.
func startServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// sseUpstream is an upstream that answers every request with events.
func sseUpstream(t *testing.T, events ...string) *httptest.Server {
	t.Helper()
	return startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, events...)
	}))
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse URL %q: %v", raw, err)
	}
	return u
}

// frontFor runs the handler under test in front of upstream.
func frontFor(t *testing.T, upstream *httptest.Server, apiKey string) *httptest.Server {
	t.Helper()
	return startServer(t, NewHandler(mustParseURL(t, upstream.URL), apiKey))
}

// post sends body as JSON to url and reads the whole reply.
func post(t *testing.T, url, body string) testhttp.Reply {
	t.Helper()
	reply, err := testhttp.PostReply(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("%v", err)
	}
	return reply
}

// get sends a GET to url and reads the whole reply.
func get(t *testing.T, url string) testhttp.Reply {
	t.Helper()
	reply, err := testhttp.GetReply(url)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return reply
}

// withLivePost posts body to url and hands use the open response, so a test
// can read the stream as it arrives rather than after it ends.
func withLivePost(t *testing.T, url, body string, use func(*http.Response)) {
	t.Helper()
	withCancelablePost(t, url, body, func(_ context.CancelFunc, resp *http.Response) { use(resp) })
}

// liveUse reads a streaming response. cancel ends the request's context.
type liveUse func(cancel context.CancelFunc, resp *http.Response)

// withCancelablePost is withLivePost with a cancel func for the request's
// context, to simulate the client disconnecting.
func withCancelablePost(t *testing.T, url, body string, use liveUse) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err := testhttp.WithPost(ctx, url, strings.NewReader(body), func(resp *http.Response) { use(cancel, resp) })
	if err != nil {
		t.Fatalf("%v", err)
	}
}

// postChat posts body to the handler's path on front.
func postChat(t *testing.T, front *httptest.Server, path, body string) testhttp.Reply {
	t.Helper()
	return post(t, front.URL+path, body)
}

// requireStatus fails unless resp has the want status, showing the body.
func requireStatus(t *testing.T, resp testhttp.Reply, want int) {
	t.Helper()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d (body: %s)", resp.StatusCode, want, body)
	}
}

// decodeObject decodes one JSON object from r.
func decodeObject(t *testing.T, r io.Reader) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.NewDecoder(r).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return out
}

// ndjsonLines reads resp's whole body and returns its non-empty lines.
func ndjsonLines(t *testing.T, resp testhttp.Reply) []string {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(body)), "\n")
}

// parseLine parses one NDJSON line into an object.
func parseLine(t *testing.T, line string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &out); err != nil {
		t.Fatalf("line %q is not valid JSON: %v", line, err)
	}
	return out
}

// requireErrorBody checks resp holds a JSON {"error": ...} body and returns
// the message.
func requireErrorBody(t *testing.T, resp testhttp.Reply) string {
	t.Helper()
	body := decodeObject(t, resp.Body)
	msg, ok := body["error"].(string)
	if !ok {
		t.Fatalf("body %v has no \"error\" field", body)
	}
	return msg
}
