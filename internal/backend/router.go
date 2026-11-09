package backend

import (
	"context"
	"net/http"
	"sort"

	"github.com/preston-bernstein/resource-broker/internal/httpx"
	"github.com/preston-bernstein/resource-broker/internal/yield"
)

// routeEntry is one entry in Router's per-model routing table: the Backend a
// matching request dispatches to, and the lane ("interactive", "batch", or
// "" for both) that entry is scoped to. resolve enforces lane-scoping
// against this field (see resolve's doc comment), and RoutingSummary groups
// model names that share an identical routeEntry (same backend+lane) into
// one summary entry.
type routeEntry struct {
	backend Backend
	lane    string
}

// Router is a Backend that dispatches requests to one of several routed
// backends by peeking at the request body's "model" field, falling back to
// a default backend for any model with no configured route (or when no
// model field is found at all). Router is itself a Backend so it can be
// wired into the same call sites (Gate, Job worker, /healthz, yield
// Controller) as any single concrete backend — see backend.go's Backend
// interface doc.
//
// Router's zero value is not usable; construct with NewRouter.
type Router struct {
	// def is the Backend used when no routed model matches (or when the
	// model can't be determined within the peek budget).
	def Backend
	// routes maps a model name to the routeEntry that should handle it.
	routes map[string]routeEntry
}

// NewRouter constructs a Router with the given default backend and an empty
// routing table. Use AddRoute to populate routes.
func NewRouter(def Backend) *Router {
	return &Router{
		def:    def,
		routes: make(map[string]routeEntry),
	}
}

// AddRoute registers backend as the target for model, scoped to lane
// ("interactive", "batch", or "" for both). Lane-matching semantics live in
// resolve; this method just populates the table.
func (r *Router) AddRoute(model, lane string, backend Backend) {
	r.routes[model] = routeEntry{backend: backend, lane: lane}
}

// ProxyForLane returns an http.Handler that, for each incoming request,
// peeks at the JSON request body's "model" field (bounded to 64KB
// — see httpx.PeekModel) and dispatches to the routed
// backend's Proxy() if one is configured for that model, else falls back to
// the default backend's Proxy(). The request body is restored before
// forwarding either way, so the backend that ultimately handles the request
// sees a body byte-identical to what the client sent — no truncation, no
// re-encoding, regardless of which path was taken.
//
// lane is the caller's lane string ("interactive"/"batch", matching
// queue.Class.String()) — Router does not import internal/queue, so callers
// convert their queue.Class to its string form before calling. An entry
// scoped to a specific lane (routeEntry.lane != "") only matches a request
// for that same lane; an unscoped entry (lane == "") matches both — see
// resolve's doc comment for the exact rule, and note that error responses
// written by whichever backend's Proxy() ultimately handles the request
// (routed or default) reach the Consumer completely unmodified: Router does
// not inspect, rewrite, or swallow a backend's response, success or error
// (4xx, 5xx, invalid JSON body, or anything else the backend writes).
func (r *Router) ProxyForLane(lane string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		backend := r.resolve(req, lane)
		backend.Proxy().ServeHTTP(w, req)
	})
}

// Proxy() satisfies the Backend interface's requirement but is not used by
// production wiring (which always calls ProxyForLane with a real lane) —
// calling it directly is equivalent to calling ProxyForLane(""), which
// correctly falls through to the default backend for any lane-scoped route
// since no lane context is available.
func (r *Router) Proxy() http.Handler {
	return r.ProxyForLane("")
}

// resolve peeks at req's body to find a routed backend for the request's
// model, restoring the body (byte-identical to what was received) before
// returning. It falls back to the default backend when: req.Body is nil,
// the "model" field isn't found within the 64KB peek, no route is
// configured for that model, or a configured route is scoped to a lane
// other than lane (when lane is non-empty and the route's lane is
// non-empty and they differ).
func (r *Router) resolve(req *http.Request, lane string) Backend {
	// RequestModel restores the body byte-identical either way.
	model := httpx.RequestModel(req)
	if model == "" {
		return r.def
	}

	entry, ok := r.routes[model]
	if !ok {
		return r.def
	}
	// entry.lane == "" means the route applies to both lanes and always
	// matches. Otherwise the entry only matches a request in that same
	// lane — in particular, Proxy() (which calls ProxyForLane("")) never
	// has lane context, so a lane-scoped entry never matches there and
	// correctly falls through to the default backend, per Proxy()'s doc
	// comment.
	if entry.lane != "" && entry.lane != lane {
		return r.def
	}
	return entry.backend
}

