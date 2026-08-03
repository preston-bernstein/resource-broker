// Package proxy provides a transparent HTTP reverse proxy to Ollama that
// relays streaming (NDJSON) responses without buffering.
package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// transport is shared by both proxies. It sets an explicit IdleConnTimeout
// (rather than relying on http.DefaultTransport's 90s default) so a pooled
// outbound connection to Ollama/Infinity is proactively recycled well before
// it can go stale and fail on reuse — see the IdleTimeout comment on
// newServer in cmd/broker/main.go for the failure this defends against on
// the inbound side; this is the same class of fix for the outbound hop.
//
// retryTransport wraps it: a bulk LightRAG embedding run against a shared,
// busy desktop host (also running unrelated automation — Tdarr, Kalshi
// trading bots, periodic timers) occasionally sees a request die mid-flight
// for reasons entirely outside this proxy's control (observed 2026-07-15: a
// burst of unrelated `systemctl daemon-reload` calls from another service on
// the box coincided with an in-flight /api/embed call failing with "server
// disconnected without sending a response"). LightRAG has no retry at its
// storage-flush layer — a single such failure halts its entire pipeline and
// cascades hundreds of in-flight documents to FAILED. /api/embed (and
// /api/generate as used here) are idempotent for our purposes, so retrying a
// connection-level failure once, transparently, before any response bytes
// reach the client, is safe and removes an entire class of upstream-fragile
// cascading failure without LightRAG (or any other consumer) ever knowing.
var transport http.RoundTripper = &retryTransport{
	base: &http.Transport{
		IdleConnTimeout: 60 * time.Second,
	},
	retries: 2,
	backoff: 500 * time.Millisecond,
}

// retryTransport retries a request once (or retries times) when the
// underlying RoundTrip fails with a connection-level error before any
// response was received — never after headers/body have started streaming
// back to the client (httputil.ReverseProxy only calls RoundTrip once per
// request, so a retry here is invisible to the caller either way: it either
// gets the eventual response or the final error, exactly as if this were a
// single slower attempt).
type retryTransport struct {
	base    http.RoundTripper
	retries int
	backoff time.Duration
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Buffer the body once so it can be replayed on retry — Ollama/Infinity
	// request bodies here are small JSON payloads (a handful of texts per
	// embed batch), so buffering is cheap.
	if req.Body != nil && req.GetBody == nil {
		body, err := io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
	}

	var lastErr error
	for attempt := 0; attempt <= t.retries; attempt++ {
		if attempt > 0 {
			if req.Context().Err() != nil {
				return nil, lastErr
			}
			if req.GetBody != nil {
				b, err := req.GetBody()
				if err != nil {
					return nil, lastErr
				}
				req.Body = b
			}
			time.Sleep(t.backoff)
			slog.Warn("retrying upstream request after connection error", "path", req.URL.Path, "attempt", attempt, "err", lastErr)
		}
		resp, err := t.base.RoundTrip(req)
		if err == nil {
			return resp, nil
		}
		if req.Context().Err() != nil || !isRetryableConnError(err) {
			return resp, err
		}
		lastErr = err
	}
	return nil, lastErr
}

// isRetryableConnError reports whether err looks like a connection-level
// failure (reset, dropped, or a stale pooled connection failing on reuse)
// rather than an application-level error that a retry can't help — this
// covers both the io/net-typed errors and net/http's own untyped
// "server disconnected without sending a response" (returned as a plain
// errors.New from net/http/httputil for a response with no status line).
func isRetryableConnError(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "server disconnected") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "EOF")
}

// embedImagePath is the route on the Infinity upstream that performs *image*
// embedding. Infinity's unified OpenAI /embeddings route tokenizes a base64
// data: URI as text (returning a degenerate text-tower vector); image embeds
// must go to /embeddings_image. The embed lane presents an OpenAI /embeddings
// face to consumers (so they keep the stable wire contract) and rewrites to
// this route, leaving the request/response bodies untouched.
const embedImagePath = "/embeddings_image"

// New returns an http.Handler that forwards every request to the Ollama
// upstream at target, preserving path, query, method, body and streaming the
// response back immediately (FlushInterval -1 flushes each write as it arrives,
// which is required for Ollama's chunked NDJSON token stream).
func New(target *url.URL) http.Handler {
	rp := &httputil.ReverseProxy{
		FlushInterval: -1,
		Transport:     transport,
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			// Ollama does not require a specific Host header; match the target.
			r.Out.Host = target.Host
		},
		// Cancelling the upstream context is normal and expected (the broker
		// aborts in-flight calls when yielding, and clients disconnect). Treat
		// cancellation as a quiet debug event; log only genuine upstream errors
		// — and via slog, so it stays in the structured JSON stream rather than
		// the default stderr log line.
		ErrorHandler: errorHandler,
	}
	return rp
}

// NewEmbed returns a reverse proxy to an Infinity image-embedding upstream. It
// behaves like New but rewrites the OpenAI embeddings route to Infinity's
// image-embedding route (see embedImagePath): a request to /embeddings or
// /v1/embeddings is forwarded as /embeddings_image. Any other path (e.g.
// /health, /models) passes through unchanged, so the lane can also be probed.
func NewEmbed(target *url.URL) http.Handler {
	rp := &httputil.ReverseProxy{
		FlushInterval: -1,
		Transport:     transport,
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.Host = target.Host
			if p := r.In.URL.Path; p == "/embeddings" || p == "/v1/embeddings" {
				r.Out.URL.Path = embedImagePath
			}
		},
		ErrorHandler: errorHandler,
	}
	return rp
}

// errorHandler fires only before the response header is written. If the
// upstream was cancelled (yield/disconnect) before any bytes, send 503 so the
// client gets a clear status instead of an empty 200. (Mid-stream cancellation
// never reaches here.) Server-level "superfluous WriteHeader" noise is routed
// to slog via Server.ErrorLog.
func errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) {
		slog.Debug("upstream cancelled", "path", r.URL.Path)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	slog.Warn("upstream error", "path", r.URL.Path, "err", err)
	w.WriteHeader(http.StatusBadGateway)
}
