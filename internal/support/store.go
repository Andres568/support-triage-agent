// Package support is the support agent: it triages tickets and hands
// follow-up work to the orders agent through the tasks outbox.
package support

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Andres568/support-triage-agent/internal/queue"
	"github.com/Andres568/support-triage-agent/internal/tasks"
	"github.com/Andres568/support-triage-agent/internal/triage"
)

// TicketStore is the tickets queue plus the finalize step that writes the
// handoff tasks in the same transaction. It does not expose the queue's raw
// Finalize, so nothing can finalize a ticket without the outbox.
type TicketStore struct {
	q *queue.Queue[triage.Ticket]
}

func NewTicketStore(db *pgxpool.Pool, lease time.Duration, maxAttempts int) *TicketStore {
	return &TicketStore{q: queue.New(db, ticketSpec, lease, maxAttempts)}
}

func (s *TicketStore) Claim(ctx context.Context, run queue.RunID, limit int) ([]queue.Claimed[triage.Ticket], error) {
	return s.q.Claim(ctx, run, limit)
}

func (s *TicketStore) Abandon(ctx context.Context, l queue.Lease, cause error) error {
	return s.q.Abandon(ctx, l, cause)
}

func (s *TicketStore) FailExhausted(ctx context.Context) (int64, error) {
	return s.q.FailExhausted(ctx)
}

// FinalizeTicket stores the record and enqueues its gated follow-ups
// atomically. If the lease was lost, neither happens (queue.ErrLostLease).
// The follow-ups and any auto-reply (Record.Check) are re-validated here,
// at the write boundary: the gate should already guarantee both, but tasks
// and customers must never depend on that alone.
func (s *TicketStore) FinalizeTicket(ctx context.Context, l queue.Lease, rec triage.Record) error {
	status := rec.Status()
	if status == "" {
		return fmt.Errorf("finalize ticket %d: unknown final action %q", l.ID, rec.Final.Action)
	}
	if err := errors.Join(rec.Final.ValidateFollowUps(), rec.Check()); err != nil {
		return fmt.Errorf("finalize ticket %d: %w", l.ID, err)
	}
	return s.q.Finalize(ctx, l, status, rec, func(tx pgx.Tx) error {
		return tasks.Enqueue(ctx, tx, l.ID, rec.Final.FollowUps)
	})
}

var ticketSpec = queue.Spec[triage.Ticket]{
	Table:   "tickets",
	Columns: "t.id, t.external_id, coalesce(t.order_number, ''), t.customer_email, t.subject, t.body, t.sender_verified",
	Dest: func(t *triage.Ticket) []any {
		return []any{&t.ID, &t.ExternalID, &t.OrderNumber, &t.CustomerEmail, &t.Subject, &t.Body, &t.SenderVerified}
	},
	ResultColumn: "decision",
}
