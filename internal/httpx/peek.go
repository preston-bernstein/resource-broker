package httpx

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

// peekLimit bounds how much of an inbound request body is buffered while
// looking for the "model" field. Inbound vision requests can carry
// base64-encoded images tens of MB in size (this repo has already been
// burned once by an unbounded io.ReadAll assumption — see
// internal/proxy/proxy.go's retryTransport comment). 64KB comfortably covers
// a "model" field appearing anywhere near the front of realistic JSON request
// bodies (Ollama/OpenAI-compatible chat/generate/embed payloads put "model"
// among the first few fields) without ever buffering the large parts of a
// payload (prompt text, base64 images) that follow it.
const peekLimit = 64 * 1024

// RequestModel returns the top-level "model" field of req's JSON body, or ""
// when there is no body, the body is not a JSON object, or the field is not
// found within peekLimit bytes. The body is always restored, so the handler
// that serves req sees it byte-identical to what the client sent.
//
// Shared by backend.Router (per-model routing, ADR-0015) and queue.Gate
// (CPU-only models skip the GPU slot, ADR-0018).
func RequestModel(req *http.Request) string {
	if req.Body == nil || req.Body == http.NoBody {
		return ""
	}
	model, consumed := PeekModel(req.Body)
	// Consumed bytes (whatever was read, even when no model was found)
	// followed by whatever remains unread: no bytes dropped or duplicated.
	req.Body = io.NopCloser(io.MultiReader(bytes.NewReader(consumed), req.Body))
	return model
}

// PeekModel reads at most peekLimit bytes from body via a streaming
// json.Decoder, stopping as soon as it has decoded the top-level "model"
// field (whatever comes after — including multi-MB base64 image data in a
// vision request's "images" field — is never touched). It returns the model
// name found (empty if none), and the raw bytes consumed from body so the
// caller can restore them ahead of the remaining unread body.
//
// PeekModel deliberately does not buffer the whole body: it wraps body in an
// io.LimitReader capped at peekLimit and additionally tees every byte the
// decoder actually consumes into a buffer, so "consumed" reflects exactly
// what the decoder read — no more.
func PeekModel(body io.Reader) (model string, consumed []byte) {
	var buf bytes.Buffer
	limited := io.LimitReader(body, peekLimit)
	teed := io.TeeReader(limited, &buf)

	dec := json.NewDecoder(teed)
	tok, err := dec.Token() // expect '{'
	if err != nil {
		return "", buf.Bytes() // not JSON (or empty)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return "", buf.Bytes()
	}

	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return "", buf.Bytes()
		}
		key, ok := keyTok.(string)
		if !ok {
			return "", buf.Bytes()
		}
		if key == "model" {
			var val string
			if err := dec.Decode(&val); err != nil {
				return "", buf.Bytes()
			}
			return val, buf.Bytes()
		}
		// Skip this field's value without caring about its shape.
		var discard json.RawMessage
		if err := dec.Decode(&discard); err != nil {
			return "", buf.Bytes()
		}
	}
	// End of object (or peekLimit) without finding "model".
	return "", buf.Bytes()
}
