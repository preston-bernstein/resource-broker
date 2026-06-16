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
}

// New returns an empty Registry.
func New() *Registry {
	return &Registry{counts: make(map[string]int64)}
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

// Gauges are point-in-time values sampled at scrape time.
type Gauges struct {
	Yielding    bool
	Busy        bool
	Interactive int
	Batch       int
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
	r.mu.Unlock()

	fmt.Fprint(w, "# HELP broker_wait_seconds_sum Total time served requests waited for the GPU slot.\n")
	fmt.Fprint(w, "# TYPE broker_wait_seconds_sum counter\n")
	fmt.Fprintf(w, "broker_wait_seconds_sum %g\n", sumMs/1000.0)
	fmt.Fprint(w, "# HELP broker_wait_seconds_count Number of served requests.\n")
	fmt.Fprint(w, "# TYPE broker_wait_seconds_count counter\n")
	fmt.Fprintf(w, "broker_wait_seconds_count %d\n", count)

	fmt.Fprint(w, "# HELP broker_yielding Whether the broker is currently yielding the GPU.\n")
	fmt.Fprint(w, "# TYPE broker_yielding gauge\n")
	fmt.Fprintf(w, "broker_yielding %d\n", b2i(g.Yielding))
	fmt.Fprint(w, "# HELP broker_busy Whether the single GPU slot is in use.\n")
	fmt.Fprint(w, "# TYPE broker_busy gauge\n")
	fmt.Fprintf(w, "broker_busy %d\n", b2i(g.Busy))
	fmt.Fprint(w, "# HELP broker_queue_depth Waiters per class.\n")
	fmt.Fprint(w, "# TYPE broker_queue_depth gauge\n")
	fmt.Fprintf(w, "broker_queue_depth{class=\"interactive\"} %d\n", g.Interactive)
	fmt.Fprintf(w, "broker_queue_depth{class=\"batch\"} %d\n", g.Batch)
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
