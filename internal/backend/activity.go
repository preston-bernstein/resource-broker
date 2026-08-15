package backend

import (
	"net/http"

	"github.com/preston-bernstein/resource-broker/internal/yield"
)

// activityBackend decorates a Backend so its Synchronous-path handler is
// wrapped with yield.Controller's per-instance idle-activity bookkeeping
// (Controller.TrackActivity). It embeds Backend so every method other than
// Proxy is satisfied by delegation to the wrapped backend unchanged.
//
// activityBackend must only ever be used as a pointer (*activityBackend),
// never a bare value — see WithActivityTracking's doc comment for why: the
// embedded Backend interface is comparable, but proxy's dynamic value (a
// closure built by ctrl.TrackActivity) is not, so a bare activityBackend
// value would panic the moment it were used as (or as part of) a map key,
// which Router.RoutingSummary does on every call.
type activityBackend struct {
	Backend
	proxy http.Handler
}

// WithActivityTracking wraps b so its Proxy() handler is decorated with
// ctrl's idle-activity tracking for the configured instance at ORIG
// (pre-nil-filter) index origIdx — the same index space
// Controller.ConfigureIdle/TrackActivity take.
//
// b.Proxy() is called exactly once here, at wrap time, to obtain the real
// underlying handler; ctrl.TrackActivity(origIdx, realHandler) wraps it
// exactly once, and the result is cached in the returned activityBackend's
// proxy field. The returned Backend's Proxy() method (below) always returns
// that same cached handler — it never re-calls b.Proxy() or
// ctrl.TrackActivity on subsequent calls.
//
// The return value is a pointer, &activityBackend{...}, never a bare
// activityBackend value: Router.RoutingSummary() uses Backend as part of a
// map key (`type key struct { backend Backend; lane string }`), which
// requires the interface's dynamic type to be comparable. activityBackend's
// proxy field holds a closure (func values are not comparable), so a bare
// activityBackend value would panic with "comparing uncomparable type" the
// instant two decorated backends landed in the same map — which
// RoutingSummary does on every call, and /status triggers on every poll. A
// pointer type is always comparable regardless of what it points to, so
// returning a pointer sidesteps this permanently.
func WithActivityTracking(b Backend, ctrl *yield.Controller, origIdx int) Backend {
	real := b.Proxy()
	wrapped := ctrl.TrackActivity(origIdx, real)
	return &activityBackend{Backend: b, proxy: wrapped}
}

// Proxy returns the already-built, activity-tracking-wrapped handler cached
// at WithActivityTracking's call time. It does not call a.Backend.Proxy()
// or ctrl.TrackActivity again — the wrapping happens exactly once.
func (a *activityBackend) Proxy() http.Handler {
	return a.proxy
}
