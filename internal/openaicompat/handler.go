package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/preston-bernstein/ollama-resource-broker/internal/proxy"
)

// NewHandler returns an http.Handler that presents Ollama's Synchronous wire
// API (/api/generate, /api/chat, /api/embed) while translating to/from an
// OpenAI-compatible upstream at base (see package doc and
// docs/openai-compatible-upstream-backend/plan.md's Architecture section).
// apiKey may be empty, in which case no Authorization header is ever sent.
//
// Only these three routes exist under this handler — any other path
// (including Ollama-only endpoints such as /api/tags, /api/show, /api/ps)
// receives 404 (FR-28, AC-21). This is a deliberate, documented behavior
// difference from the ollama backend's transparent reverse proxy.
//
// Error handling follows two rules described in plan.md's Architecture
// section: a failure BEFORE any response bytes reach the client (upstream
// unreachable, non-2xx before streaming begins, or an already-canceled
// r.Context()) goes through proxy.WriteUpstreamError's 503/deferred shape
// with a 502 JSON fallback when that helper declines the error (see
// writeUpstreamErrorOrFallback); a failure AFTER NDJSON chunks have already
// been flushed for a stream:true request is surfaced as one final
// Ollama-shaped NDJSON line carrying an "error" field, mirroring
// ollama.Client.Generate's in-band chunk.Error convention, since the HTTP
// status can no longer change at that point.
func NewHandler(base *url.URL, apiKey string) http.Handler {
	return &handler{client: New(base, apiKey)}
}

type handler struct {
	client *Client
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/generate", "/api/chat":
		h.serveChat(w, r)
	case "/api/embed":
		h.serveEmbed(w, r)
	default:
		writeJSONError(w, http.StatusNotFound, "not found: openai backend only supports /api/generate, /api/chat, /api/embed")
	}
}

// ollamaChatRequest is the incoming Ollama-shaped request body shared by
// /api/generate ({model, prompt, system, template, context, stream,
// options}) and /api/chat ({model, messages, stream, options}). Prompt,
// System, Template, and Context are unused for /api/chat and Messages is
// unused for /api/generate; each handler only reads the fields it expects.
type ollamaChatRequest struct {
	Model    string           `json:"model"`
	Prompt   string           `json:"prompt"`
	Messages []map[string]any `json:"messages"`
	// System, when present on an /api/generate request, is mapped to a
	// system-role message prepended to the translated messages array
	// (FR-29, AC-23). It has no meaning for /api/chat (Ollama's chat shape
	// carries system content as a message with role:"system" instead), so
	// it is only consulted when messages is built from Prompt below.
	System string `json:"system"`
	// Template has no OpenAI-compatible equivalent and is always ignored
	// without error (FR-29, AC-23) — declared here only so its presence in
	// the request body never trips an unknown-field error.
	Template string `json:"template"`
	// Context is Ollama's /api/generate token-continuation state. It has no
	// OpenAI-compatible equivalent: accepted without error but never
	// forwarded and never acted on (FR-29, AC-23). json.RawMessage rather
	// than a concrete type since its shape (an opaque token-id array) is
	// never inspected.
	Context json.RawMessage `json:"context"`
	// Stream is a pointer so an absent field is distinguishable from an
	// explicit false — Ollama defaults stream to true when the field is
	// omitted entirely, and that default must be preserved here (FR-25).
	Stream  *bool          `json:"stream"`
	Options map[string]any `json:"options"`
}

// ollamaChatChunk is a single Ollama-shaped NDJSON line (stream:true) or the
// single buffered JSON object (stream:false) written for /api/generate and
// /api/chat. EvalCount is omitted on intermediate streamed chunks (it is
// only known once generation completes) and populated on the final line.
type ollamaChatChunk struct {
	Model     string `json:"model"`
	Response  string `json:"response"`
	Done      bool   `json:"done"`
	EvalCount int    `json:"eval_count,omitempty"`
}

func (h *handler) serveChat(w http.ResponseWriter, r *http.Request) {
	var req ollamaChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "decode request: "+err.Error())
		return
	}

	// Hard-reject any message carrying a field this translator does not know
	// how to forward — most notably a non-empty "images" array (multimodal
	// input is out of scope) — BEFORE any upstream request is constructed
	// (FR-27, AC-20). This is a deliberate design decision (plan.md's
	// "reject unsupported /api/chat fields" paragraph, Design decisions #3):
	// silently dropping the field and returning a degraded text-only
	// response is a worse failure mode than an explicit, immediate 400.
	if field, ok := unsupportedMessageField(req.Messages); ok {
		writeJSONError(w, http.StatusBadRequest, "unsupported field: "+field)
		return
	}

	stream := true
	if req.Stream != nil {
		stream = *req.Stream
	}

	// /api/chat's messages pass through as-is (plain-text case); /api/generate
	// (and a /api/chat request that omitted messages) build a single
	// user-role message from prompt, mirroring Client.Generate's own
	// prompt->message translation.
	messages := req.Messages
	if len(messages) == 0 {
		messages = []map[string]any{{"role": "user", "content": req.Prompt}}
		// System, when present, is an /api/generate-only field mapped to a
		// system-role message prepended before the prompt-derived user
		// message (FR-29, AC-23). Template and Context (declared on
		// ollamaChatRequest above) are intentionally never consulted here:
		// both are accepted without error but have no translated-request
		// effect.
		if req.System != "" {
			messages = append([]map[string]any{{"role": "system", "content": req.System}}, messages...)
		}
	}

	if !stream {
		h.serveChatBuffered(w, r, req.Model, messages, req.Options)
		return
	}
	h.serveChatStreaming(w, r, req.Model, messages, req.Options)
}

