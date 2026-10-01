// Package queue is a Postgres work queue with leases and a fencing token,
// shared by tickets and tasks. Each row is a small state machine:
//
//	pending → claimed → <final status> | failed
//	             ↑  │
//	             └──┘ lease expired and attempts left: claimable again
//
// Workers claim with FOR UPDATE SKIP LOCKED. Every claim increments attempts,
// and (claimed_by, attempts) is the fencing token: a worker whose lease was
// taken over can never finalize, even if it belongs to the same run.
package queue

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrLostLease means the row is no longer ours: another claim took it over or
// it is already final. The caller's work is discarded, including anything it
// wrote in the finalize transaction.
var ErrLostLease = errors.New("queue: lease lost (reclaimed by another run or already final)")

// RunID is a runs.id uuid. pgx scans and encodes uuids as [16]byte natively,
// so no uuid dependency is needed.
type RunID [16]byte

func (r RunID) String() string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", r[0:4], r[4:6], r[6:8], r[8:10], r[10:16])
}

// Lease identifies one claim of one row. The fencing token is (Run, Attempt).
// Attempt increases on every claim, so two goroutines of the same run cannot
// finalize each other's lease.
type Lease struct {
	ID      int64
	Run     RunID
	Attempt int
}

type Claimed[T any] struct {
	Lease
	Item T
}

// Spec describes one queue table. Table, Columns, the join and ResultColumn are
// compile-time constants chosen by our code, never input; that is why the
// queries below can splice them in with Sprintf.
type Spec[T any] struct {
	Table   string // "tickets" | "tasks"
	Columns string // payload columns returned by Claim, qualified with "t." (or the joined alias)
	// JoinTable and JoinOn optionally add one table to the claim, for
	// payload columns that live there: "tickets k" on "k.id = t.ticket_id".
	JoinTable, JoinOn string
	Dest              func(*T) []any
	ResultColumn      string // jsonb column written by Finalize: "decision" | "proposal"
}

type Queue[T any] struct {
	db          *pgxpool.Pool
	spec        Spec[T]
	lease       time.Duration
	maxAttempts int

	claimSQL, finalizeSQL, abandonSQL, failExhaustedSQL string
}

// resultColumns lists the only tables New accepts and their result column.
// Table and ResultColumn are spliced into SQL, so they must come from here.
var resultColumns = map[string]string{"tickets": "decision", "tasks": "proposal"}

func prefixed(prefix, s string) string {
	if s == "" {
		return ""
	}
	return prefix + s
}

// New panics on a misconfigured Spec or limits: these are programming
// errors, and failing at startup beats a queue that never drains.
func New[T any](db *pgxpool.Pool, s Spec[T], lease time.Duration, maxAttempts int) *Queue[T] {
	if col, ok := resultColumns[s.Table]; !ok || col != s.ResultColumn {
		panic(fmt.Sprintf("queue: unknown table/result column %q/%q", s.Table, s.ResultColumn))
	}
	if lease <= 0 || maxAttempts < 1 {
		panic(fmt.Sprintf("queue: lease %v must be > 0 and maxAttempts %d >= 1", lease, maxAttempts))
	}
	return &Queue[T]{
		db: db, spec: s, lease: lease, maxAttempts: maxAttempts,

		// A row is claimable when pending, or when its lease expired and it
		// still has attempts left. Rows locked by a concurrent claim are
		// skipped, never waited on.
		claimSQL: fmt.Sprintf(`
			WITH picked AS (
			    SELECT id FROM %[1]s
			    WHERE status = 'pending'
			       OR (status = 'claimed' AND lease_until < now() AND attempts < $3)
			    ORDER BY created_at, id
			    LIMIT $2
			    FOR UPDATE SKIP LOCKED)
			UPDATE %[1]s t
			SET status = 'claimed', claimed_by = $1,
			    lease_until = now() + $4::bigint * interval '1 millisecond',
			    attempts = t.attempts + 1
			FROM picked%[3]s WHERE t.id = picked.id%[4]s
			RETURNING t.id, t.attempts, %[2]s`, s.Table, s.Columns, prefixed(", ", s.JoinTable), prefixed(" AND ", s.JoinOn)),

		// No lease_until > now() check on purpose: an expired lease that nobody
		// reclaimed can still finish correctly, because a reclaim would have
		// changed (claimed_by, attempts).
		finalizeSQL: fmt.Sprintf(`
			UPDATE %s SET status = $4, %s = $5, lease_until = NULL, last_error = NULL
			WHERE id = $1 AND status = 'claimed' AND claimed_by = $2 AND attempts = $3`,
			s.Table, s.ResultColumn),

		// The row stays claimed until its lease expires: the retry backoff is
		// the rest of the lease, so an error and a crash recover the same way.
		abandonSQL: fmt.Sprintf(`
			UPDATE %s SET last_error = $4,
			    status      = CASE WHEN attempts >= $5 THEN 'failed' ELSE status END,
			    lease_until = CASE WHEN attempts >= $5 THEN NULL ELSE lease_until END
			WHERE id = $1 AND status = 'claimed' AND claimed_by = $2 AND attempts = $3`, s.Table),

		failExhaustedSQL: fmt.Sprintf(`
			UPDATE %s SET status = 'failed', lease_until = NULL,
			    last_error = coalesce(last_error, 'lease expired') || ' (attempts exhausted)'
			WHERE status = 'claimed' AND lease_until < now() AND attempts >= $1`, s.Table),
	}
}

