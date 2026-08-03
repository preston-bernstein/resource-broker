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

// Gate wraps next so each request holds the scheduler's single slot for the
// duration of the upstream call, acquired at the given priority class.
//
// It is stateless: a request waits at most `wait` for the slot; if the budget
// is exceeded — or the broker is yielding — it returns 503 with Retry-After
// and the caller is expected to retry. The upstream context is derived from
// both the client request and the serve context, so an in-flight call aborts
// if the client disconnects OR yielding begins mid-flight. Each request is
// tallied (rec) and logged, and carries X-Broker-* response headers.
//
// Batch-class requests caught by Yielding() (either on arrival or just after
// acquiring the slot) park instead of an immediate 503, when parking is
// configured (see parkEnabled/parkFor in park.go) — FR-2, FR-6, FR-7. This is
// why admission is a loop: a released parked waiter re-enters admission from
// the top rather than recursing. Interactive requests never park (FR-10):
// class != Batch short-circuits straight to the old immediate-deferRequest
// path in both places Yielding() is checked below. See ADR-0009.
//
// upstreamTimeout bounds how long next.ServeHTTP may run once the slot is
// held, separately from wait (which only bounds how long a request may
// queue FOR the slot). Zero disables the bound — the pre-existing behavior,
// required for the Ollama interactive/batch lanes where a legitimate
// generation can run for minutes. A stuck backend call on a lane with no
// bound holds its slot forever: since MaxInflight/embed-lane concurrency is
// 1, one wedged request permanently starves every request behind it (ADR-
// 0013). Callers that pass a nonzero upstreamTimeout get that guarantee;
// see cmd/broker/main.go's embed-lane wiring for the one lane that does.
func (s *Scheduler) Gate(class Class, wait, upstreamTimeout time.Duration, adm Admission, rec Recorder, next http.Handler) http.Handler {
	cls := class.String()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		var parkWait time.Duration
		var waited time.Duration
		wasParked := false

		// reqID correlates admission, the access log, and the response for one
		// Synchronous request end to end — the thing a consumer needed to
		// answer "which request got 503'd" and previously had no way to do
		// (2026-08-01 audit, "correlation" finding; the durable Job path has
		// always had this via j.ID, see internal/job/worker.go).
		reqID := newRequestID()
		w.Header().Set("X-Broker-Request-Id", reqID)

		for {
			if yielding, reason := adm.Yielding(); yielding {
				if class != Batch || !s.parkEnabled() { // FR-10 + kill-switch/never-configured
					deferRequest(w, r, rec, reqID, cls, "deferred", "deferred", "yielding GPU: "+reason, wait, time.Since(start))
					return
				}
				t0 := time.Now()
				switch res := s.parkFor(r.Context()); res {
				case parkReleased:
					wasParked = true
					parkWait += time.Since(t0)
					continue // FR-6/FR-7: re-enter admission from the top
				case parkExpired:
					deferRequest(w, r, rec, reqID, cls, "deferred", "expired", "park hold exceeded", wait, time.Since(start))
					return
				case parkRejected:
					deferRequest(w, r, rec, reqID, cls, "deferred", "park_rejected", "park queue full", wait, 0)
					return
				case parkCanceled:
					record(rec, cls, "canceled", time.Since(start)) // FR-8: no response write, client is gone
					return
				case parkShutdown:
					deferRequest(w, r, rec, reqID, cls, "crash_failed", "crash_failed", "broker shutting down", wait, time.Since(start))
					return
				}
			}

			actx, acancel := context.WithTimeout(r.Context(), wait)
			err := s.Acquire(actx, class)
			acancel()
			waited = time.Since(start)
			if err != nil {
				deferRequest(w, r, rec, reqID, cls, "deferred", "deferred", "GPU busy: wait budget exceeded", wait, waited)
				return
			}

			if yielding, _ := adm.Yielding(); yielding {
				s.Release() // never hold the slot through a park (NFR-3's spirit)
				if class != Batch || !s.parkEnabled() {
					deferRequest(w, r, rec, reqID, cls, "deferred", "deferred", "yielding GPU", wait, waited)
					return
				}
				continue // FR-2: park instead of the old immediate deferRequest
			}
			defer s.Release()
			break
		}

		var ctx context.Context
		var cancel context.CancelFunc
		if upstreamTimeout > 0 {
			ctx, cancel = context.WithTimeout(r.Context(), upstreamTimeout)
		} else {
			ctx, cancel = context.WithCancel(r.Context())
		}
		defer cancel()
		serveDone := adm.ServeContext().Done()
		go func() {
			select {
			case <-serveDone:
				cancel() // yielding began: abort the in-flight upstream call
			case <-ctx.Done():
			}
		}()

		// Headers must be set before the upstream writes them, so X-Broker-Status
		// starts optimistic ("served"). For a streamed (chunked) response that is
		// later preempted mid-flight we can't change that header — so we also emit
		// the *authoritative* final outcome as a trailer via TrailerPrefix, which
		// is delivered on chunked responses. (Non-streamed responses can't carry
		// trailers, but a non-streamed request that is preempted fails before any
		// body and surfaces as a 503 instead, so the header is never wrong there.)
		w.Header().Set("X-Broker-Status", "served")
		w.Header().Set("X-Broker-Wait-Ms", strconv.FormatInt(waited.Milliseconds(), 10))
		w.Header().Set(http.TrailerPrefix+"X-Broker-Status", "served")
		next.ServeHTTP(w, r.WithContext(ctx))

		// Final outcome for metrics/logs/trailer, read deterministically from the
		// serve state rather than a raced flag. A yield preemption and an
		// upstream-timeout both cancel ctx, so serveDone is checked first:
		// contention is the higher-priority explanation when both are
		// possible (e.g. yield began just as the bound also would have
		// fired) and is what operators need to see in the outcome label.
		outcome := "served"
		select {
		case <-serveDone:
			outcome = "preempted"
		default:
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				outcome = "upstream_timeout"
			}
		}
		w.Header().Set(http.TrailerPrefix+"X-Broker-Status", outcome)
		if wasParked && outcome == "served" {
			recordPark(rec, parkWait)
		}
		record(rec, cls, outcome, waited)
		slog.Info("request", "req_id", reqID, "class", cls, "outcome", outcome,
			"path", r.URL.Path, "method", r.Method, "wait_ms", waited.Milliseconds())
	})
}

// deferRequest writes a 503 with Retry-After and records the outcome.
// status is the X-Broker-Status header value; outcome is the metrics tag.
// They differ for "expired" (status "deferred") and "crash_failed" (status
// and outcome both "crash_failed") — see the API contract in ADR-0009.
func deferRequest(w http.ResponseWriter, r *http.Request, rec Recorder, reqID, class, status, outcome, reason string, retryAfter, waited time.Duration) {
	secs := int(retryAfter.Round(time.Second) / time.Second)
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	w.Header().Set("X-Broker-Status", status)
	http.Error(w, "broker: "+reason, http.StatusServiceUnavailable)
	record(rec, class, outcome, waited)
	slog.Info("request", "req_id", reqID, "class", class, "outcome", outcome, "reason", reason,
		"path", r.URL.Path, "method", r.Method, "wait_ms", waited.Milliseconds())
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
