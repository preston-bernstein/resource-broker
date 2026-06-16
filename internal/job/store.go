package job

import (
	"context"
	"time"
)

// Filter scopes a List query. Empty fields are wildcards.
type Filter struct {
	Source string
	Owner  string
	State  State
	Limit  int // 0 -> default cap
}

// Counts is a snapshot of Job counts by state, for metrics/status.
type Counts struct {
	Queued    int
	Running   int
	Succeeded int
	Failed    int
	Canceled  int
}

// Store is the durable Job backing. All methods are safe for concurrent use.
// Write ordering is durability-first (ADR-0007): Submit persists before the id
// is returned; Succeed writes the result and the SUCCEEDED state in one atomic
// step so no SUCCEEDED Job ever lacks its result.
type Store interface {
	// Submit persists a new Job, or — if its IdempotencyKey already exists —
	// returns the existing Job with created=false (no duplicate work).
	Submit(ctx context.Context, j *Job) (stored *Job, created bool, err error)

	// Get returns the Job by id, or ErrNotFound.
	Get(ctx context.Context, id string) (*Job, error)

	// List returns Jobs matching f, newest first.
	List(ctx context.Context, f Filter) ([]*Job, error)

	// HasQueued reports whether any Job is waiting, so the worker can avoid
	// taking a GPU slot just to find an empty queue.
	HasQueued(ctx context.Context) (bool, error)

	// ClaimNext atomically moves the highest-priority QUEUED Job to RUNNING and
	// returns it, or ErrNotFound if the queue is empty.
	ClaimNext(ctx context.Context) (*Job, error)

	// Succeed records the result and flips the Job to SUCCEEDED atomically.
	Succeed(ctx context.Context, id, result string) error

	// Preempt returns a RUNNING Job to QUEUED at the front (gaming/interactive).
	// attempts is unchanged: a clean preempt is not a failed attempt.
	Preempt(ctx context.Context, id string) error

	// FailOrRetry increments attempts on a run error; if attempts reach
	// maxAttempts the Job is FAILED, otherwise it requeues at the front.
	FailOrRetry(ctx context.Context, id, errMsg string, maxAttempts int) (failed bool, err error)

	// RecoverRunning resets every RUNNING Job to QUEUED@front with attempts++,
	// failing any that exceed maxAttempts. Run once at startup. Returns the
	// number of Jobs touched.
	RecoverRunning(ctx context.Context, maxAttempts int) (int, error)

	// Cancel marks a non-terminal Job CANCELED and returns it; terminal Jobs are
	// returned unchanged.
	Cancel(ctx context.Context, id string) (*Job, error)

	// Position is the 1-based place of a QUEUED Job among the batch line, or 0
	// if it is not queued.
	Position(ctx context.Context, id string) (int, error)

	// StampFetched marks the first successful result read (retain-until-fetched).
	StampFetched(ctx context.Context, id string) error

	// Prune deletes terminal Jobs: SUCCEEDED ones fetchedGrace after their first
	// fetch, and any terminal Job older than hardCap. Returns the count deleted.
	Prune(ctx context.Context, fetchedGrace, hardCap time.Duration, now time.Time) (int, error)

	// Counts returns Job counts by state.
	Counts(ctx context.Context) (Counts, error)

	// Close releases the backing handle.
	Close() error
}
