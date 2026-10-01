package support

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Andres568/support-triage-agent/internal/dbtest"
	"github.com/Andres568/support-triage-agent/internal/handoff"
	"github.com/Andres568/support-triage-agent/internal/queue"
	"github.com/Andres568/support-triage-agent/internal/tasks"
	"github.com/Andres568/support-triage-agent/internal/triage"
)

const seededTickets = 30

func startRun(t *testing.T, db *pgxpool.Pool) queue.RunID {
	t.Helper()
	id, err := queue.StartRun(context.Background(), db, queue.Run{Agent: "support", Model: "fake/test", Autonomy: "shadow", PromptSHA: "test"})
	if err != nil {
		t.Error(err) // Error, not Fatal: also called from goroutines
	}
	return id
}

// draft is a record whose follow-ups are already gated.
func draft(fus ...handoff.FollowUp) triage.Record {
	return triage.Record{
		Source:   triage.SourceAgent,
		Final:    triage.Decision{Category: triage.CategoryGeneral, Action: triage.ActionDraftForReview, DraftReply: "x", Confidence: 0.9, Reason: "test", FollowUps: append([]handoff.FollowUp{}, fus...)},
		Autonomy: triage.AutonomyShadow,
	}
}

func count(t *testing.T, db *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Eight workers race over the seeded queue, like a scheduler firing while a
// previous batch still runs. Every ticket must be finalized exactly once and
// every follow-up enqueued exactly once.
func TestClaim_ConcurrentWorkersFinalizeEachTicketOnce(t *testing.T) {
	db := dbtest.Fresh(t)
	store := NewTicketStore(db, time.Minute, 3)
	ctx := context.Background()

	const workers = 8
	var (
		mu        sync.Mutex
		finalized = map[int64]int{}
		wantTasks int
		wg        sync.WaitGroup
		start     = make(chan struct{})
	)
	// Runs are created up front and all workers are released together, so
	// setup time cannot let one worker drain the queue alone.
	runs := make([]queue.RunID, workers)
	for i := range runs {
		runs[i] = startRun(t, db)
	}
	for _, run := range runs {
		wg.Go(func() {
			<-start
			for {
				claimed, err := store.Claim(ctx, run, 1)
				if err != nil {
					t.Error(err)
					return
				}
				if len(claimed) == 0 {
					return
				}
				c := claimed[0]
				rec := draft()
				if handoff.OrderNumberRE.MatchString(c.Item.OrderNumber) {
					rec = draft(handoff.FollowUp{Type: handoff.ReprintRequest, Reason: handoff.ReasonDamaged, OrderNumber: c.Item.OrderNumber})
				}
				if err := store.FinalizeTicket(ctx, c.Lease, rec); err != nil {
					t.Errorf("finalize %s: %v", c.Item.ExternalID, err)
					return
				}
				mu.Lock()
				finalized[c.ID]++
				wantTasks += len(rec.Final.FollowUps)
				mu.Unlock()
			}
		})
	}
	close(start)
	wg.Wait()

	// Otherwise the test proved nothing about concurrency.
	if n := count(t, db, `SELECT count(DISTINCT claimed_by) FROM tickets`); n < 2 {
		t.Errorf("tickets were finalized by %d run(s), want several racing runs", n)
	}
	if len(finalized) != seededTickets {
		t.Errorf("finalized %d distinct tickets, want %d", len(finalized), seededTickets)
	}
	for id, n := range finalized {
		if n != 1 {
			t.Errorf("ticket %d finalized %d times", id, n)
		}
	}
	if n := count(t, db, `SELECT count(*) FROM tickets WHERE status <> 'drafted'`); n != 0 {
		t.Errorf("%d tickets not drafted", n)
	}
	if n := count(t, db, `SELECT count(*) FROM tasks`); n != wantTasks || wantTasks == 0 {
		t.Errorf("tasks = %d, want %d (> 0)", n, wantTasks)
	}
}

// Run A's lease expires and run B takes the ticket over. A's late finalize
// fails the fence before its outbox insert runs, so a lost lease writes no
// outbox rows. (Rollback of outbox rows written before an error is covered
// by TestFinalize_InTxErrorRollsBackTicket.)
func TestFinalize_LostLeaseWritesNoOutboxRows(t *testing.T) {
	db := dbtest.Fresh(t)
	store := NewTicketStore(db, time.Minute, 3)
	ctx := context.Background()
	runA, runB := startRun(t, db), startRun(t, db)

	a, err := store.Claim(ctx, runA, 1)
	if err != nil || len(a) != 1 {
		t.Fatalf("A claim = %v, %v", a, err)
	}
	x := a[0]
	if _, err := db.Exec(ctx, `UPDATE tickets SET lease_until = now() - interval '1 second' WHERE id = $1`, x.ID); err != nil {
		t.Fatal(err)
	}
	b, err := store.Claim(ctx, runB, 1)
	if err != nil || len(b) != 1 || b[0].ID != x.ID || b[0].Attempt != 2 {
		t.Fatalf("B claim = %+v, %v; want ticket %d at attempt 2", b, err, x.ID)
	}

	orderB := handoff.FollowUp{Type: handoff.ReprintRequest, Reason: handoff.ReasonDamaged, OrderNumber: "ORD-100103"}
	if err := store.FinalizeTicket(ctx, b[0].Lease, draft(orderB)); err != nil {
		t.Fatalf("B finalize: %v", err)
	}
	orderA := handoff.FollowUp{Type: handoff.AddressChange, Reason: handoff.ReasonCustomerRequest, OrderNumber: "ORD-100103"}
	if err := store.FinalizeTicket(ctx, x.Lease, draft(orderB, orderA)); !errors.Is(err, queue.ErrLostLease) {
		t.Fatalf("A finalize = %v, want ErrLostLease", err)
	}

	if n := count(t, db, `SELECT count(*) FROM tasks WHERE ticket_id = $1`, x.ID); n != 1 {
		t.Errorf("tasks for ticket = %d, want only B's", n)
	}
	if n := count(t, db, `SELECT count(*) FROM tasks WHERE ticket_id = $1 AND type = 'address_change'`, x.ID); n != 0 {
		t.Error("A's follow-up survived the lost lease")
	}
	if n := count(t, db, `SELECT count(*) FROM tickets WHERE id = $1 AND claimed_by = $2 AND status = 'drafted'`, x.ID, runB); n != 1 {
		t.Error("ticket is not B's drafted outcome")
	}
}

// The ticket update runs first in the finalize transaction; if the outbox
// step fails after writing, both must roll back and the lease stays usable.
func TestFinalize_InTxErrorRollsBackTicket(t *testing.T) {
	db := dbtest.Fresh(t)
	store := NewTicketStore(db, time.Minute, 3)
	ctx := context.Background()
	run := startRun(t, db)

	claimed, err := store.Claim(ctx, run, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	c := claimed[0]
	boom := errors.New("outbox failed")
	err = store.q.Finalize(ctx, c.Lease, "drafted", draft(), func(tx pgx.Tx) error {
		fu := handoff.FollowUp{Type: handoff.ReprintRequest, Reason: handoff.ReasonDamaged, OrderNumber: "ORD-100101"}
		if err := tasks.Enqueue(ctx, tx, c.ID, []handoff.FollowUp{fu}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Finalize = %v, want the inTx error", err)
	}

	q := `SELECT count(*) FROM tickets WHERE id = $1 AND status = 'claimed' AND claimed_by = $2 AND attempts = $3 AND decision IS NULL`
	if n := count(t, db, q, c.ID, run, c.Attempt); n != 1 {
		t.Error("ticket is no longer claimed by the same attempt with no decision")
	}
	if n := count(t, db, `SELECT count(*) FROM tasks`); n != 0 {
		t.Errorf("tasks = %d, want 0", n)
	}
	// The lease survived, so the same attempt can still finalize.
	if err := store.FinalizeTicket(ctx, c.Lease, draft()); err != nil {
		t.Fatalf("retry FinalizeTicket: %v", err)
	}
}

// The gate drops follow-ups on escalation, but the store must not rely on it.
// No database: the record is rejected before any query.
func TestFinalizeTicket_RejectsInvalidFollowUps(t *testing.T) {
	store := NewTicketStore(nil, time.Minute, 3)
	reprint := handoff.FollowUp{Type: handoff.ReprintRequest, Reason: handoff.ReasonDamaged, OrderNumber: "ORD-100105"}

	escalated := draft(reprint)
	escalated.Final.Action = triage.ActionEscalate
	badNumber := draft(handoff.FollowUp{Type: handoff.ReprintRequest, Reason: handoff.ReasonDamaged, OrderNumber: "ord-100105"})
	unknownType := draft(handoff.FollowUp{Type: "cancel_order", OrderNumber: "ORD-100105"})
	missing := draft()
	missing.Final.FollowUps = nil
	shadowAuto := draft()
	shadowAuto.Final.Action, shadowAuto.Autonomy = triage.ActionAutoReply, triage.AutonomyShadow

	for name, rec := range map[string]triage.Record{
		"escalate with follow-ups": escalated,
		"bad order number":         badNumber,
		"unknown type":             unknownType,
		"nil follow-ups":           missing,
		"auto-reply in shadow":     shadowAuto, // Record.Check
	} {
		if err := store.FinalizeTicket(context.Background(), queue.Lease{ID: 1}, rec); err == nil {
			t.Errorf("%s: FinalizeTicket accepted it", name)
		}
	}
}