// Generate dispatches to the routed backend for model, if one is
// configured, else to the default backend — matching model directly against
// r.routes rather than peeking a request body, since the durable Job path
// (unlike ProxyForLane's HTTP path) already has model as a plain argument.
// Unlike ProxyForLane/resolve, Generate does not consult a routeEntry's lane:
// Jobs have no lane at all (queue.Class doesn't apply to the Job worker), so
// a configured route applies to Generate regardless of what lane (if any) it
// was scoped to for the proxy path.
func (r *Router) Generate(ctx context.Context, model, prompt string, options map[string]any, onTokens func(int)) (string, error) {
	if entry, ok := r.routes[model]; ok {
		return entry.backend.Generate(ctx, model, prompt, options, onTokens)
	}
	return r.def.Generate(ctx, model, prompt, options, onTokens)
}

// Reachable reports whether the default backend is reachable. It
// deliberately does not check any routed backend's reachability —
// RoutingSummary (below) is Router's admin-facing routing-table surface,
// but it does not probe liveness either (see its doc comment for why); so
// /healthz calling Reachable on a Router only ever reflects the default
// backend's health, exactly as if no routes were configured at all.
func (r *Router) Reachable(ctx context.Context) error {
	return r.def.Reachable(ctx)
}

// Unloader returns a literal nil — Router's own unload/reload responsibility
// for its default and routed backends is wired explicitly in
// cmd/broker/main.go (via each individual backend's own Unloader(),
// collected into yield.NewWithConfirmMulti's unloaders slice), not through
// this interface method. This is a deliberate, reasoned trade-off, not an
// oversight: giving Router its own narrower interface (splitting Backend so
// Router wouldn't need to satisfy Unloader() at all) was considered and
// rejected as unnecessary churn — one polymorphic Backend-satisfying type
// simplifies the Gate/Job-worker/healthCheck call sites at the cost of one
// method that must be documented rather than meaningful. See
// docs/adr/0015-per-model-backend-routing.md (added by a later task) for the
// full reasoning.
//
// Per backend.go's Backend interface doc ("Typed-nil safety"), this must be
// (and is) a direct, literal nil of the yield.Unloader interface type, never
// a typed-nil concrete pointer boxed into it.
func (r *Router) Unloader() yield.Unloader {
	return nil
}

// RouteSummary is one entry in RoutingSummary's returned table: one or more
// model names that all map to an identical routeEntry (same Backend value
// and same lane), grouped together rather than duplicated one-per-model.
type RouteSummary struct {
	// Models lists every model name routed to this entry's backend+lane,
	// sorted for deterministic output.
	Models []string
	// Lane is "interactive", "batch", or "" (both lanes) — mirrors
	// routeEntry.lane and matches the string form ProxyForLane's callers
	// pass (queue.Class.String()).
	Lane string
}

// RoutingSummary returns the current routing table for admin output (see
// plan.md's /status endpoint contract, which documents an optional
// "routing" key populated from this). It groups model names that share an
// identical routeEntry — same Backend value and same lane — into a single
// RouteSummary rather than emitting one entry per model, so N models
// pointed at the same routed backend for the same lane produce one entry
// with an N-element Models slice.
//
// plan.md's /status "routing" key describes a fuller per-entry JSON shape —
// {models, backend, url, lane, reachable} — but Router as implemented today
// has no way to produce backend/url/reachable honestly: routeEntry only
// holds a Backend interface value (not a "backend family" string or the URL
// it was constructed from — those strings exist only in whatever
// cmd/broker/main.go built the Backend from, and are never threaded into
// AddRoute), and Router has no live reachability probe per route (Reachable
// above only ever checks the default backend, by design). Rather than
// fabricate placeholder values for fields Router cannot honestly know, this
// method omits backend/url/reachable entirely and returns only what Router
// actually holds: the models-to-lane grouping. Populating the fuller shape
// is a natural follow-up for whoever wires /status in a later task — it
// would need AddRoute (or a new registration path) to also capture each
// route's backend name/URL strings, plus a per-route reachability check.
//
// The return type is `any` (rather than a named []RouteSummary result) so
// callers that only need to serialize this for /status don't need to
// import RouteSummary directly; callers that do need the concrete shape can
// still type-assert to []RouteSummary.
func (r *Router) RoutingSummary() any {
	type key struct {
		backend Backend
		lane    string
	}
	grouped := make(map[key][]string)
	for model, entry := range r.routes {
		k := key{backend: entry.backend, lane: entry.lane}
		grouped[k] = append(grouped[k], model)
	}

	summaries := make([]RouteSummary, 0, len(grouped))
	for k, models := range grouped {
		sort.Strings(models)
		summaries = append(summaries, RouteSummary{Models: models, Lane: k.lane})
	}
	// Sort the top-level slice too (by lane, then by first model name) so
	// RoutingSummary's output is deterministic across calls despite being
	// built from map iteration, which Go does not order.
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Lane != summaries[j].Lane {
			return summaries[i].Lane < summaries[j].Lane
		}
		return summaries[i].Models[0] < summaries[j].Models[0]
	})
	return summaries
}
