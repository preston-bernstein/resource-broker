package openaicompat

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// countingUpstream is an upstream that counts its calls and otherwise ends the
// stream immediately.
func countingUpstream(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeSSE(w, sseDone)
	}))
	return srv, &calls
}

// capturingUpstream is an upstream that records the JSON body and
// Authorization header of the request it receives and answers with a short
// successful stream.
type capturingUpstream struct {
	*httptest.Server
	raw  string
	body map[string]any
	auth string
}

func newCapturingUpstream(t *testing.T, events ...string) *capturingUpstream {
	t.Helper()
	c := &capturingUpstream{}
	c.Server = startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.auth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		c.raw = string(raw)
		if err := json.Unmarshal(raw, &c.body); err != nil {
			t.Errorf("mock server: decode request: %v", err)
		}
		writeSSE(w, events...)
	}))
	return c
}

// TestServeEmbed verifies /api/embed reaches the embed translation and
// returns an Ollama-shaped embeddings response (AC-5).
func TestServeEmbed(t *testing.T) {
	upstream := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("upstream path = %q, want /v1/embeddings", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"embed-model","data":[{"embedding":[0.1,0.2],"index":0},{"embedding":[0.3,0.4],"index":1}]}`))
	}))
	front := frontFor(t, upstream, "")

	resp := postChat(t, front, "/api/embed", `{"model":"embed-model","input":["a","b"]}`)
	requireStatus(t, resp, http.StatusOK)

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
	// The base URL is never dialed — 404 short-circuits first.
	front := startServer(t, NewHandler(mustParseURL(t, "http://127.0.0.1:1"), ""))

	resp := get(t, front.URL+"/api/tags")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	requireErrorBody(t, resp)
}

// TestServeChat_MalformedJSONBody400 verifies a request body that fails to
// decode as JSON is rejected with 400 and a "decode request: ..." message,
// rather than reaching the upstream. No prior test ever posted a malformed
// body to /api/generate or /api/chat — every fixture was valid JSON —
// leaving handler.go's decode-error branch unexercised.
func TestServeChat_MalformedJSONBody400(t *testing.T) {
	upstream, calls := countingUpstream(t)
	front := frontFor(t, upstream, "")

	resp := postChat(t, front, "/api/generate", `{not valid json`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if msg := requireErrorBody(t, resp); !strings.Contains(msg, "decode request") {
		t.Fatalf("error message = %q, want a message containing \"decode request\"", msg)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("upstream was called %d times, want 0 (a malformed body must never be forwarded)", n)
	}
}

// TestServeChat_AuthorizationHeaderSetWhenAPIKeyConfigured verifies the
// handler's outbound chat request (both stream:true and stream:false, since
// both funnel through Client.openChatStream) carries "Authorization: Bearer
// <key>" when NewHandler was configured with a non-empty apiKey. The
// equivalent check on Client.Generate's own request path is covered
// separately by TestGenerate_AuthorizationHeader.
func TestServeChat_AuthorizationHeaderSetWhenAPIKeyConfigured(t *testing.T) {
	upstream := newCapturingUpstream(t, sseDelta("hi"), sseDone)
	front := frontFor(t, upstream.Server, "secret-key")

	postChat(t, front, "/api/generate", `{"model":"test-model","prompt":"hi","stream":false}`)

	if want := "Bearer secret-key"; upstream.auth != want {
		t.Fatalf("Authorization = %q, want %q", upstream.auth, want)
	}
}

// TestServeChat_OptionsFilterReservedKeys verifies a /api/chat request's
// "options" object can't override the fields the handler itself controls
// (model, messages, stream, stream_options) when relayed to the upstream,
// while a non-reserved option key passes through. The equivalent check on
// Client.Generate's own options is covered separately by
// TestGenerate_OptionsFilterReservedKeys.
func TestServeChat_OptionsFilterReservedKeys(t *testing.T) {
	upstream := newCapturingUpstream(t, sseDelta("hi"), sseDone)
	front := frontFor(t, upstream.Server, "")

	postChat(t, front, "/api/generate", `{"model":"real-model","prompt":"hi","stream":false,"options":{`+
		`"model":"SHOULD-BE-IGNORED","messages":"SHOULD-BE-IGNORED",`+
		`"stream":"SHOULD-BE-IGNORED","stream_options":"SHOULD-BE-IGNORED",`+
		`"temperature":0.7}}`)

	gotBody := upstream.body
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
// already covered for empty/non-empty.
func TestServeChat_ImagesStringField(t *testing.T) {
	t.Run("empty string accepted", func(t *testing.T) {
		front := frontFor(t, sseUpstream(t, sseDelta("hi"), sseDone), "")

		resp := postChat(t, front, "/api/chat", `{"model":"test-model","messages":[{"role":"user","content":"hi","images":""}],"stream":false}`)
		requireStatus(t, resp, http.StatusOK)
	})

	t.Run("non-empty string rejected", func(t *testing.T) {
		upstream, calls := countingUpstream(t)
		front := frontFor(t, upstream, "")

		resp := postChat(t, front, "/api/chat", `{"model":"test-model","messages":[{"role":"user","content":"hi","images":"base64blob"}],"stream":false}`)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (a non-empty string \"images\" must be rejected)", resp.StatusCode)
		}
		if n := calls.Load(); n != 0 {
			t.Fatalf("upstream was called %d times, want 0", n)
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
	upstream := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	front := frontFor(t, upstream, "")

	resp := postChat(t, front, "/api/generate", `{"model":"test-model","prompt":"hi","stream":true}`)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	requireErrorBody(t, resp)
}

// TestServeChat_UnsupportedImagesField400 verifies a /api/chat request whose
// message includes a non-empty "images" field is rejected with HTTP 400 and
// a descriptive JSON error body naming the field, and — critically — that no
// request is ever forwarded to the upstream (FR-27, AC-20). The mock
// upstream increments a call counter so the test can assert it was never hit.
func TestServeChat_UnsupportedImagesField400(t *testing.T) {
	upstream, calls := countingUpstream(t)
	front := frontFor(t, upstream, "")

	resp := postChat(t, front, "/api/chat", `{"model":"test-model","messages":[{"role":"user","content":"describe this","images":["base64data"]}],"stream":false}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if msg := requireErrorBody(t, resp); !strings.Contains(msg, "images") {
		t.Fatalf("error message = %q, want a descriptive message naming \"images\"", msg)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("upstream was called %d times, want 0 (request must never be forwarded)", n)
	}
}

// requireBufferedHi posts body to /api/generate (or /api/chat) and checks a
// 200 response whose "response" field is "Hi".
func requireBufferedHi(t *testing.T, front *httptest.Server, path, body string) {
	t.Helper()
	resp := postChat(t, front, path, body)
	requireStatus(t, resp, http.StatusOK)
	if chunk := decodeObject(t, resp.Body); chunk["response"] != "Hi" {
		t.Fatalf("response = %v, want %q", chunk["response"], "Hi")
	}
}

// TestServeChat_NoUnsupportedFields_StillSucceeds is a regression check
// verifying a valid /api/chat request with no unsupported fields (an empty
// "images" array, which some Ollama clients send even absent an actual
// image) still succeeds normally.
func TestServeChat_NoUnsupportedFields_StillSucceeds(t *testing.T) {
	front := frontFor(t, sseUpstream(t, sseDelta("Hi"), sseStop(1), sseDone), "")
	requireBufferedHi(t, front, "/api/chat", `{"model":"test-model","messages":[{"role":"user","content":"hi","images":[]}],"stream":false}`)
}

// TestServeGenerate_ContextFieldAccepted verifies an /api/generate request
// including a "context" field is accepted (not 400) and produces normal
// output — the field has no OpenAI-compatible equivalent and is simply
// ignored, never forwarded (FR-29, AC-23).
func TestServeGenerate_ContextFieldAccepted(t *testing.T) {
	upstream := newCapturingUpstream(t, sseDelta("Hi"), sseStop(1), sseDone)
	front := frontFor(t, upstream.Server, "")

	requireBufferedHi(t, front, "/api/generate", `{"model":"test-model","prompt":"hi","stream":false,"context":[1,2,3]}`)

	if strings.Contains(upstream.raw, "context") {
		t.Errorf("outbound request unexpectedly forwarded \"context\": %s", upstream.raw)
	}
}

// TestServeGenerate_SystemFieldMappedToSystemMessage verifies an
// /api/generate request including a "system" field produces a translated
// upstream request whose messages array includes a system-role message
// prepended before the prompt-derived user message (FR-29, AC-23).
func TestServeGenerate_SystemFieldMappedToSystemMessage(t *testing.T) {
	upstream := newCapturingUpstream(t, sseDelta("Hi"), sseStop(1), sseDone)
	front := frontFor(t, upstream.Server, "")

	resp := postChat(t, front, "/api/generate", `{"model":"test-model","prompt":"hi","stream":false,"system":"be terse"}`)
	requireStatus(t, resp, http.StatusOK)

	messages, ok := upstream.body["messages"].([]any)
	if !ok || len(messages) < 2 {
		t.Fatalf("outbound messages = %v, want at least 2 entries (system + user)", upstream.body["messages"])
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
	upstream := newCapturingUpstream(t, sseDelta("Hi"), sseStop(1), sseDone)
	front := frontFor(t, upstream.Server, "")

	requireBufferedHi(t, front, "/api/generate", `{"model":"test-model","prompt":"hi","stream":false,"template":"{{ .Prompt }}"}`)

	if strings.Contains(upstream.raw, "template") {
		t.Errorf("outbound request unexpectedly forwarded \"template\": %s", upstream.raw)
	}
}
