package queue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/preston-bernstein/resource-broker/internal/httpx"
)

// newRequestID mints an 8-byte hex correlation id for one Synchronous
// request (see CONTEXT.md's Synchronous request entry) end to end through
// admission, the access log, and the response — the same role a Job's id
// already plays on the durable path (internal/job/worker.go), which had one
// and the synchronous path did not (2026-08-01 audit, "correlation"
// finding). crypto/rand costs nothing at Synchronous-request volume and
// avoids any shared-PRNG state across goroutines.
func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is essentially unheard-of on any real OS; if it
		// ever happens, still return something usable rather than a request
		// with no id at all — a zero id is a visible placeholder, not silent.
		return "00000000"
	}
	return hex.EncodeToString(b[:])
}

// Admission decides whether inference may run at all right now. When yielding
// (gaming/Plex active or manual override), requests are refused before they
// reach the upstream. ServeContext is cancelled the instant yielding begins,
// so an in-flight request derives its upstream context from it and aborts.
type Admission interface {
	Yielding() (bool, string)
	ServeContext() context.Context
}

// Recorder tallies request outcomes for metrics. May be nil.
type Recorder interface {
	Record(class, outcome string, wait time.Duration)
	RecordPark(wait time.Duration)
}

// GateConfig is the per-lane policy Gate applies. Wait is how long a request
// may queue for the slot. UpstreamTimeout bounds how long next.ServeHTTP may
// run once the slot is held, separately from Wait. Zero disables the bound —
// the pre-existing behavior, required for the Ollama interactive/batch lanes
// where a legitimate generation can run for minutes. A stuck backend call on a
// lane with no bound holds its slot forever: since MaxInflight/embed-lane
// concurrency is 1, one wedged request permanently starves every request
// behind it (ADR-0013). Callers that pass a nonzero UpstreamTimeout get that
// guarantee; see cmd/broker/main.go's embed-lane wiring for the one lane that
// does. Recorder may be nil.
type GateConfig struct {
	Class           Class
	Wait            time.Duration
	UpstreamTimeout time.Duration
	Admission       Admission
	Recorder        Recorder
}

// Gate wraps next so each request holds the scheduler's single slot for the
// duration of the upstream call, acquired at the given priority class.
//
// It is stateless: a request waits at most cfg.Wait for the slot; if the
// budget is exceeded — or the broker is yielding — it returns 503 with
// Retry-After and the caller is expected to retry. The upstream context is
// derived from both the client request and the serve context, so an in-flight
// call aborts if the client disconnects OR yielding begins mid-flight. Each
// request is tallied (cfg.Recorder) and logged, and carries X-Broker-*
// response headers.
//
// Batch-class requests caught by Yielding() (either on arrival or just after
// acquiring the slot) park instead of an immediate 503, when parking is
// configured (see parkEnabled/parkFor in park.go) — FR-2, FR-6, FR-7. This is
// why admission is a loop: a released parked waiter re-enters admission from
// the top rather than recursing. Interactive requests never park (FR-10):
// class != Batch short-circuits straight to the old immediate-deferRequest
// path in both places Yielding() is checked. See ADR-0009.
func (s *Scheduler) Gate(cfg GateConfig, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g := &gatedRequest{s: s, cfg: cfg, cls: cfg.Class.String(), w: w, r: r, start: time.Now()}
		g.serve(r.Context(), next)
	})
}

// gatedRequest is the state of one request passing through Gate.
type gatedRequest struct {
	s     *Scheduler
	cfg   GateConfig
	cls   string
	w     http.ResponseWriter
	r     *http.Request
	start time.Time
	// reqID correlates admission, the access log, and the response for one
	// Synchronous request end to end — the thing a consumer needed to
	// answer "which request got 503'd" and previously had no way to do
	// (2026-08-01 audit, "correlation" finding; the durable Job path has
	// always had this via j.ID, see internal/job/worker.go).
	reqID     string
	parkWait  time.Duration
	waited    time.Duration
	wasParked bool
}

// admitStep tells the admission loop what to do next.
type admitStep int

const (
	stepProceed admitStep = iota // go on to the next stage
	stepRetry                    // re-enter admission from the top
	stepStop                     // the response is already written
)

