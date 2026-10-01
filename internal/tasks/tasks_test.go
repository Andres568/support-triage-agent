package tasks_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Andres568/support-triage-agent/internal/dbtest"
	"github.com/Andres568/support-triage-agent/internal/handoff"
	"github.com/Andres568/support-triage-agent/internal/queue"
	"github.com/Andres568/support-triage-agent/internal/tasks"
)

func ticketID(t *testing.T, db *pgxpool.Pool, externalID string) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(context.Background(), `SELECT id FROM tickets WHERE external_id = $1`, externalID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func enqueue(t *testing.T, db *pgxpool.Pool, ticketID int64, fus ...handoff.FollowUp) {
	t.Helper()
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error { return tasks.Enqueue(ctx, tx, ticketID, fus) }); err != nil {
		t.Fatal(err)
	}
}

// The task's email must come from the ticket row, so a model cannot point
// the orders agent at another customer. Enqueue does not even take an email.
func TestEnqueue_EmailFromTicketRow(t *testing.T) {
	db := dbtest.Fresh(t)
	ctx := context.Background()
	id := ticketID(t, db, "HD-2005")
	fus := []handoff.FollowUp{
		{Type: handoff.ReprintRequest, Reason: handoff.ReasonDamaged, OrderNumber: "ORD-100105"},
		{Type: handoff.RefundReview, Reason: handoff.ReasonDamaged, OrderNumber: "ORD-100105"},
	}
	// Twice: a retried finalize must not duplicate tasks.
	enqueue(t, db, id, fus...)
	enqueue(t, db, id, fus...)

	rows, _ := db.Query(ctx, `SELECT type, order_number, customer_email, status FROM tasks WHERE ticket_id = $1 ORDER BY type`, id)
	type task struct{ Type, Order, Email, Status string }
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (task, error) {
		var tk task
		return tk, r.Scan(&tk.Type, &tk.Order, &tk.Email, &tk.Status)
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []task{
		{"refund_review", "ORD-100105", "erin@example.com", "pending"},
		{"reprint_request", "ORD-100105", "erin@example.com", "pending"},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("tasks = %+v, want %+v", got, want)
	}
}

// The generic queue works over the tasks table too: its Spec columns scan,
// and "proposed" with a proposal satisfies the table's CHECKs.
func TestSpec_ClaimAndFinalizeProposed(t *testing.T) {
	db := dbtest.Fresh(t)
	ctx := context.Background()
	id := ticketID(t, db, "HD-2005")
	enqueue(t, db, id, handoff.FollowUp{Type: handoff.ReprintRequest, Reason: handoff.ReasonDamaged, OrderNumber: "ORD-100105"})

	run, err := queue.StartRun(ctx, db, queue.Run{Agent: "orders", Model: "fake/test", Autonomy: "suggest", PromptSHA: "test"})
	if err != nil {
		t.Fatal(err)
	}
	q := queue.New(db, tasks.Spec(), time.Minute, 3)
	claimed, err := q.Claim(ctx, run, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("Claim = %+v, %v; want one task", claimed, err)
	}
	c := claimed[0]
	var reported time.Time
	if err := db.QueryRow(ctx, `SELECT created_at FROM tickets WHERE id = $1`, id).Scan(&reported); err != nil {
		t.Fatal(err)
	}
	want := tasks.Task{ID: c.ID, TicketID: id, Ticket: "HD-2005", Type: handoff.ReprintRequest, Reason: handoff.ReasonDamaged,
		OrderNumber: "ORD-100105", CustomerEmail: "erin@example.com", ReportedAt: reported}
	if c.Item != want || c.Attempt != 1 {
		t.Fatalf("claimed %+v attempt %d, want %+v attempt 1", c.Item, c.Attempt, want)
	}

	proposal := map[string]string{"verdict": "eligible"}
	if err := q.Finalize(ctx, c.Lease, "proposed", proposal, nil); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	var status, verdict string
	err = db.QueryRow(ctx, `SELECT status, proposal->>'verdict' FROM tasks WHERE id = $1`, c.ID).Scan(&status, &verdict)
	if err != nil || status != "proposed" || verdict != "eligible" {
		t.Fatalf("task = %s/%s, %v; want proposed/eligible", status, verdict, err)
	}
}
