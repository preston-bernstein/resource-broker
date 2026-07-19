package queue

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

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
func (s *Scheduler) Gate(class Class, wait time.Duration, adm Admission, rec Recorder, next http.Handler) http.Handler {
	cls := class.String()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		if yielding, reason := adm.Yielding(); yielding {
			deferRequest(w, rec, cls, "yielding GPU: "+reason, wait, 0)
			return
		}

		actx, acancel := context.WithTimeout(r.Context(), wait)
		err := s.Acquire(actx, class)
		acancel()
		waited := time.Since(start)
		if err != nil {
			deferRequest(w, rec, cls, "GPU busy: wait budget exceeded", wait, waited)
			return
		}
		defer s.Release()

		if yielding, reason := adm.Yielding(); yielding {
			deferRequest(w, rec, cls, "yielding GPU: "+reason, wait, waited)
			return
		}

		ctx, cancel := context.WithCancel(r.Context())
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
		// serve state rather than a raced flag.
		outcome := "served"
		select {
		case <-serveDone:
			outcome = "preempted"
		default:
		}
		w.Header().Set(http.TrailerPrefix+"X-Broker-Status", outcome)
		record(rec, cls, outcome, waited)
		slog.Info("request", "class", cls, "outcome", outcome, "wait_ms", waited.Milliseconds())
	})
}

func deferRequest(w http.ResponseWriter, rec Recorder, class, reason string, retryAfter, waited time.Duration) {
	secs := int(retryAfter.Round(time.Second) / time.Second)
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	w.Header().Set("X-Broker-Status", "deferred")
	http.Error(w, "broker: "+reason, http.StatusServiceUnavailable)
	record(rec, class, "deferred", waited)
	slog.Info("request", "class", class, "outcome", "deferred", "reason", reason, "wait_ms", waited.Milliseconds())
}

func record(rec Recorder, class, outcome string, wait time.Duration) {
	if rec != nil {
		rec.Record(class, outcome, wait)
	}
}
