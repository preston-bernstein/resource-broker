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
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// ErrorHandler fires only before the response header is written. If
			// the upstream was cancelled (yield/disconnect) before any bytes,
			// send 503 so the client gets a clear status instead of an empty
			// 200. (Mid-stream cancellation never reaches here.) Server-level
			// "superfluous WriteHeader" noise is routed to slog via Server.ErrorLog.
			if errors.Is(err, context.Canceled) {
				slog.Debug("upstream cancelled", "path", r.URL.Path)
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			slog.Warn("upstream error", "path", r.URL.Path, "err", err)
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	return rp
}
