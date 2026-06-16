// Package admin exposes the broker's control plane: manual yield override,
// status, metrics, and health — on a listener separate from the proxied
// Ollama ports.
package admin

import (
	"encoding/json"
	"net/http"

	"github.com/preston-bernstein/ollama-resource-broker/internal/queue"
	"github.com/preston-bernstein/ollama-resource-broker/internal/yield"
)

// Controller is the yield control surface the admin API drives.
type Controller interface {
	SetMode(yield.Mode)
	Snapshot() yield.State
}

// StatsProvider exposes live scheduler occupancy.
type StatsProvider interface {
	Stats() queue.Stats
}

// Mux builds the control-plane handler. jobs is the durable Job API surface
// (may be nil if disabled); jobStatus, if non-nil, contributes a "jobs" section
// to /status.
func Mux(ctrl Controller, stats StatsProvider, metricsHandler http.Handler, jobs http.Handler, jobStatus func() any) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("ok\n"))
	})

	mux.Handle("/metrics", metricsHandler)

	if jobs != nil {
		mux.Handle("/jobs", jobs)
		mux.Handle("/jobs/", jobs)
	}

	// GET  /control -> current state
	// POST /control {"mode":"auto|yield|serve"} -> set override, return state
	mux.HandleFunc("/control", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, ctrl.Snapshot())
		case http.MethodPost:
			var body struct {
				Mode string `json:"mode"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "invalid JSON body", http.StatusBadRequest)
				return
			}
			m, ok := yield.ParseMode(body.Mode)
			if !ok {
				http.Error(w, "mode must be one of: auto, yield, serve", http.StatusBadRequest)
				return
			}
			ctrl.SetMode(m)
			writeJSON(w, http.StatusOK, ctrl.Snapshot())
		default:
			w.Header().Set("Allow", "GET, POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		st := stats.Stats()
		out := map[string]any{
			"yield": ctrl.Snapshot(),
			"queue": map[string]any{
				"busy":         st.Busy,
				"inflight":     st.Inflight,
				"max_inflight": st.MaxInflight,
				"interactive":  st.Interactive,
				"batch":        st.Batch,
			},
		}
		if jobStatus != nil {
			out["jobs"] = jobStatus()
		}
		writeJSON(w, http.StatusOK, out)
	})

	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
