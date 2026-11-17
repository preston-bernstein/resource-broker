// Package metrics exposes broker counters and live gauges in Prometheus text
// format, with no external dependencies.
package metrics

import (
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Registry holds cumulative counters.
type Registry struct {
	mu        sync.Mutex
	counts    map[string]int64 // key: class|outcome
	waitSumMs float64
	waitCount int64

	parkWaitSumMs float64
	parkWaitCount int64

	jobCounts   map[string]int64 // key: job outcome
	jobRunSumMs float64
	jobRunCount int64

	detectErrors int64 // internal/detect.Detector fail-open count (2026-08-01 audit)
}

// New returns an empty Registry.
func New() *Registry {
	return &Registry{counts: make(map[string]int64), jobCounts: make(map[string]int64)}
}

// IncDetectError tallies one contention-detection failure (implements
// detect.ErrorRecorder). Every increment corresponds to a poll that failed
// open — see internal/detect/detect.go's Detect(). A nonzero rate here means
// the yield feature may be silently blind; alert on
// rate(broker_detect_errors_total[10m]) > 0.
func (r *Registry) IncDetectError() {
	r.mu.Lock()
	r.detectErrors++
	r.mu.Unlock()
}

// RecordJob tallies one terminal Job outcome ("succeeded", "failed",
// "canceled", "preempted", "retried") and, for a completed run, its duration.
func (r *Registry) RecordJob(outcome string, run time.Duration) {
	r.mu.Lock()
	r.jobCounts[outcome]++
	if outcome == "succeeded" || outcome == "failed" {
		r.jobRunSumMs += float64(run) / float64(time.Millisecond)
		r.jobRunCount++
	}
	r.mu.Unlock()
}

// Record tallies one request outcome ("served", "deferred", "preempted") for a
// class, and (for served) the time spent waiting for the slot.
func (r *Registry) Record(class, outcome string, wait time.Duration) {
	r.mu.Lock()
	r.counts[class+"|"+outcome]++
	if outcome == "served" {
		r.waitSumMs += float64(wait) / float64(time.Millisecond)
		r.waitCount++
	}
	r.mu.Unlock()
}

// RecordPark tallies time spent parked (waiting during yield) for a request
// that was eventually served. Outcome strings ("expired", "park_rejected",
// "canceled", "crash_failed") are recorded separately via Record(); this
// method is only called for requests with outcome "served" that were parked.
func (r *Registry) RecordPark(wait time.Duration) {
	r.mu.Lock()
	r.parkWaitSumMs += float64(wait) / float64(time.Millisecond)
	r.parkWaitCount++
	r.mu.Unlock()
}

// Gauges are point-in-time values sampled at scrape time.
type Gauges struct {
	Yielding    bool
	Busy        bool
	Inflight    int
	MaxInflight int
	Interactive int
	Batch       int
	Parked      int

	// Durable Job counts by state (ADR-0006).
	JobQueued    int
	JobRunning   int
	JobSucceeded int
	JobFailed    int
	JobCanceled  int
}

// Handler renders /metrics. gauges is sampled on each scrape, with the
// scrape's context.
func (r *Registry) Handler(gauges func(context.Context) Gauges) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		r.write(w, gauges(req.Context()))
	})
}

// snapshot is a consistent copy of the cumulative counters.
type snapshot struct {
	counts        map[string]int64
	waitSumMs     float64
	waitCount     int64
	parkWaitSumMs float64
	parkWaitCount int64
	jobCounts     map[string]int64
	jobRunSumMs   float64
	jobRunCount   int64
	detectErrors  int64
}

func (r *Registry) snapshot() snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return snapshot{
		counts:        maps.Clone(r.counts),
		waitSumMs:     r.waitSumMs,
		waitCount:     r.waitCount,
		parkWaitSumMs: r.parkWaitSumMs,
		parkWaitCount: r.parkWaitCount,
		jobCounts:     maps.Clone(r.jobCounts),
		jobRunSumMs:   r.jobRunSumMs,
		jobRunCount:   r.jobRunCount,
		detectErrors:  r.detectErrors,
	}
}

// exposition builds Prometheus text format.
type exposition struct{ strings.Builder }

