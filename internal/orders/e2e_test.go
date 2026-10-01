package orders_test

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Andres568/support-triage-agent/internal/agent"
	"github.com/Andres568/support-triage-agent/internal/agent/agenttest"
	"github.com/Andres568/support-triage-agent/internal/commerce"
	"github.com/Andres568/support-triage-agent/internal/dbtest"
	"github.com/Andres568/support-triage-agent/internal/handoff"
	"github.com/Andres568/support-triage-agent/internal/obs"
	"github.com/Andres568/support-triage-agent/internal/orders"
	"github.com/Andres568/support-triage-agent/internal/providers"
	"github.com/Andres568/support-triage-agent/internal/queue"
	"github.com/Andres568/support-triage-agent/internal/support"
	"github.com/Andres568/support-triage-agent/internal/tasks"
	"github.com/Andres568/support-triage-agent/internal/triage"
	"github.com/Andres568/support-triage-agent/internal/worker"
)

var workerCfg = func(agentName string) worker.Config {
	return worker.Config{Agent: agentName, Batch: 10, Concurrency: 2, MaxAttempts: 3,
		ItemTimeout: 10 * time.Second, FinalizeTimeout: 5 * time.Second, Lease: time.Minute}
}

// The handoff end to end: a scripted support run creates tasks through the
// outbox, then the orders worker turns them into gated proposals. Both run
// against a real database and the commerce API over HTTP.
//   - HD-2005 reprint ORD-100105 (delivered 5 days ago)  → model eligible, baseline eligible → eligible
//   - HD-2017 reprint ORD-100107 (delivered 45 days ago) → model eligible, baseline not     → needs_human
//   - HD-2012 refund  ORD-100115 (80 USD, delivered)     → model eligible, baseline eligible → eligible
//   - HD-2030 reprint ORD-100105 again (misprint)          → model eligible, baseline eligible → whichever
//     of HD-2005 and HD-2030 is decided second is needs_human, a duplicate
func TestWorkerRun_OrdersEndToEnd(t *testing.T) {
	db := dbtest.Fresh(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `DELETE FROM tickets WHERE external_id NOT IN ('HD-2005', 'HD-2012', 'HD-2017', 'HD-2030')`); err != nil {
		t.Fatal(err)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	api := httptest.NewServer(commerce.NewHandler(commerce.NewStore(db), quiet))
	t.Cleanup(api.Close)
	client := commerce.NewClient(api.URL)

	runSupport(t, db, client, quiet)

	proposal := func(p orders.Proposal) *agenttest.Script {
		return agenttest.NewScript(agenttest.Call("c1", "get_task_order", nil), agenttest.Call("c2", orders.SubmitProposalTool, p))
	}
	router := agenttest.Router{Scripts: map[string]*agenttest.Script{
		"HD-2005/reprint_request": proposal(orders.Proposal{Verdict: orders.Eligible, PolicySlug: "reprint-damaged", Reason: "broken on arrival",
			ReprintItems: []orders.ReprintItem{{SKU: "MUG-CER-11OZ", Qty: 250}}}),
		"HD-2017/reprint_request": proposal(orders.Proposal{Verdict: orders.Eligible, PolicySlug: "reprint-damaged", Reason: "water damage",
			ReprintItems: []orders.ReprintItem{{SKU: "PRT-ART-A4", Qty: 40}}}),
		"HD-2030/reprint_request": proposal(orders.Proposal{Verdict: orders.Eligible, PolicySlug: "reprint-damaged", Reason: "smudged",
			ReprintItems: []orders.ReprintItem{{SKU: "MUG-CER-11OZ", Qty: 250}}}),
		"HD-2012/refund_review": proposal(orders.Proposal{Verdict: orders.Eligible, PolicySlug: "refunds", Reason: "peeling; prefers a refund",
			ReprintItems: []orders.ReprintItem{}, RefundCents: 8000}),
	}}
	cfg := workerCfg("orders")
	cfg.Concurrency = 1 // sequential, so the duplicate check sees the first proposal
	store := orders.NewTaskStore(db, cfg.Lease, cfg.MaxAttempts)
	h := &orders.Handler{Store: store, Commerce: client, ModelFor: router.ModelFor, ModelID: "fake/test",
		RefundLimitCents: triage.EvalPolicy().RefundLimitCents, Limits: agent.Config{MaxSteps: 4}, FinalizeTimeout: cfg.FinalizeTimeout}
	run, err := queue.StartRun(ctx, db, queue.Run{Agent: "orders", Model: "fake/test", Autonomy: "suggest", PromptSHA: orders.PromptSHA})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := worker.Run(ctx, cfg, store, run, h, &obs.Factory{DB: db, Spec: providers.ModelSpec{ID: "fake/test"}}, quiet)
	if err != nil || stats != (worker.Stats{Claimed: 4, Finalized: 4}) {
		t.Fatalf("stats = %+v, err = %v; want 4 claimed and finalized", stats, err)
	}

	type row struct{ Status, Source, Proposed, Final, Baseline, Overrides string }
	want := map[string]row{
		"HD-2005/reprint_request": {"proposed", "agent", "eligible", "eligible", "eligible", ""},
		"HD-2030/reprint_request": {"proposed", "agent", "eligible", "needs_human", "eligible", "duplicate of task"},
		"HD-2017/reprint_request": {"proposed", "agent", "eligible", "needs_human", "not_eligible", "disagrees"},
		"HD-2012/refund_review":   {"proposed", "agent", "eligible", "eligible", "eligible", ""},
	}
	rows, err := db.Query(ctx, `
		SELECT t.external_id || '/' || k.type, k.status, k.proposal->>'source', k.proposal->'proposed'->>'verdict',
		       k.proposal->'final'->>'verdict', k.proposal->>'baseline', coalesce(k.proposal->'overrides'->>0, '')
		FROM tasks k JOIN tickets t ON t.id = k.ticket_id WHERE k.claimed_by = $1`, run)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]row{}
	for rows.Next() {
		var id string
		var r row
		if err := rows.Scan(&id, &r.Status, &r.Source, &r.Proposed, &r.Final, &r.Baseline, &r.Overrides); err != nil {
			t.Fatal(err)
		}
		got[id] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Errorf("tasks = %+v, want %d", got, len(want))
	}
	// The support run enqueues HD-2005 and HD-2030 in either order; the one
	// decided second is the duplicate.
	if a, b := "HD-2005/reprint_request", "HD-2030/reprint_request"; got[b].Final == "eligible" {
		want[a], want[b] = want[b], want[a]
	}
	for id, w := range want {
		g := got[id]
		if w.Overrides != "" && strings.Contains(g.Overrides, w.Overrides) {
			g.Overrides = w.Overrides
		}
		if g != w {
			t.Errorf("%s = %+v, want %+v", id, got[id], w)
		}
	}

	// One run item per task, with the finalized status and two steps
	// (the proposal loop's two model calls) plus the tool call.
	var items, proposed, steps int
	if err := db.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE outcome = 'proposed' AND source = 'agent' AND subject_kind = 'task'),
		       (SELECT count(*) FROM run_steps s JOIN run_items i ON i.id = s.run_item_id WHERE i.run_id = $1)
		FROM run_items WHERE run_id = $1`, run).Scan(&items, &proposed, &steps); err != nil {
		t.Fatal(err)
	}
	if items != 4 || proposed != 4 || steps != 4*3 {
		t.Errorf("run_items: %d items, %d proposed by the agent, %d steps; want 4, 4, 12", items, proposed, steps)
	}
}

// runSupport drafts the four tickets with one follow-up each.
func runSupport(t *testing.T, db *pgxpool.Pool, client *commerce.Client, log *slog.Logger) {
	t.Helper()
	draft := func(num string, cat triage.Category, typ handoff.TaskType, reason handoff.Reason) *agenttest.Script {
		return agenttest.NewScript(
			agenttest.Call("c1", "get_order", map[string]string{"order_number": num}),
			agenttest.Submit("c2", triage.Decision{Category: cat, Action: triage.ActionDraftForReview,
				DraftReply: "A teammate will review this.", Confidence: 0.9, Reason: "test",
				FollowUps: []handoff.FollowUp{{Type: typ, Reason: reason, OrderNumber: num}}}),
		)
	}
	router := agenttest.Router{Scripts: map[string]*agenttest.Script{
		"HD-2005": draft("ORD-100105", triage.CategoryDamagedOrMisprint, handoff.ReprintRequest, handoff.ReasonDamaged),
		"HD-2030": draft("ORD-100105", triage.CategoryDamagedOrMisprint, handoff.ReprintRequest, handoff.ReasonMisprint),
		"HD-2017": draft("ORD-100107", triage.CategoryDamagedOrMisprint, handoff.ReprintRequest, handoff.ReasonDamaged),
		"HD-2012": draft("ORD-100115", triage.CategoryRefundRequest, handoff.RefundReview, handoff.ReasonDamaged),
	}}
	cfg := workerCfg("support")
	store := support.NewTicketStore(db, cfg.Lease, cfg.MaxAttempts)
	h := &support.Handler{Store: store, Commerce: client, ModelFor: router.ModelFor, ModelID: "fake/test",
		Policy: triage.PolicyFor(triage.AutonomyShadow, nil), Limits: agent.Config{MaxSteps: 4}, FinalizeTimeout: cfg.FinalizeTimeout}
	ctx := context.Background()
	run, err := queue.StartRun(ctx, db, queue.Run{Agent: "support", Model: "fake/test", Autonomy: "shadow", PromptSHA: support.PromptSHA})
	if err != nil {
		t.Fatal(err)
	}
	if stats, err := worker.Run(ctx, cfg, store, run, h, nil, log); err != nil || stats.Finalized != 4 {
		t.Fatalf("support stats = %+v, err = %v", stats, err)
	}
}

// A refund is paid instead of a reprint, never on top of it: an eligible
// reprint makes a refund of the same order a duplicate, and the other way
// round. An address change is a different job.
func TestEligibleDuplicate_CompensationIsOneJob(t *testing.T) {
	db := dbtest.Fresh(t)
	ctx := context.Background()
	var reprint, refund, address int64
	if err := db.QueryRow(ctx, `
		WITH k AS (SELECT id, customer_email FROM tickets WHERE external_id = 'HD-2005'),
		ins AS (
		    INSERT INTO tasks (ticket_id, type, reason, order_number, customer_email, status, proposal)
		    SELECT k.id, v.type, v.reason, 'ORD-100105', k.customer_email, v.status, v.proposal::jsonb
		    FROM k, (VALUES
		        ('reprint_request', 'damaged',         'proposed', '{"final":{"verdict":"eligible"}}'),
		        ('refund_review',   'damaged',         'pending',  NULL),
		        ('address_change',  'customer_request', 'pending', NULL)) AS v(type, reason, status, proposal)
		    RETURNING id, type)
		SELECT (SELECT id FROM ins WHERE type = 'reprint_request'), (SELECT id FROM ins WHERE type = 'refund_review'),
		       (SELECT id FROM ins WHERE type = 'address_change')`).Scan(&reprint, &refund, &address); err != nil {
		t.Fatal(err)
	}
	store := orders.NewTaskStore(db, time.Minute, 3)
	for _, tt := range []struct {
		task tasks.Task
		want int64
	}{
		{tasks.Task{ID: refund, Type: handoff.RefundReview, OrderNumber: "ORD-100105"}, reprint},
		{tasks.Task{ID: address, Type: handoff.AddressChange, OrderNumber: "ORD-100105"}, 0},
		{tasks.Task{ID: reprint, Type: handoff.ReprintRequest, OrderNumber: "ORD-100105"}, 0}, // not its own duplicate
	} {
		if got, err := store.EligibleDuplicate(ctx, tt.task); err != nil || got != tt.want {
			t.Errorf("%s: duplicate = %d, err = %v; want %d", tt.task.Type, got, err, tt.want)
		}
	}
}