func (g *gatedRequest) serve(ctx context.Context, next http.Handler) {
	g.reqID = newRequestID()
	g.w.Header().Set("X-Broker-Request-Id", g.reqID)

	// A CPU-only model never touches the GPU, so it takes no slot and
	// ignores a GPU yield (ADR-0018). The body is peeked only when CPU
	// models are configured; otherwise this is the exact old path.
	if g.s.cpuModels != nil && g.s.isCPUModel(httpx.RequestModel(g.r)) {
		g.serveCPU(ctx, next)
		return
	}
	if !g.admit(ctx) {
		return
	}
	defer g.s.Release()
	g.forward(ctx, next)
}

// canPark reports whether a yielding request may park instead of getting an
// immediate 503: batch class only (FR-10), and only when parking is on.
func (g *gatedRequest) canPark() bool {
	return g.cfg.Class == Batch && g.s.parkEnabled()
}

// admit loops until the request holds the slot (true) or has been answered
// (false).
func (g *gatedRequest) admit(ctx context.Context) bool {
	for {
		if yielding, reason := g.cfg.Admission.Yielding(); yielding {
			switch g.onYieldingArrival(ctx, reason) {
			case stepRetry:
				continue
			case stepStop:
				return false
			}
		}
		switch g.acquireSlot(ctx) {
		case stepRetry:
			continue
		case stepStop:
			return false
		}
		return true
	}
}

// onYieldingArrival handles a request that arrives while the broker yields:
// park it when allowed, otherwise defer it with a 503.
func (g *gatedRequest) onYieldingArrival(ctx context.Context, reason string) admitStep {
	if !g.canPark() { // FR-10 + kill-switch/never-configured
		g.deferRequest("deferred", "deferred", "yielding GPU: "+reason, time.Since(g.start))
		return stepStop
	}
	t0 := time.Now()
	switch g.s.parkFor(ctx) {
	case parkReleased:
		g.wasParked = true
		g.parkWait += time.Since(t0)
		return stepRetry // FR-6/FR-7: re-enter admission from the top
	case parkExpired:
		g.deferRequest("deferred", "expired", "park hold exceeded", time.Since(g.start))
	case parkRejected:
		g.deferRequest("deferred", "park_rejected", "park queue full", 0)
	case parkCanceled:
		record(g.cfg.Recorder, g.cls, "canceled", time.Since(g.start)) // FR-8: no response write, client is gone
	case parkShutdown:
		g.deferRequest("crash_failed", "crash_failed", "broker shutting down", time.Since(g.start))
	default:
		return stepProceed
	}
	return stepStop
}

// acquireSlot takes the scheduler slot within the wait budget. A yield that
// began while waiting releases it again and parks or defers the request.
func (g *gatedRequest) acquireSlot(ctx context.Context) admitStep {
	actx, acancel := context.WithTimeout(ctx, g.cfg.Wait)
	err := g.s.Acquire(actx, g.cfg.Class)
	acancel()
	g.waited = time.Since(g.start)
	if err != nil {
		g.deferRequest("deferred", "deferred", "GPU busy: wait budget exceeded", g.waited)
		return stepStop
	}
	if yielding, _ := g.cfg.Admission.Yielding(); !yielding {
		return stepProceed
	}
	g.s.Release() // never hold the slot through a park (NFR-3's spirit)
	if !g.canPark() {
		g.deferRequest("deferred", "deferred", "yielding GPU", g.waited)
		return stepStop
	}
	return stepRetry // FR-2: park instead of the old immediate deferRequest
}

// forward runs next while holding the slot, then records the outcome.
func (g *gatedRequest) forward(reqCtx context.Context, next http.Handler) {
	var ctx context.Context
	var cancel context.CancelFunc
	if g.cfg.UpstreamTimeout > 0 {
		ctx, cancel = context.WithTimeout(reqCtx, g.cfg.UpstreamTimeout)
	} else {
		ctx, cancel = context.WithCancel(reqCtx)
	}
	defer cancel()
	serveDone := g.cfg.Admission.ServeContext().Done()
	go func() {
		select {
		case <-serveDone:
			cancel() // yielding began: abort the in-flight upstream call
		case <-ctx.Done():
		}
	}()

	g.setServedHeaders(g.waited)
	next.ServeHTTP(g.w, g.r.WithContext(ctx))

	outcome := forwardOutcome(ctx, serveDone)
	g.w.Header().Set(http.TrailerPrefix+"X-Broker-Status", outcome)
	if g.wasParked && outcome == "served" {
		recordPark(g.cfg.Recorder, g.parkWait)
	}
	record(g.cfg.Recorder, g.cls, outcome, g.waited)
	slog.Info("request", "req_id", g.reqID, "class", g.cls, "outcome", outcome,
		"path", g.r.URL.Path, "method", g.r.Method, "wait_ms", g.waited.Milliseconds())
}