// serveChatBuffered handles stream:false: the same underlying streaming
// upstream call is made (see chatCompletion), but the full response is
// collected internally and written as one Ollama-shaped JSON object — no
// NDJSON framing (FR-25, AC-18). Because nothing is written to w until the
// call fully succeeds, any error here — whether a connection-level failure,
// a non-2xx status, or an in-band SSE error partway through the upstream's
// own stream — is a genuine pre-response failure from this client's point of
// view, and goes through writeUpstreamErrorOrFallback.
func (h *handler) serveChatBuffered(w http.ResponseWriter, r *http.Request, model string, messages []map[string]any, options map[string]any) {
	text, tokens, err := h.client.chatCompletion(r.Context(), model, messages, options, nil)
	if err != nil {
		writeUpstreamErrorOrFallback(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ollamaChatChunk{
		Model:     model,
		Response:  text,
		Done:      true,
		EvalCount: tokens,
	})
}

// serveChatStreaming handles stream:true (or unset): writes an Ollama NDJSON
// line as each upstream chunk arrives, flushing after every write, mirroring
// proxy.go's FlushInterval:-1 streaming guarantee (FR-8, AC-4).
//
// Content-delivery note: each intermediate NDJSON line carries that chunk's
// actual delta text (via parseSSEStreamChunks, internal/openaicompat/
// stream.go) — matching real Ollama's own streaming convention, where a
// client displaying tokens as they arrive concatenates each line's
// "response" field to render progressively. The final line carries an empty
// "response" plus done:true and eval_count, since the full text was already
// relayed across the preceding lines; sending it again there would double
// the content for any client that concatenates every line.
func (h *handler) serveChatStreaming(w http.ResponseWriter, r *http.Request, model string, messages []map[string]any, options map[string]any) {
	// Panic recovery (queue.Gate's outcome trailer is driven by context
	// state, not by what this handler writes, so a recovered panic here
	// leaves that composition intact — see plan.md's "Trailer correctness
	// and single-write discipline" risk note). Wrapping the whole function,
	// not just the post-header write loop, also catches a panic from the
	// initial w.WriteHeader/flusher.Flush() below on a bad/closed
	// ResponseWriter, not only from writes deeper in the loop.
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("panic in openai handler stream write", "recover", rec)
		}
	}()

	ctx := r.Context()
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	// Establish and validate the upstream connection (network error or
	// non-2xx status) BEFORE writing anything to the client. This is a
	// genuine pre-response failure — no bytes have reached the client yet —
	// so it gets the same 503/deferred (or 502 fallback) shape every other
	// pre-response error in this package uses, rather than the in-band NDJSON
	// convention reserved for failures after streaming has actually begun.
	resp, err := h.client.openChatStream(ctx, model, messages, options)
	if err != nil {
		writeUpstreamErrorOrFallback(w, r, err)
		return
	}
	defer drainClose(resp)

	// From here on, the 200 status is about to be committed (or already has
	// been) — single-write discipline applies: never call w.WriteHeader
	// again, and any later failure can only be surfaced in-band.
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	enc := json.NewEncoder(w)
	onChunk := func(delta string, _ int) {
		enc.Encode(ollamaChatChunk{Model: model, Response: delta, Done: false})
		flusher.Flush()
	}

	_, tokens, err := parseSSEStreamChunks(ctx, resp.Body, onChunk)
	if err != nil {
		// Mid-stream failure: the 200 status is already committed, so a
		// status-code change is impossible now. Mirror ollama.Client.
		// Generate's in-band chunk.Error convention with one final
		// Ollama-shaped NDJSON line and stop — never attempt w.WriteHeader
		// again.
		enc.Encode(map[string]string{"error": "openai: " + err.Error()})
		flusher.Flush()
		return
	}

	// Response is intentionally omitted (zero value ""): the full text was
	// already relayed across the preceding onChunk-driven lines above.
	enc.Encode(ollamaChatChunk{
		Model:     model,
		Done:      true,
		EvalCount: tokens,
	})
	flusher.Flush()
}

