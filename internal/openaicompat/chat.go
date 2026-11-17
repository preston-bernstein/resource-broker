package openaicompat

import (
	"context"
	"fmt"
	"net/http"
)

// chatRequest is one translated upstream chat/completions request: a model, an
// arbitrary messages array and the caller's pass-through options.
type chatRequest struct {
	Model    string
	Messages []map[string]any
	Options  map[string]any
}

// body is the chat/completions JSON body. It always requests stream:true from
// the upstream regardless of the Consumer's own stream preference.
func (r chatRequest) body() map[string]any {
	body := map[string]any{
		"model":          r.Model,
		"messages":       r.Messages,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	for k, v := range r.Options {
		// Never let caller options override the fields this method controls.
		if k == "model" || k == "messages" || k == "stream" || k == "stream_options" {
			continue
		}
		body[k] = v
	}
	return body
}

// openChatStream builds and sends the outbound POST {base}/v1/chat/completions
// request for an arbitrary messages array and validates the upstream's
// response — a connection-level error or a non-2xx status — before returning.
// Splitting this out from response-body consumption lets callers
// (serveChatStreaming) distinguish a pre-response failure (nothing written to
// the original client yet, so proxy.WriteUpstreamError's 503/502 shape
// applies) from a genuine mid-stream failure, which can only be surfaced
// in-band once bytes have already been relayed. The caller decides whether to
// relay chunks incrementally or buffer them (see serveChatStreaming/
// serveChatBuffered).
//
// On success, the caller owns the returned *http.Response and must eventually
// drainClose it. On error the response (if any) has already been drained and
// closed here.
func (c *Client) openChatStream(ctx context.Context, req chatRequest) (*http.Response, error) {
	resp, err := c.postJSON(ctx, req.body(), "v1", "chat", "completions")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		drainClose(resp)
		return nil, fmt.Errorf("openai upstream: status %d", resp.StatusCode)
	}
	return resp, nil
}

// chatCompletion runs the full buffered request/response cycle: it opens and
// validates the upstream stream via openChatStream, then delegates
// response-body SSE consumption to the shared parseSSEStream helper
// (internal/openaicompat/stream.go) rather than a second, duplicate
// implementation. Every error path here — connection-level, non-2xx, or an
// in-band SSE error partway through the upstream's own stream — is reported
// before a caller has written anything to its client, so it is always a
// pre-response failure from that caller's point of view.
func (c *Client) chatCompletion(ctx context.Context, req chatRequest, onTokens func(int)) (string, int, error) {
	resp, err := c.openChatStream(ctx, req)
	if err != nil {
		return "", 0, err
	}
	defer drainClose(resp)
	return parseSSEStream(ctx, resp.Body, onTokens)
}