// header writes the HELP and TYPE lines of one metric family.
func (e *exposition) header(name, kind, help string) {
	_, _ = fmt.Fprintf(e, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
}

// sample writes one value line.
func (e *exposition) sample(format string, args ...any) {
	_, _ = fmt.Fprintf(e, format+"\n", args...)
}

// counter writes a one-sample counter family.
func (e *exposition) counter(name, help, format string, value any) {
	e.header(name, "counter", help)
	e.sample(format, value)
}

// gauge writes a one-sample gauge family.
func (e *exposition) gauge(name, help string, value int) {
	e.header(name, "gauge", help)
	e.sample("%s %d", name, value)
}

func (r *Registry) write(w io.Writer, g Gauges) {
	snap := r.snapshot()
	var e exposition
	writeRequestCounters(&e, snap)
	writeLiveGauges(&e, g)
	writeParkMetrics(&e, snap, g)
	writeJobMetrics(&e, snap, g)
	_, _ = io.WriteString(w, e.String())
}

func writeRequestCounters(e *exposition, snap snapshot) {
	e.header("broker_requests_total", "counter", "Requests by class and outcome.")
	for _, k := range slices.Sorted(maps.Keys(snap.counts)) {
		class, outcome := splitKey(k)
		e.sample("broker_requests_total{class=%q,outcome=%q} %d", class, outcome, snap.counts[k])
	}
	e.counter("broker_wait_seconds_sum", "Total time served requests waited for the GPU slot.", "broker_wait_seconds_sum %g", snap.waitSumMs/1000.0)
	e.counter("broker_wait_seconds_count", "Number of served requests.", "broker_wait_seconds_count %d", snap.waitCount)
	e.counter("broker_detect_errors_total", "Contention-detection polls that failed and reported no contention (fail-open). Nonzero means the yield feature may be blind.", "broker_detect_errors_total %d", snap.detectErrors)
}

func writeLiveGauges(e *exposition, g Gauges) {
	e.gauge("broker_yielding", "Whether the broker is currently yielding the GPU.", b2i(g.Yielding))
	e.gauge("broker_busy", "Whether the single GPU slot is in use.", b2i(g.Busy))
	e.gauge("broker_inflight", "In-flight requests reaching Ollama.", g.Inflight)
	e.gauge("broker_max_inflight", "Configured concurrency limit.", g.MaxInflight)
	e.header("broker_queue_depth", "gauge", "Waiters per class.")
	e.sample("broker_queue_depth{class=\"interactive\"} %d", g.Interactive)
	e.sample("broker_queue_depth{class=\"batch\"} %d", g.Batch)
}

func writeParkMetrics(e *exposition, snap snapshot, g Gauges) {
	e.gauge("broker_parked_depth", "Batch-class park depth; Interactive never parks.", g.Parked)
	e.counter("broker_park_wait_seconds_sum", "Total time requests spent parked waiting for yield to end.", "broker_park_wait_seconds_sum %g", snap.parkWaitSumMs/1000.0)
	e.counter("broker_park_wait_seconds_count", "Number of requests that were parked and eventually served.", "broker_park_wait_seconds_count %d", snap.parkWaitCount)
}

func writeJobMetrics(e *exposition, snap snapshot, g Gauges) {
	e.header("broker_jobs", "gauge", "Durable Jobs by state.")
	for _, state := range []struct {
		name  string
		count int
	}{
		{"queued", g.JobQueued},
		{"running", g.JobRunning},
		{"succeeded", g.JobSucceeded},
		{"failed", g.JobFailed},
		{"canceled", g.JobCanceled},
	} {
		e.sample("broker_jobs{state=\"%s\"} %d", state.name, state.count)
	}
	e.header("broker_job_outcomes_total", "counter", "Terminal Job outcomes.")
	for _, k := range slices.Sorted(maps.Keys(snap.jobCounts)) {
		e.sample("broker_job_outcomes_total{outcome=%q} %d", k, snap.jobCounts[k])
	}
	e.counter("broker_job_run_seconds_sum", "Total time Jobs spent running to completion.", "broker_job_run_seconds_sum %g", snap.jobRunSumMs/1000.0)
	e.counter("broker_job_run_seconds_count", "Number of completed Job runs.", "broker_job_run_seconds_count %d", snap.jobRunCount)
}

func splitKey(k string) (class, outcome string) {
	for i := 0; i < len(k); i++ {
		if k[i] == '|' {
			return k[:i], k[i+1:]
		}
	}
	return k, ""
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
