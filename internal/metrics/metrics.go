// Package metrics exposes broker counters and live gauges in Prometheus text
// format, with no external dependencies.
package metrics

import (
	"fmt"
	"io"
	"net/http"
	"sort"
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

// Handler renders /metrics. gauges is sampled on each scrape.
func (r *Registry) Handler(gauges func() Gauges) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		r.write(w, gauges())
	})
}

func (r *Registry) write(w io.Writer, g Gauges) {
	fmt.Fprint(w, "# HELP broker_requests_total Requests by class and outcome.\n")
	fmt.Fprint(w, "# TYPE broker_requests_total counter\n")

	r.mu.Lock()
	keys := make([]string, 0, len(r.counts))
	for k := range r.counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		class, outcome := splitKey(k)
		fmt.Fprintf(w, "broker_requests_total{class=%q,outcome=%q} %d\n", class, outcome, r.counts[k])
	}
	sumMs, count := r.waitSumMs, r.waitCount
	parkSumMs, parkCount := r.parkWaitSumMs, r.parkWaitCount
	jobKeys := make([]string, 0, len(r.jobCounts))
	for k := range r.jobCounts {
		jobKeys = append(jobKeys, k)
	}
	sort.Strings(jobKeys)
	jobCountsCopy := make(map[string]int64, len(r.jobCounts))
	for k, v := range r.jobCounts {
		jobCountsCopy[k] = v
	}
	jobRunSumMs, jobRunCount := r.jobRunSumMs, r.jobRunCount
	detectErrors := r.detectErrors
	r.mu.Unlock()

	fmt.Fprint(w, "# HELP broker_wait_seconds_sum Total time served requests waited for the GPU slot.\n")
	fmt.Fprint(w, "# TYPE broker_wait_seconds_sum counter\n")
	fmt.Fprintf(w, "broker_wait_seconds_sum %g\n", sumMs/1000.0)
	fmt.Fprint(w, "# HELP broker_wait_seconds_count Number of served requests.\n")
	fmt.Fprint(w, "# TYPE broker_wait_seconds_count counter\n")
	fmt.Fprintf(w, "broker_wait_seconds_count %d\n", count)

	fmt.Fprint(w, "# HELP broker_detect_errors_total Contention-detection polls that failed and reported no contention (fail-open). Nonzero means the yield feature may be blind.\n")
	fmt.Fprint(w, "# TYPE broker_detect_errors_total counter\n")
	fmt.Fprintf(w, "broker_detect_errors_total %d\n", detectErrors)

	fmt.Fprint(w, "# HELP broker_yielding Whether the broker is currently yielding the GPU.\n")
	fmt.Fprint(w, "# TYPE broker_yielding gauge\n")
	fmt.Fprintf(w, "broker_yielding %d\n", b2i(g.Yielding))
	fmt.Fprint(w, "# HELP broker_busy Whether the single GPU slot is in use.\n")
	fmt.Fprint(w, "# TYPE broker_busy gauge\n")
	fmt.Fprintf(w, "broker_busy %d\n", b2i(g.Busy))
	fmt.Fprint(w, "# HELP broker_inflight In-flight requests reaching Ollama.\n")
	fmt.Fprint(w, "# TYPE broker_inflight gauge\n")
	fmt.Fprintf(w, "broker_inflight %d\n", g.Inflight)
	fmt.Fprint(w, "# HELP broker_max_inflight Configured concurrency limit.\n")
	fmt.Fprint(w, "# TYPE broker_max_inflight gauge\n")
	fmt.Fprintf(w, "broker_max_inflight %d\n", g.MaxInflight)
	fmt.Fprint(w, "# HELP broker_queue_depth Waiters per class.\n")
	fmt.Fprint(w, "# TYPE broker_queue_depth gauge\n")
	fmt.Fprintf(w, "broker_queue_depth{class=\"interactive\"} %d\n", g.Interactive)
	fmt.Fprintf(w, "broker_queue_depth{class=\"batch\"} %d\n", g.Batch)

	fmt.Fprint(w, "# HELP broker_parked_depth Batch-class park depth; Interactive never parks.\n")
	fmt.Fprint(w, "# TYPE broker_parked_depth gauge\n")
	fmt.Fprintf(w, "broker_parked_depth %d\n", g.Parked)

	fmt.Fprint(w, "# HELP broker_park_wait_seconds_sum Total time requests spent parked waiting for yield to end.\n")
	fmt.Fprint(w, "# TYPE broker_park_wait_seconds_sum counter\n")
	fmt.Fprintf(w, "broker_park_wait_seconds_sum %g\n", parkSumMs/1000.0)
	fmt.Fprint(w, "# HELP broker_park_wait_seconds_count Number of requests that were parked and eventually served.\n")
	fmt.Fprint(w, "# TYPE broker_park_wait_seconds_count counter\n")
	fmt.Fprintf(w, "broker_park_wait_seconds_count %d\n", parkCount)

	fmt.Fprint(w, "# HELP broker_jobs Durable Jobs by state.\n")
	fmt.Fprint(w, "# TYPE broker_jobs gauge\n")
	fmt.Fprintf(w, "broker_jobs{state=\"queued\"} %d\n", g.JobQueued)
	fmt.Fprintf(w, "broker_jobs{state=\"running\"} %d\n", g.JobRunning)
	fmt.Fprintf(w, "broker_jobs{state=\"succeeded\"} %d\n", g.JobSucceeded)
	fmt.Fprintf(w, "broker_jobs{state=\"failed\"} %d\n", g.JobFailed)
	fmt.Fprintf(w, "broker_jobs{state=\"canceled\"} %d\n", g.JobCanceled)

	fmt.Fprint(w, "# HELP broker_job_outcomes_total Terminal Job outcomes.\n")
	fmt.Fprint(w, "# TYPE broker_job_outcomes_total counter\n")
	for _, k := range jobKeys {
		fmt.Fprintf(w, "broker_job_outcomes_total{outcome=%q} %d\n", k, jobCountsCopy[k])
	}
	fmt.Fprint(w, "# HELP broker_job_run_seconds_sum Total time Jobs spent running to completion.\n")
	fmt.Fprint(w, "# TYPE broker_job_run_seconds_sum counter\n")
	fmt.Fprintf(w, "broker_job_run_seconds_sum %g\n", jobRunSumMs/1000.0)
	fmt.Fprint(w, "# HELP broker_job_run_seconds_count Number of completed Job runs.\n")
	fmt.Fprint(w, "# TYPE broker_job_run_seconds_count counter\n")
	fmt.Fprintf(w, "broker_job_run_seconds_count %d\n", jobRunCount)
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
