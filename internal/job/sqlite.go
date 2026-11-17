package job

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: keeps the CGO-free static binary
)

// memSeq makes each in-memory store a distinct shared-cache database so tests
// (and any other ":memory:" callers) never collide on one global DB.
var memSeq atomic.Int64

// SQLiteStore is a durable Store backed by a single SQLite file in WAL mode
// (ADR-0007). Pure-Go driver, so the broker stays a static CGO-free binary.
type SQLiteStore struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS jobs (
	id              TEXT PRIMARY KEY,
	idempotency_key TEXT NOT NULL UNIQUE,
	source          TEXT NOT NULL DEFAULT '',
	owner           TEXT NOT NULL DEFAULT '',
	state           TEXT NOT NULL,
	attempts        INTEGER NOT NULL DEFAULT 0,
	model           TEXT NOT NULL,
	params_json     TEXT NOT NULL DEFAULT '',
	prompt          TEXT NOT NULL DEFAULT '',
	result          TEXT NOT NULL DEFAULT '',
	error           TEXT NOT NULL DEFAULT '',
	position_hint   INTEGER NOT NULL DEFAULT 0,
	created_at      INTEGER NOT NULL,
	started_at      INTEGER,
	finished_at     INTEGER,
	fetched_at      INTEGER
);
CREATE INDEX IF NOT EXISTS idx_jobs_queue ON jobs(state, position_hint, created_at);
CREATE INDEX IF NOT EXISTS idx_jobs_scope ON jobs(source, owner, state);
`

// OpenSQLite opens (creating if needed) the Job database at path and applies
// the schema. Use ":memory:" for tests.
func OpenSQLite(ctx context.Context, path string) (*SQLiteStore, error) {
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	if path == ":memory:" {
		// A uniquely-named shared in-memory DB so the *sql.DB pool sees one
		// database, but separate stores never alias each other.
		name := "memdb" + strconv.FormatInt(memSeq.Add(1), 10)
		dsn = "file:" + name + "?mode=memory&cache=shared&_pragma=busy_timeout(5000)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("OpenSQLite: open database: %w", err)
	}
	// One writer at a time avoids SQLITE_BUSY churn on a home-scale workload.
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &SQLiteStore{db: db}, nil
}

// Close releases the database handle.
func (s *SQLiteStore) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close database: %w", err)
	}
	return nil
}

func (s *SQLiteStore) Submit(ctx context.Context, j *Job) (*Job, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("Submit: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Idempotency: a retried submit with a known key returns the existing Job.
	if existing, err := scanOne(tx.QueryRowContext(ctx,
		selectCols+` FROM jobs WHERE idempotency_key = ?`, j.IdempotencyKey)); err == nil {
		return existing, false, commit(tx)
	} else if !errors.Is(err, ErrNotFound) {
		return nil, false, err
	}

	var nextHint int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(position_hint), 0) + 1 FROM jobs`).Scan(&nextHint); err != nil {
		return nil, false, fmt.Errorf("Submit: scan row: %w", err)
	}
	j.State = StateQueued
	j.Attempts = 0
	j.PositionHint = nextHint
	if j.CreatedAt.IsZero() {
		j.CreatedAt = time.Now()
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO jobs (id, idempotency_key, source, owner, state, attempts, model, params_json, prompt, result, error, position_hint, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,'','',?,?)`,
		j.ID, j.IdempotencyKey, j.Source, j.Owner, j.State, j.Attempts, j.Model, j.ParamsJSON, j.Prompt, j.PositionHint, j.CreatedAt.UnixNano())
	if err != nil {
		return nil, false, fmt.Errorf("Submit: exec: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("Submit: commit transaction: %w", err)
	}
	return j, true, nil
}

func (s *SQLiteStore) Get(ctx context.Context, id string) (*Job, error) {
	return scanOne(s.db.QueryRowContext(ctx, selectCols+` FROM jobs WHERE id = ?`, id))
}

func (s *SQLiteStore) List(ctx context.Context, f Filter) ([]*Job, error) {
	q := selectCols + ` FROM jobs WHERE 1=1`
	var args []any
	if f.Source != "" {
		q += ` AND source = ?`
		args = append(args, f.Source)
	}
	if f.Owner != "" {
		q += ` AND owner = ?`
		args = append(args, f.Owner)
	}
	if f.State != "" {
		q += ` AND state = ?`
		args = append(args, f.State)
	}
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	q += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("List: query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*Job
	for rows.Next() {
		j, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rowsErr(rows)
}

func (s *SQLiteStore) HasQueued(ctx context.Context) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM jobs WHERE state = ?)`, StateQueued).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("HasQueued: scan row: %w", err)
	}
	return n == 1, nil
}