// openChatStream builds and sends the outbound POST {base}/v1/chat/completions
// request for an arbitrary messages array (unlike Client.Generate, which
// always builds a single user-role message from a prompt) and validates the
// upstream's response — a connection-level error or a non-2xx status — before
// returning. Splitting this out from response-body consumption lets callers
// (serveChatStreaming) distinguish a pre-response failure (nothing written to
// the original client yet, so proxy.WriteUpstreamError's 503/502 shape
// applies) from a genuine mid-stream failure, which can only be surfaced
// in-band once bytes have already been relayed. It always requests
// stream:true from the upstream regardless of the Consumer's own stream
// preference — the caller decides whether to relay chunks incrementally or
// buffer them (see serveChatStreaming/serveChatBuffered).
//
// On success, the caller owns the returned *http.Response and must eventually
// drainClose it. On error the response (if any) has already been drained and
// closed here.
func (c *Client) openChatStream(ctx context.Context, model string, messages []map[string]any, options map[string]any) (*http.Response, error) {
	body := map[string]any{
		"model":          model,
		"messages":       messages,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	for k, v := range options {
		// Never let caller options override the fields this method controls.
		if k == "model" || k == "messages" || k == "stream" || k == "stream_options" {
			continue
		}
		body[k] = v
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url("v1", "chat", "completions"), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		drainClose(resp)
		return nil, fmt.Errorf("openai upstream: status %d", resp.StatusCode)
	}
	return resp, nil
}

// chatCompletion runs the full stream:false (buffered) request/response
// cycle: it opens and validates the upstream stream via openChatStream, then
// delegates response-body SSE consumption to the shared parseSSEStream
// helper (internal/openaicompat/stream.go) rather than a second, duplicate
// implementation. Every error path here — connection-level, non-2xx, or an
// in-band SSE error partway through the upstream's own stream — is reported
// before serveChatBuffered has written anything to its client, so it is
// always a pre-response failure from that caller's point of view.
func (c *Client) chatCompletion(ctx context.Context, model string, messages []map[string]any, options map[string]any, onTokens func(int)) (string, int, error) {
	resp, err := c.openChatStream(ctx, model, messages, options)
	if err != nil {
		return "", 0, err
	}
	defer drainClose(resp)
	return parseSSEStream(ctx, resp.Body, onTokens)
}

func (h *handler) serveEmbed(w http.ResponseWriter, r *http.Request) {
	req, err := DecodeEmbedRequest(r.Body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	resp, err := h.client.Embed(r.Context(), req)
	if err != nil {
		// Nothing has been written to w yet, so this is always a
		// pre-response failure.
		writeUpstreamErrorOrFallback(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// writeUpstreamErrorOrFallback is the single call site every pre-response
// failure in this package routes through: it writes the broker's standard
// 503 deferral shape (X-Broker-Status: deferred, Retry-After) via
// proxy.WriteUpstreamError when err is a cancellation or Gate upstreamTimeout
// (context.Canceled/context.DeadlineExceeded), matching what
// internal/proxy/proxy.go's errorHandler does for the ollama backend, or a
// 502 JSON fallback otherwise. proxy.WriteUpstreamError's bool return MUST be
// checked (Design decision #6, docs/openai-compatible-upstream-backend/plan.md)
// — an unchecked false return risks net/http silently defaulting to an empty
// 200 OK when a handler returns without writing anything, turning a real
// upstream failure into a false-success response.
func writeUpstreamErrorOrFallback(w http.ResponseWriter, r *http.Request, err error) {
	if proxy.WriteUpstreamError(w, r, err) {
		return
	}
	writeJSONError(w, http.StatusBadGateway, err.Error())
}

// unsupportedMessageField scans a decoded /api/chat messages array for a
// field this translator cannot forward, returning that field's name and true
// on the first match. Today the only such field is a non-empty "images"
// entry (multimodal/vision input — out of scope per FR-27, AC-20); an absent,
// null, empty-array, or empty-string "images" value is not considered
// present, since Ollama clients may include the key with a zero value even
// when no image was actually attached.
func unsupportedMessageField(messages []map[string]any) (string, bool) {
	for _, msg := range messages {
		if images, ok := msg["images"]; ok && !isEmptyFieldValue(images) {
			return "images", true
		}
	}
	return "", false
}

// isEmptyFieldValue reports whether a decoded JSON value (from
// map[string]any) is JSON null, an empty array, or an empty string — the
// shapes an absent-in-spirit field takes once decoded generically.
func isEmptyFieldValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case []any:
		return len(t) == 0
	case string:
		return t == ""
	default:
		return false
	}
}

// writeJSONError writes a minimal {"error": msg} JSON body with the given
// status.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