// Claim leases up to limit rows to run.
func (q *Queue[T]) Claim(ctx context.Context, run RunID, limit int) ([]Claimed[T], error) {
	rows, err := q.db.Query(ctx, q.claimSQL, run, limit, q.maxAttempts, q.lease.Milliseconds())
	if err != nil {
		return nil, fmt.Errorf("claim %s: %w", q.spec.Table, err)
	}
	claimed, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Claimed[T], error) {
		c := Claimed[T]{Lease: Lease{Run: run}}
		err := row.Scan(append([]any{&c.ID, &c.Attempt}, q.spec.Dest(&c.Item)...)...)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("claim %s: %w", q.spec.Table, err)
	}
	return claimed, nil
}

// ErrNotFinal is returned by Finalize for a status that is not an outcome.
var ErrNotFinal = errors.New("queue: finalize needs a final status")

// Finalize records the outcome and runs inTx (e.g. an outbox insert) in the
// same transaction. If the lease was lost it returns ErrLostLease and rolls
// everything back, including inTx's writes.
//
// It clears last_error: that column describes the row's current state (why
// it is waiting for a retry), and a finalized row is not waiting. The history
// of failed attempts belongs to the run records, not to the row.
func (q *Queue[T]) Finalize(ctx context.Context, l Lease, status string, result any, inTx func(pgx.Tx) error) error {
	if status == "pending" || status == "claimed" {
		return fmt.Errorf("finalize %s %d as %q: %w", q.spec.Table, l.ID, status, ErrNotFinal)
	}
	err := pgx.BeginFunc(ctx, q.db, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, q.finalizeSQL, l.ID, l.Run, l.Attempt, status, result)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrLostLease
		}
		if inTx == nil {
			return nil
		}
		return inTx(tx)
	})
	if err != nil {
		return fmt.Errorf("finalize %s %d: %w", q.spec.Table, l.ID, err)
	}
	return nil
}

// Abandon records a retryable failure. The row becomes claimable again when
// its lease expires, or failed right away if it has no attempts left.
func (q *Queue[T]) Abandon(ctx context.Context, l Lease, cause error) error {
	msg := "unknown error"
	if cause != nil {
		msg = cause.Error()
	}
	msg = truncateError(msg)
	tag, err := q.db.Exec(ctx, q.abandonSQL, l.ID, l.Run, l.Attempt, msg, q.maxAttempts)
	if err != nil {
		return fmt.Errorf("abandon %s %d: %w", q.spec.Table, l.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("abandon %s %d: %w", q.spec.Table, l.ID, ErrLostLease)
	}
	return nil
}

// FailExhausted marks expired leases with no attempts left as failed. Claim
// never picks them up, so without this they would stay claimed forever. Run
// it at the start of each batch.
func (q *Queue[T]) FailExhausted(ctx context.Context) (int64, error) {
	tag, err := q.db.Exec(ctx, q.failExhaustedSQL, q.maxAttempts)
	if err != nil {
		return 0, fmt.Errorf("fail exhausted %s: %w", q.spec.Table, err)
	}
	return tag.RowsAffected(), nil
}

// MaxLastErrorBytes caps last_error: an error text can wrap an upstream
// response body or model output, and every read of the row carries it.
const MaxLastErrorBytes = 2048

const truncatedSuffix = "… (truncated)"

// truncateError cuts msg to MaxLastErrorBytes on a rune boundary.
func truncateError(msg string) string {
	if len(msg) <= MaxLastErrorBytes {
		return msg
	}
	cut := MaxLastErrorBytes - len(truncatedSuffix)
	for cut > 0 && !utf8.RuneStart(msg[cut]) {
		cut--
	}
	return msg[:cut] + truncatedSuffix
}