func (s *SQLiteStore) ClaimNext(ctx context.Context) (*Job, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("ClaimNext: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	j, err := scanOne(tx.QueryRowContext(ctx,
		selectCols+` FROM jobs WHERE state = ? ORDER BY position_hint ASC, created_at ASC LIMIT 1`, StateQueued))
	if err != nil {
		return nil, err // ErrNotFound when the queue is empty
	}
	now := time.Now()
	if _, err := tx.ExecContext(ctx,
		`UPDATE jobs SET state = ?, started_at = ?, error = '' WHERE id = ? AND state = ?`,
		StateRunning, now.UnixNano(), j.ID, StateQueued); err != nil {
		return nil, fmt.Errorf("ClaimNext: exec: %w", err)
	}
	j.State = StateRunning
	j.StartedAt = &now
	j.Error = ""
	return j, commit(tx)
}

func (s *SQLiteStore) Succeed(ctx context.Context, id, result string) error {
	now := time.Now().UnixNano()
	// Result and terminal state in one statement: a SUCCEEDED Job always has it.
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET state = ?, result = ?, finished_at = ?, error = '' WHERE id = ? AND state = ?`,
		StateSucceeded, result, now, id, StateRunning)
	return mustAffect(res, err)
}

func (s *SQLiteStore) Preempt(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET state = ?, started_at = NULL, position_hint = (
			SELECT COALESCE(MIN(position_hint), 0) - 1 FROM jobs WHERE state = ?
		) WHERE id = ? AND state = ?`,
		StateQueued, StateQueued, id, StateRunning)
	return mustAffect(res, err)
}

