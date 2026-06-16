// Package proxy provides a transparent HTTP reverse proxy to Ollama that
// relays streaming (NDJSON) responses without buffering.
package proxy

import (
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
			// Preserve the original Host-independent client semantics; Ollama
			// does not require a specific Host header, so set it to the target.
			r.Out.Host = target.Host
		},
	}
	return rp
}