// setServedHeaders sets the optimistic response headers before the upstream
// writes, with waited as the time spent queued for the slot.
func (g *gatedRequest) setServedHeaders(waited time.Duration) {
	// Headers must be set before the upstream writes them, so X-Broker-Status
	// starts optimistic ("served"). For a streamed (chunked) response that is
	// later preempted mid-flight we can't change that header — so we also emit
	// the *authoritative* final outcome as a trailer via TrailerPrefix, which
	// is delivered on chunked responses. (Non-streamed responses can't carry
	// trailers, but a non-streamed request that is preempted fails before any
	// body and surfaces as a 503 instead, so the header is never wrong there.)
	h := g.w.Header()
	h.Set("X-Broker-Status", "served")
	h.Set("X-Broker-Wait-Ms", strconv.FormatInt(waited.Milliseconds(), 10))
	h.Set(http.TrailerPrefix+"X-Broker-Status", "served")
}

// forwardOutcome is the final outcome for metrics, logs and the trailer, read
// deterministically from the serve state rather than a raced flag. A yield
// preemption and an upstream-timeout both cancel ctx, so serveDone is checked
// first: contention is the higher-priority explanation when both are possible
// (e.g. yield began just as the bound also would have fired) and is what
// operators need to see in the outcome label.
func forwardOutcome(ctx context.Context, serveDone <-chan struct{}) string {
	select {
	case <-serveDone:
		return "preempted"
	default:
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "upstream_timeout"
	}
	return "served"
}

// serveCPU forwards a CPU-only model's request straight to next, with no
// slot and no yield check (ADR-0018). The client's disconnect still cancels
// it, and a lane's UpstreamTimeout still bounds it. It is recorded as
// "served" so dashboards keep one success label; the log line's cpu_model
// field tells the two paths apart.
func (g *gatedRequest) serveCPU(ctx context.Context, next http.Handler) {
	if g.cfg.UpstreamTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, g.cfg.UpstreamTimeout)
		defer cancel()
	}
	h := g.w.Header()
	h.Set("X-Broker-Status", "served")
	h.Set("X-Broker-Wait-Ms", "0")
	h.Set(http.TrailerPrefix+"X-Broker-Status", "served")
	next.ServeHTTP(g.w, g.r.WithContext(ctx))

	outcome := "served"
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		outcome = "upstream_timeout"
	}
	h.Set(http.TrailerPrefix+"X-Broker-Status", outcome)
	record(g.cfg.Recorder, g.cls, outcome, 0)
	slog.Info("request", "req_id", g.reqID, "class", g.cls, "outcome", outcome, "cpu_model", true,
		"path", g.r.URL.Path, "method", g.r.Method, "wait_ms", 0)
}

// deferRequest writes a 503 with Retry-After and records the outcome.
// status is the X-Broker-Status header value; outcome is the metrics tag.
// They differ for "expired" (status "deferred") and "crash_failed" (status
// and outcome both "crash_failed") — see the API contract in ADR-0009.
// Retry-After is the lane's wait budget.
func (g *gatedRequest) deferRequest(status, outcome, reason string, waited time.Duration) {
	secs := int(g.cfg.Wait.Round(time.Second) / time.Second)
	if secs < 1 {
		secs = 1
	}
	g.w.Header().Set("Retry-After", strconv.Itoa(secs))
	g.w.Header().Set("X-Broker-Status", status)
	http.Error(g.w, "broker: "+reason, http.StatusServiceUnavailable)
	record(g.cfg.Recorder, g.cls, outcome, waited)
	slog.Info("request", "req_id", g.reqID, "class", g.cls, "outcome", outcome, "reason", reason,
		"path", g.r.URL.Path, "method", g.r.Method, "wait_ms", waited.Milliseconds())
}

func record(rec Recorder, class, outcome string, wait time.Duration) {
	if rec != nil {
		rec.Record(class, outcome, wait)
	}
}

// recordPark tallies wait time for a request that was parked and eventually
// served (Gate calls this only when wasParked && outcome == "served").
func recordPark(rec Recorder, wait time.Duration) {
	if rec != nil {
		rec.RecordPark(wait)
	}
}