func (s *SQLiteStore) FailOrRetry(ctx context.Context, id, errMsg string, maxAttempts int) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("FailOrRetry: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var attempts int
	var state State
	if err := tx.QueryRowContext(ctx, `SELECT attempts, state FROM jobs WHERE id = ?`, id).Scan(&attempts, &state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, fmt.Errorf("FailOrRetry: scan row: %w", err)
	}
	if state != StateRunning {
		return false, ErrAlreadyTerminal
	}
	attempts++
	if attempts >= maxAttempts {
		if _, err := tx.ExecContext(ctx,
			`UPDATE jobs SET state = ?, attempts = ?, error = ?, finished_at = ? WHERE id = ?`,
			StateFailed, attempts, errMsg, time.Now().UnixNano(), id); err != nil {
			return false, fmt.Errorf("FailOrRetry: exec: %w", err)
		}
		return true, commit(tx)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE jobs SET state = ?, attempts = ?, error = ?, started_at = NULL, position_hint = (
			SELECT COALESCE(MIN(position_hint), 0) - 1 FROM jobs WHERE state = ?
		) WHERE id = ?`,
		StateQueued, attempts, errMsg, StateQueued, id); err != nil {
		return false, fmt.Errorf("FailOrRetry: exec: %w", err)
	}
	return false, commit(tx)
}

// RecoverRunning requeues every RUNNING Job a crash left behind, or fails it
// once it has used maxAttempts. It returns how many Jobs it found.
func (s *SQLiteStore) RecoverRunning(ctx context.Context, maxAttempts int) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("RecoverRunning: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	recs, err := runningJobs(ctx, tx)
	if err != nil {
		return 0, err
	}
	now := time.Now().UnixNano()
	for _, r := range recs {
		if err := r.recover(ctx, tx, maxAttempts, now); err != nil {
			return 0, err
		}
	}
	return len(recs), commit(tx)
}

type runningRec struct {
	id       string
	attempts int
}

// runningJobs lists the RUNNING Jobs in creation order.
func runningJobs(ctx context.Context, tx *sql.Tx) ([]runningRec, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, attempts FROM jobs WHERE state = ? ORDER BY created_at ASC`, StateRunning)
	if err != nil {
		return nil, fmt.Errorf("runningJobs: query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var recs []runningRec
	for rows.Next() {
		var r runningRec
		if err := rows.Scan(&r.id, &r.attempts); err != nil {
			return nil, fmt.Errorf("runningJobs: scan row: %w", err)
		}
		recs = append(recs, r)
	}
	return recs, rowsErr(rows)
}

// recover counts a restart as an attempt, then fails or requeues the Job.
func (r runningRec) recover(ctx context.Context, tx *sql.Tx, maxAttempts int, now int64) error {
	attempts := r.attempts + 1
	if attempts >= maxAttempts {
		_, err := tx.ExecContext(ctx,
			`UPDATE jobs SET state = ?, attempts = ?, error = ?, finished_at = ? WHERE id = ?`,
			StateFailed, attempts, "exceeded attempts after restart", now, r.id)
		return execErr("fail recovered job", err)
	}
	// Front of the line, computed per-row so multiple recovered Jobs keep a
	// stable created-order lead over already-queued work.
	_, err := tx.ExecContext(ctx,
		`UPDATE jobs SET state = ?, attempts = ?, started_at = NULL, position_hint = (
			SELECT COALESCE(MIN(position_hint), 0) - 1 FROM jobs WHERE state = ?
		) WHERE id = ?`,
		StateQueued, attempts, StateQueued, r.id)
	return execErr("requeue recovered job", err)
}

// execErr says which statement failed, or returns nil when err is nil.
func execErr(what string, err error) error {
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

func (s *SQLiteStore) Cancel(ctx context.Context, id string) (*Job, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("Cancel: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	j, err := scanOne(tx.QueryRowContext(ctx, selectCols+` FROM jobs WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	if j.State.Terminal() {
		return j, commit(tx) // already done; cancel is a no-op
	}
	now := time.Now()
	if _, err := tx.ExecContext(ctx,
		`UPDATE jobs SET state = ?, finished_at = ? WHERE id = ?`, StateCanceled, now.UnixNano(), id); err != nil {
		return nil, fmt.Errorf("Cancel: exec: %w", err)
	}
	j.State = StateCanceled
	j.FinishedAt = &now
	return j, commit(tx)
}

func (s *SQLiteStore) Position(ctx context.Context, id string) (int, error) {
	j, err := s.Get(ctx, id)
	if err != nil {
		return 0, err
	}
	if j.State != StateQueued {
		return 0, nil
	}
	var ahead int
	err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE state = ? AND (position_hint < ? OR (position_hint = ? AND created_at < ?))`,
		StateQueued, j.PositionHint, j.PositionHint, j.CreatedAt.UnixNano()).Scan(&ahead)
	if err != nil {
		return 0, fmt.Errorf("Position: scan row: %w", err)
	}
	return ahead + 1, nil
}

func (s *SQLiteStore) StampFetched(ctx context.Context, id string) error {
	// Stamp only once, only on a succeeded Job; re-fetches keep the first time.
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET fetched_at = ? WHERE id = ? AND state = ? AND fetched_at IS NULL`,
		time.Now().UnixNano(), id, StateSucceeded)
	if err != nil {
		return fmt.Errorf("StampFetched: exec: %w", err)
	}
	_ = res // zero rows is fine: already stamped or not succeeded
	return nil
}

func (s *SQLiteStore) Prune(ctx context.Context, fetchedGrace, hardCap time.Duration, now time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM jobs WHERE
			(state = ? AND fetched_at IS NOT NULL AND fetched_at < ?)
			OR (state IN (?,?,?) AND finished_at IS NOT NULL AND finished_at < ?)`,
		StateSucceeded, now.Add(-fetchedGrace).UnixNano(),
		StateSucceeded, StateFailed, StateCanceled, now.Add(-hardCap).UnixNano())
	if err != nil {
		return 0, fmt.Errorf("Prune: exec: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *SQLiteStore) Counts(ctx context.Context) (Counts, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT state, COUNT(*) FROM jobs GROUP BY state`)
	if err != nil {
		return Counts{}, fmt.Errorf("Counts: query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var c Counts
	for rows.Next() {
		var st State
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return Counts{}, fmt.Errorf("Counts: scan row: %w", err)
		}
		switch st {
		case StateQueued:
			c.Queued = n
		case StateRunning:
			c.Running = n
		case StateSucceeded:
			c.Succeeded = n
		case StateFailed:
			c.Failed = n
		case StateCanceled:
			c.Canceled = n
		}
	}
	return c, rowsErr(rows)
}

// --- row scanning ---

const selectCols = `SELECT id, idempotency_key, source, owner, state, attempts, model, params_json, prompt, result, error, position_hint, created_at, started_at, finished_at, fetched_at`

type scanner interface{ Scan(dest ...any) error }

func scanOne(row scanner) (*Job, error) {
	j, err := scanRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}

func scanRow(row scanner) (*Job, error) {
	var j Job
	var created int64
	var started, finished, fetched sql.NullInt64
	if err := row.Scan(&j.ID, &j.IdempotencyKey, &j.Source, &j.Owner, &j.State, &j.Attempts,
		&j.Model, &j.ParamsJSON, &j.Prompt, &j.Result, &j.Error, &j.PositionHint,
		&created, &started, &finished, &fetched); err != nil {
		return nil, fmt.Errorf("scanRow: scan row: %w", err)
	}
	j.CreatedAt = time.Unix(0, created)
	j.StartedAt = nanoPtr(started)
	j.FinishedAt = nanoPtr(finished)
	j.FetchedAt = nanoPtr(fetched)
	return &j, nil
}

func nanoPtr(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := time.Unix(0, n.Int64)
	return &t
}

func mustAffect(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("mustAffect: rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound // state guard didn't match: Job moved out from under us
	}
	return nil
}

// commit commits tx, saying so in the error.
func commit(tx *sql.Tx) error {
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// rowsErr reports the error that ended a rows iteration, if any.
func rowsErr(rows *sql.Rows) error {
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate rows: %w", err)
	}
	return nil
}
