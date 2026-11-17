package openaicompat

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// sseChunk is the OpenAI chat-completions streaming chunk shape. Only the
// fields this client cares about are decoded; everything else is ignored.
type sseChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// parseSSEStream reads an OpenAI-compatible chat-completions SSE response
// body, accumulating the streamed text and calling onTokens (if non-nil)
// with the running token count as each chunk arrives. It returns the full
// accumulated text and the final token count.
//
// This is a thin wrapper around parseSSEStreamChunks for callers (the Job
// path's Client.Generate, and this file's own tests) that only need the
// running count, not each chunk's delta text — see parseSSEStreamChunks'
// doc comment for why the two are split.
//
// Token counting prefers the `usage.completion_tokens` field carried on the
// final chunk; many upstreams (vLLM by default, even with
// stream_options.include_usage requested) omit `usage` entirely, in which
// case a running per-chunk counter is used instead.
//
// Mirrors ollama.Client.Generate's error handling: an in-band
// `{"error":...}` payload and a stream that ends without a `[DONE]`
// sentinel (and without ever having seen an OpenAI-style [DONE]) are both
// reported as errors rather than a silently-truncated success.
func parseSSEStream(ctx context.Context, body io.Reader, onTokens func(int)) (string, int, error) {
	return parseSSEStreamChunks(ctx, body, func(_ string, total int) {
		if onTokens != nil {
			onTokens(total)
		}
	})
}

// parseSSEStreamChunks is parseSSEStream's underlying implementation: it
// additionally reports each chunk's actual delta text (not just the running
// count) via onChunk, so a caller that relays content live to its own
// client — internal/openaicompat/handler.go's serveChatStreaming — can send
// the real incremental text per NDJSON line, matching Ollama's own streaming
// convention (each intermediate line carries its slice of the response, not
// an empty placeholder) rather than a content-free progress signal.
func parseSSEStreamChunks(ctx context.Context, body io.Reader, onChunk func(delta string, total int)) (string, int, error) {
	r := &sseReader{usageTokens: -1, onChunk: onChunk}

	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20) // tolerate large single SSE chunks (default ~64KB is too small)

	for sc.Scan() {
		// A caller-side context cancellation doesn't necessarily tear down
		// the underlying connection instantly — sc.Scan() can still return
		// an already-buffered event that arrived before cancellation. Without
		// this check, that event would still reach onChunk/onTokens after
		// the caller has already moved on from a canceled stream (2026-08-15:
		// this is exactly what made TestGenerate_ContextCancellationMidStream
		// flaky under CI's slower/busier runners — the mock's post-pause
		// write could still be scanned before the connection actually broke).
		if err := ctx.Err(); err != nil {
			return "", r.tokens, fmt.Errorf("openai upstream: %w", err)
		}
		trimmed := strings.TrimSpace(sc.Text())
		if trimmed != "" {
			// Ignore non-data SSE fields (event:, id:, comments, etc.).
			if data, ok := cutPrefix(trimmed, "data:"); ok {
				r.dataLines = append(r.dataLines, data)
			}
			continue
		}
		// Blank line: event boundary.
		isDone, err := r.flush()
		if err != nil {
			return "", r.tokens, err
		}
		if isDone {
			return r.success()
		}
	}
	return r.finish(ctx, sc.Err())
}

// sseReader accumulates one SSE response: the text so far, the running token
// count, the usage count if the upstream sent one, and the data lines of the
// event being read.
type sseReader struct {
	sb          strings.Builder
	tokens      int
	usageTokens int // -1 means "no usage field seen"
	dataLines   []string
	onChunk     func(delta string, total int)
}

// flush decodes the buffered event. It reports whether that event was the
// [DONE] sentinel.
func (r *sseReader) flush() (bool, error) {
	if len(r.dataLines) == 0 {
		return false, nil
	}
	data := strings.Join(r.dataLines, "\n")
	r.dataLines = r.dataLines[:0]

	if data == "[DONE]" {
		return true, nil
	}

	var chunk sseChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return false, fmt.Errorf("decode SSE chunk: %w", err)
	}
	if chunk.Error != nil {
		msg := chunk.Error.Message
		if msg == "" {
			msg = "unknown error"
		}
		return false, fmt.Errorf("openai upstream: %s", msg)
	}
	r.apply(chunk)
	return false, nil
}

// apply adds one decoded chunk's delta text and usage to the totals.
func (r *sseReader) apply(chunk sseChunk) {
	if len(chunk.Choices) > 0 {
		if content := chunk.Choices[0].Delta.Content; content != "" {
			r.sb.WriteString(content)
			r.tokens++
			if r.onChunk != nil {
				r.onChunk(content, r.tokens)
			}
		}
	}
	if chunk.Usage != nil {
		r.usageTokens = chunk.Usage.CompletionTokens
	}
}

// success returns the accumulated text and the final token count: the
// upstream's usage count when it sent one, else the running count.
func (r *sseReader) success() (string, int, error) {
	if r.usageTokens >= 0 {
		return r.sb.String(), r.usageTokens, nil
	}
	return r.sb.String(), r.tokens, nil
}

// finish ends a stream whose scanner stopped without a blank line + [DONE]:
// scanErr is the scanner's own error, if any.
func (r *sseReader) finish(ctx context.Context, scanErr error) (string, int, error) {
	if scanErr != nil {
		if ctx.Err() != nil {
			return "", r.tokens, fmt.Errorf("openai upstream: %w", ctx.Err())
		}
		return "", r.tokens, fmt.Errorf("read SSE stream: %w", scanErr)
	}

	// Flush any trailing event that wasn't followed by a final blank line.
	isDone, err := r.flush()
	if err != nil {
		return "", r.tokens, err
	}
	if isDone {
		return r.success()
	}

	// Stream ended without a [DONE] sentinel (e.g. upstream closed
	// abruptly) — treat as an error rather than a silently-truncated
	// success, mirroring ollama.Client.Generate's "stream ended without
	// done" guard.
	if ctx.Err() != nil {
		return "", r.tokens, fmt.Errorf("openai upstream: %w", ctx.Err())
	}
	return "", r.tokens, fmt.Errorf("openai upstream: stream ended without done")
}

// cutPrefix trims an SSE field prefix (e.g. "data:") from s and returns the
// remainder with exactly one leading space stripped, per the SSE spec (a
// single space after the colon is part of the field syntax, not the value).
func cutPrefix(s, prefix string) (string, bool) {
	if !strings.HasPrefix(s, prefix) {
		return "", false
	}
	rest := s[len(prefix):]
	rest = strings.TrimPrefix(rest, " ")
	return rest, true
}
