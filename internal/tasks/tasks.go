// Package tasks is the tasks table: the outbox the support agent writes when
// it finalizes a ticket, and the queue the orders agent claims from.
package tasks

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Andres568/support-triage-agent/internal/handoff"
	"github.com/Andres568/support-triage-agent/internal/queue"
)

// Task is a claimed row of the tasks table.
type Task struct {
	ID, TicketID  int64
	Ticket        string // the ticket's external id, e.g. HD-2005: for logs and eval keys
	Type          handoff.TaskType
	Reason        handoff.Reason
	OrderNumber   string
	CustomerEmail string
	// ReportedAt is when the customer wrote the ticket. Windows such as
	// "report within 30 days of delivery" are measured at this time, not when
	// a worker happens to process the task.
	ReportedAt time.Time
}

// Enqueue inserts the follow-ups of a ticket inside the caller's transaction
// (the ticket's fenced finalize), so tasks exist if and only if the ticket
// was finalized. The customer email is copied from the tickets row by SQL,
// never taken from the model. Retries are harmless: (ticket_id, type) is
// unique and conflicts are ignored.
func Enqueue(ctx context.Context, tx pgx.Tx, ticketID int64, fs []handoff.FollowUp) error {
	const q = `
		INSERT INTO tasks (ticket_id, type, reason, order_number, customer_email)
		SELECT t.id, $2::text, $3::text, $4::text, t.customer_email FROM tickets t WHERE t.id = $1
		ON CONFLICT (ticket_id, type) DO NOTHING`
	for _, f := range fs {
		if _, err := tx.Exec(ctx, q, ticketID, string(f.Type), string(f.Reason), f.OrderNumber); err != nil {
			return fmt.Errorf("enqueue %s for ticket %d: %w", f.Type, ticketID, err)
		}
	}
	return nil
}

// Spec describes the tasks table to the generic queue.
func Spec() queue.Spec[Task] {
	return queue.Spec[Task]{
		Table: "tasks",
		Columns: "t.id, t.ticket_id, k.external_id, t.type, t.reason, " +
			"t.order_number, t.customer_email, k.created_at",
		JoinTable: "tickets k",
		JoinOn:    "k.id = t.ticket_id",
		Dest: func(t *Task) []any {
			return []any{&t.ID, &t.TicketID, &t.Ticket, &t.Type, &t.Reason, &t.OrderNumber, &t.CustomerEmail, &t.ReportedAt}
		},
		ResultColumn: "proposal",
	}
}

// Subject names a task in logs and eval recordings: ticket and type, which
// are unique together (the tasks table's UNIQUE constraint).
func (t Task) Subject() string {
	return t.Ticket + "/" + string(t.Type)
}
