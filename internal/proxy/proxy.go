// Package proxy provides a transparent HTTP reverse proxy to Ollama that
// relays streaming (NDJSON) responses without buffering.
package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
)

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
