// Package httpx holds small HTTP response helpers shared by the broker's
// control-plane API (internal/admin) and durable Job API (internal/job).
// Both surfaces wrote an identical writeJSON helper independently; this is
// the single copy.
package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// WriteJSON writes v as a JSON response with the given status code. An
// encode failure here is not a client problem (headers and status are
// already sent) — it means v itself could not be marshaled, which is a
// programming bug. Logging it, rather than dropping it silently, is what
// makes that bug visible instead of just an unexpectedly-truncated response
// body on the wire.
func WriteJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("httpx: encode json response failed", "err", err)
	}
}
