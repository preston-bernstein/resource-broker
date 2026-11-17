package broker_test

import (
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

// --- SSE events a mock OpenAI-compatible upstream sends ---

const sseDone = "data: [DONE]\n\n"

// sseDelta is one SSE event carrying a content delta.
func sseDelta(content string) string {
	return `data: {"choices":[{"delta":{"content":"` + content + `"}}]}` + "\n\n"
}

// sseStop is the final SSE event with a stop reason and no usage field,
// replicating vLLM's default streaming behavior (FR-26/AC-19).
const sseStop = `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n"

// sseStopWithUsage is the final SSE event with a stop reason and a usage count.
func sseStopWithUsage(tokens int) string {
	return fmt.Sprintf(`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"completion_tokens":%d}}`+"\n\n", tokens)
}

// writeSSE starts an event-stream response and writes each event to w.
func writeSSE(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, e := range events {
		_, _ = io.WriteString(w, e)
	}
}

// --- mock upstreams ---

// startServer runs h as a test server that closes with the test.
func startServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// pathUpstream is an upstream that serves path with h and 404s anything else.
func pathUpstream(t *testing.T, path string, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	return startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		h(w, r)
	}))
}

// chatUpstream is an OpenAI-compatible upstream that answers
// /v1/chat/completions with the given SSE events.
func chatUpstream(t *testing.T, events ...string) *httptest.Server {
	t.Helper()
	return pathUpstream(t, "/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, events...)
	})
}

// writeJSON answers with v as a JSON body.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse URL %q: %v", raw, err)
	}
	return u
}

// --- HTTP calls; each reply is read to its end ---

func get(t *testing.T, url string) testhttp.Reply {
	t.Helper()
	reply, err := testhttp.GetReply(url)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return reply
}

func post(t *testing.T, url, body string) testhttp.Reply {
	t.Helper()
	reply, err := testhttp.PostReply(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("%v", err)
	}
	return reply
}

// postDrained posts body, reads the response to its end, and returns the
// status and body.
func postDrained(t *testing.T, url, body string) (int, string) {
	t.Helper()
	resp := post(t, url, body)
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(raw)
}

// requireStatus fails unless resp has the want status.
func requireStatus(t *testing.T, resp testhttp.Reply, want int) {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("status = %d, want %d", resp.StatusCode, want)
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

// parseLine parses one NDJSON line into an object.
func parseLine(t *testing.T, line string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &out); err != nil {
		t.Fatalf("line %q is not valid JSON: %v", line, err)
	}
	return out
}
