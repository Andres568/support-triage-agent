package orders

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Andres568/support-triage-agent/internal/handoff"
	"github.com/Andres568/support-triage-agent/internal/queue"
	"github.com/Andres568/support-triage-agent/internal/tasks"
)

// StatusProposed is the only final status besides failed: an orders outcome
// is always a proposal for a person. The verdict lives in the proposal.
const StatusProposed = "proposed"

// TaskStore is the tasks queue with the one finalize step the orders agent
// may take.
type TaskStore struct {
	db *pgxpool.Pool
	q  *queue.Queue[tasks.Task]
}

func NewTaskStore(db *pgxpool.Pool, lease time.Duration, maxAttempts int) *TaskStore {
	return &TaskStore{db: db, q: queue.New(db, tasks.Spec(), lease, maxAttempts)}
}

func (s *TaskStore) Claim(ctx context.Context, run queue.RunID, limit int) ([]queue.Claimed[tasks.Task], error) {
	return s.q.Claim(ctx, run, limit)
}

func (s *TaskStore) Abandon(ctx context.Context, l queue.Lease, cause error) error {
	return s.q.Abandon(ctx, l, cause)
}

func (s *TaskStore) FailExhausted(ctx context.Context) (int64, error) {
	return s.q.FailExhausted(ctx)
}

// FinalizeTask stores the outcome of a task of type t, after checking its
// invariants (Outcome.Check). If the lease was lost, nothing is written
// (queue.ErrLostLease).
func (s *TaskStore) FinalizeTask(ctx context.Context, l queue.Lease, t handoff.TaskType, out Outcome) error {
	if err := out.Check(t); err != nil {
		return fmt.Errorf("finalize task %d: %w", l.ID, err)
	}
	return s.q.Finalize(ctx, l, StatusProposed, out, nil)
}

// EligibleDuplicate returns the id of another task for the same order that
// is already proposed as eligible and would do the same job, or 0. Two
// tickets about the same order (HD-2005 and HD-2030 both ask to reprint
// ORD-100105) must not both send the orders team to fulfil it.
//
// Reprints and refunds are one job, compensation: the refunds policy pays a
// refund instead of a reprint, never on top of it. So an eligible reprint
// makes a refund of the same order a duplicate, and the other way round.
//
// Tasks decided concurrently in one batch can both miss each other; a person
// executes every proposal, so that race costs a second look, not a second
// shipment.
func (s *TaskStore) EligibleDuplicate(ctx context.Context, t tasks.Task) (int64, error) {
	types := []string{string(t.Type)}
	if isCompensation(t.Type) {
		types = []string{string(handoff.ReprintRequest), string(handoff.RefundReview)}
	}
	const q = `
		SELECT id FROM tasks
		WHERE order_number = $1 AND type = ANY($2) AND id <> $3
		  AND status = 'proposed' AND proposal->'final'->>'verdict' = 'eligible'
		ORDER BY id LIMIT 1`
	var id int64
	err := s.db.QueryRow(ctx, q, t.OrderNumber, types, t.ID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("duplicate check for task %d: %w", t.ID, err)
	}
	return id, nil
}
