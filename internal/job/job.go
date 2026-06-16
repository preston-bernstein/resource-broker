// Package job implements the durable async Job path (ADR-0006/0007): long batch
// inference is submitted, persisted, and processed through the same single-GPU
// scheduler as the synchronous proxy, surviving broker restarts. Jobs are the
// batch priority class; gaming/Plex preempts them and an interactive request
// preempts a running Job past its min-run quantum — a preempted Job requeues at
// the front of the line.
package job

import (
	"errors"
	"time"
)

// State is a Job's lifecycle position. QUEUED and RUNNING are live; the rest
// are terminal.
type State string

const (
	// StateQueued: waiting for a GPU slot.
	StateQueued State = "QUEUED"
	// StateRunning: holding a slot, generating on the upstream.
	StateRunning State = "RUNNING"
	// StateSucceeded: completed; result persisted.
	StateSucceeded State = "SUCCEEDED"
	// StateFailed: exhausted attempts or hit a fatal error.
	StateFailed State = "FAILED"
	// StateCanceled: cancelled by the consumer.
	StateCanceled State = "CANCELED"
)

// Terminal reports whether the state is final (no further transitions).
func (s State) Terminal() bool {
	switch s {
	case StateSucceeded, StateFailed, StateCanceled:
		return true
	default:
		return false
	}
}

// DefaultMaxAttempts caps re-runs of a Job that keeps crashing the broker
// (ADR-0007). Each restart of a RUNNING Job costs one attempt.
const DefaultMaxAttempts = 3

// Errors returned by the store and service.
var (
	// ErrNotFound: no Job with the given id.
	ErrNotFound = errors.New("job not found")
	// ErrNotReady: result requested for a Job that has not succeeded.
	ErrNotReady = errors.New("job result not ready")
	// ErrAlreadyTerminal: a transition was requested on a terminal Job.
	ErrAlreadyTerminal = errors.New("job already terminal")
)

// Job is a durable unit of batch inference work. Times are nil when unset.
type Job struct {
	ID             string
	IdempotencyKey string
	Source         string
	Owner          string
	State          State
	Attempts       int
	Model          string
	ParamsJSON     string // extra Ollama options, raw JSON object (may be empty)
	Prompt         string
	Result         string
	Error          string
	PositionHint   int64 // queue ordering key; lower runs first (front < 0 < tail)
	CreatedAt      time.Time
	StartedAt      *time.Time
	FinishedAt     *time.Time
	FetchedAt      *time.Time
}

// Status is the small, canonical status view returned by GET /jobs/{id}: no
// result blob, just state and live signals.
type Status struct {
	ID        string     `json:"id"`
	State     State      `json:"state"`
	Source    string     `json:"source,omitempty"`
	Owner     string     `json:"owner,omitempty"`
	Attempts  int        `json:"attempts"`
	Position  int        `json:"position,omitempty"` // 1-based place in the batch line; 0 when not queued
	Progress  *Progress  `json:"progress,omitempty"` // present while RUNNING
	Error     string     `json:"error,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	FetchedAt *time.Time `json:"fetched_at,omitempty"`
}

// Progress is live feedback for a RUNNING Job.
type Progress struct {
	Tokens    int   `json:"tokens"`
	ElapsedMs int64 `json:"elapsed_ms"`
}
