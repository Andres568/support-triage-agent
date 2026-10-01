package queue_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Andres568/support-triage-agent/internal/dbtest"
	"github.com/Andres568/support-triage-agent/internal/queue"
)

const maxAttempts = 2

// The queue is generic; these tests drive it over the seeded tickets table
// with the smallest possible payload.
var spec = queue.Spec[string]{
	Table:        "tickets",
	Columns:      "t.external_id",
	Dest:         func(s *string) []any { return []any{s} },
	ResultColumn: "decision",
}

var done = map[string]any{"final": map[string]string{"action": "escalate"}}

func setup(t *testing.T) (*pgxpool.Pool, *queue.Queue[string]) {
	t.Helper()
	db := dbtest.Fresh(t)
	return db, queue.New(db, spec, time.Minute, maxAttempts)
}

func startRun(t *testing.T, db *pgxpool.Pool) queue.RunID {
	t.Helper()
	id, err := queue.StartRun(context.Background(), db, queue.Run{Agent: "support", Model: "fake/test", Autonomy: "shadow", PromptSHA: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func claimOne(t *testing.T, q *queue.Queue[string], run queue.RunID) queue.Claimed[string] {
	t.Helper()
	c, err := q.Claim(context.Background(), run, 1)
	if err != nil || len(c) != 1 {
		t.Fatalf("Claim = %v, %v; want one row", c, err)
	}
	return c[0]
}

func expireLease(t *testing.T, db *pgxpool.Pool, id int64) {
	t.Helper()
	if _, err := db.Exec(context.Background(), `UPDATE tickets SET lease_until = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
}

type row struct {
	Status    string
	Attempts  int
	LastError *string
	Leased    bool
}

func readRow(t *testing.T, db *pgxpool.Pool, id int64) row {
	t.Helper()
	var r row
	err := db.QueryRow(context.Background(),
		`SELECT status, attempts, last_error, lease_until IS NOT NULL FROM tickets WHERE id = $1`, id).
		Scan(&r.Status, &r.Attempts, &r.LastError, &r.Leased)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestClaim_OldestFirstAndNotTwice(t *testing.T) {
	db, q := setup(t)
	ctx := context.Background()
	run := startRun(t, db)

	first := claimOne(t, q, run)
	if first.Item != "HD-2001" || first.Attempt != 1 || first.Run != run {
		t.Fatalf("first claim = %+v, want HD-2001 attempt 1 of our run", first)
	}
	rest, err := q.Claim(ctx, run, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 29 {
		t.Fatalf("claimed %d more, want the other 29", len(rest))
	}
	if again, err := q.Claim(ctx, run, 100); err != nil || len(again) != 0 {
		t.Fatalf("Claim with live leases = %v, %v; want nothing", again, err)
	}
}

// Two goroutines of the same run hold different attempts of the same row;
// only the current attempt may finalize.
func TestFinalize_SameRunDifferentAttemptIsFenced(t *testing.T) {
	db, q := setup(t)
	ctx := context.Background()
	run := startRun(t, db)

	stale := claimOne(t, q, run)
	expireLease(t, db, stale.ID)
	current := claimOne(t, q, run)
	if current.ID != stale.ID || current.Attempt != 2 {
		t.Fatalf("reclaim = %+v, want the same row at attempt 2", current)
	}

	if err := q.Finalize(ctx, stale.Lease, "escalated", done, nil); !errors.Is(err, queue.ErrLostLease) {
		t.Fatalf("stale Finalize = %v, want ErrLostLease", err)
	}
	if err := q.Abandon(ctx, stale.Lease, errors.New("late")); !errors.Is(err, queue.ErrLostLease) {
		t.Fatalf("stale Abandon = %v, want ErrLostLease", err)
	}
	if err := q.Finalize(ctx, current.Lease, "escalated", done, nil); err != nil {
		t.Fatalf("current Finalize: %v", err)
	}
	if err := q.Finalize(ctx, current.Lease, "escalated", done, nil); !errors.Is(err, queue.ErrLostLease) {
		t.Fatalf("second Finalize = %v, want ErrLostLease (already final)", err)
	}
	if r := readRow(t, db, current.ID); r.Status != "escalated" || r.Leased {
		t.Fatalf("row = %+v, want escalated without a lease", r)
	}
}

func TestAbandon_BackoffThenFail(t *testing.T) {
	db, q := setup(t)
	ctx := context.Background()
	run := startRun(t, db)

	c := claimOne(t, q, run)
	if err := q.Abandon(ctx, c.Lease, errors.New("model timeout")); err != nil {
		t.Fatal(err)
	}
	r := readRow(t, db, c.ID)
	if r.Status != "claimed" || !r.Leased || r.LastError == nil || *r.LastError != "model timeout" {
		t.Fatalf("after abandon: %+v, want still claimed with last_error", r)
	}

	// Backoff: while the lease runs, nobody can retry the row.
	others, err := q.Claim(ctx, run, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range others {
		if o.ID == c.ID {
			t.Fatal("abandoned row was reclaimed before its lease expired")
		}
	}

	expireLease(t, db, c.ID)
	retry := claimOne(t, q, run)
	if retry.ID != c.ID || retry.Attempt != maxAttempts {
		t.Fatalf("retry = %+v, want row %d at attempt %d", retry, c.ID, maxAttempts)
	}
	if err := q.Abandon(ctx, retry.Lease, errors.New("model timeout again")); err != nil {
		t.Fatal(err)
	}
	if r := readRow(t, db, c.ID); r.Status != "failed" || r.Leased || r.Attempts != maxAttempts {
		t.Fatalf("after last attempt: %+v, want failed without a lease", r)
	}
}

// A worker that crashed on its last attempt never calls Abandon; the row's
// lease just expires. Claim skips it, FailExhausted closes it.
func TestFailExhausted(t *testing.T) {
	db, q := setup(t)
	ctx := context.Background()
	run := startRun(t, db)

	c := claimOne(t, q, run)
	expireLease(t, db, c.ID)
	c = claimOne(t, q, run) // attempt 2 = max
	expireLease(t, db, c.ID)
	live := claimOne(t, q, run) // attempt 1, lease still running

	if n, err := q.FailExhausted(ctx); err != nil || n != 1 {
		t.Fatalf("FailExhausted = %d, %v; want 1", n, err)
	}
	r := readRow(t, db, c.ID)
	if r.Status != "failed" || r.Leased || r.LastError == nil || *r.LastError != "lease expired (attempts exhausted)" {
		t.Fatalf("exhausted row = %+v", r)
	}
	if r := readRow(t, db, live.ID); r.Status != "claimed" {
		t.Fatalf("live lease was touched: %+v", r)
	}
	if n, err := q.FailExhausted(ctx); err != nil || n != 0 {
		t.Fatalf("second FailExhausted = %d, %v; want 0", n, err)
	}
}

func TestAbandon_CapsLastError(t *testing.T) {
	db, q := setup(t)
	ctx := context.Background()
	c := claimOne(t, q, startRun(t, db))
	if err := q.Abandon(ctx, c.Lease, errors.New(strings.Repeat("ü", 4_000))); err != nil {
		t.Fatal(err)
	}
	r := readRow(t, db, c.ID)
	if r.LastError == nil || len(*r.LastError) > queue.MaxLastErrorBytes || !strings.HasSuffix(*r.LastError, "(truncated)") {
		t.Fatalf("last_error not capped: %+v", r)
	}
}
