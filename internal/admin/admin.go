// Package admin exposes the broker's control plane: manual yield override,
// status, metrics, and health — on a listener separate from the proxied
// Ollama ports.
package admin

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/preston-bernstein/resource-broker/internal/httpx"
	"github.com/preston-bernstein/resource-broker/internal/queue"
	"github.com/preston-bernstein/resource-broker/internal/schedule"
	"github.com/preston-bernstein/resource-broker/internal/yield"
)

// HealthCheck reports whether the broker can actually do its job right now —
// not just whether the process is up. A non-nil error names the failed
// dependency (Ollama upstream, the job store, the detector loop) and becomes
// the /healthz response body. See ADR-0010: before this, /healthz was a
// hardcoded "ok" that could never fail, the same class of defect as a
// watchdog whose probe always returns success.
type HealthCheck func(ctx context.Context) error

// Controller is the yield control surface the admin API drives.
type Controller interface {
	SetMode(yield.Mode)
	Snapshot() yield.State
}

// StatsProvider exposes live scheduler occupancy.
type StatsProvider interface {
	Stats() queue.Stats
}

// TdarrStatus is an optional snapshot of Tdarr GPU worker state included in /status.
type TdarrStatus struct {
	GPUWorkers int  `json:"gpu_workers"`
	Managed    bool `json:"managed"`
}

// TdarrStatusFn returns the current Tdarr GPU worker count (nil = disabled).
type TdarrStatusFn func(ctx context.Context) *TdarrStatus

// Options carries everything Mux wires together. Jobs is the durable Job API
// surface (nil if disabled). JobStatus, if non-nil, contributes a "jobs"
// section to /status; TdarrStatus, if non-nil, contributes a "tdarr" section;
// RoutingStatus, if non-nil, contributes a "routing" section (per-model
// backend routing, docs/adr/0015-per-model-backend-routing.md) — nil (the
// default, when no BROKER_ROUTE_<N>_* config is set) omits the key entirely,
// preserving /status's pre-feature shape byte-for-byte; IdleStatus, if non-nil,
// contributes an "idle" section (idle-unload state per yield.Controller) — nil
// (the default, when no idle timeouts are configured) omits the key entirely.
// Health, if non-nil, backs /healthz with a real readiness check (ADR-0010);
// nil preserves the old always-200 behavior (used by tests that don't care).
//
// ControlToken gates POST /control (ADR-0005): GET /metrics, /healthz,
// /status, and GET /control stay open to any LAN caller (Grafana must scrape
// /metrics, and that data is low-sensitivity). POST /control mutates broker
// state, so it is gated: if ControlToken is non-empty, the request must carry
// a matching "Authorization: Bearer <token>" header; if ControlToken is
// empty, mutations are accepted only from a loopback remote address. Either
// way, an unauthorized POST /control gets 401.
type Options struct {
	Controller    Controller
	Stats         StatsProvider
	Health        HealthCheck
	Metrics       http.Handler
	Jobs          http.Handler
	JobStatus     func(ctx context.Context) any
	TdarrStatus   TdarrStatusFn
	RoutingStatus func() any
	IdleStatus    func() any
	ControlToken  string
}

// Mux builds the control-plane handler.
func Mux(o Options) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthzHandler(o.Health))
	mux.Handle("/metrics", o.Metrics)
	if o.Jobs != nil {
		mux.Handle("/jobs", o.Jobs)
		mux.Handle("/jobs/", o.Jobs)
	}
	mux.HandleFunc("/control", controlHandler(o.Controller, o.ControlToken))
	mux.HandleFunc("/status", statusHandler(o))
	return mux
}

func healthzHandler(health HealthCheck) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if health == nil {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("ok\n"))
			return
		}
		// Bounded independently of any caller timeout: a hung dependency check
		// must not hang the health probe itself past a few seconds.
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := health(ctx); err != nil {
			httpx.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{
				"status": "unhealthy",
				"error":  err.Error(),
			})
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

// controlHandler serves
//
//	GET  /control -> current state
//	POST /control {"mode":"auto|yield|serve"} -> set override, return state
func controlHandler(ctrl Controller, controlToken string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			httpx.WriteJSON(w, http.StatusOK, ctrl.Snapshot())
		case http.MethodPost:
			setControlMode(w, r, ctrl, controlToken)
		default:
			w.Header().Set("Allow", "GET, POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

func setControlMode(w http.ResponseWriter, r *http.Request, ctrl Controller, controlToken string) {
	if !authorized(r, controlToken) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="control"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
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
	httpx.WriteJSON(w, http.StatusOK, ctrl.Snapshot())
}

func statusHandler(o Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st := o.Stats.Stats()
		out := map[string]any{
			"yield": o.Controller.Snapshot(),
			"queue": map[string]any{
				"busy":         st.Busy,
				"inflight":     st.Inflight,
				"max_inflight": st.MaxInflight,
				"interactive":  st.Interactive,
				"batch":        st.Batch,
				"parked":       st.Parked,
			},
			"schedule": schedule.TakeSnapshot(time.Now()),
		}
		if o.JobStatus != nil {
			out["jobs"] = o.JobStatus(r.Context())
		}
		if o.TdarrStatus != nil {
			if ts := o.TdarrStatus(r.Context()); ts != nil {
				out["tdarr"] = ts
			}
		}
		addOptionalSection(out, "routing", o.RoutingStatus)
		addOptionalSection(out, "idle", o.IdleStatus)
		httpx.WriteJSON(w, http.StatusOK, out)
	}
}

// addOptionalSection adds fn's result under key. A nil fn omits the key.
func addOptionalSection(out map[string]any, key string, fn func() any) {
	if fn != nil {
		out[key] = fn()
	}
}

// authorized implements ADR-0005's POST /control gate: with a configured
// token, only a matching "Authorization: Bearer <token>" header passes;
// without one, only a loopback remote address passes (zero-config-safe
// default so SSH-local control keeps working untouched).
func authorized(r *http.Request, controlToken string) bool {
	if controlToken == "" {
		return isLoopback(r.RemoteAddr)
	}
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, prefix) {
		return false
	}
	got := strings.TrimPrefix(auth, prefix)
	return subtle.ConstantTimeCompare([]byte(got), []byte(controlToken)) == 1
}

// isLoopback reports whether remoteAddr (host:port, as seen on http.Request)
// is a loopback address.
func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
