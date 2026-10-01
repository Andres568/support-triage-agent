package support

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Andres568/support-triage-agent/internal/agent"
	"github.com/Andres568/support-triage-agent/internal/agent/agenttest"
	"github.com/Andres568/support-triage-agent/internal/commerce"
	"github.com/Andres568/support-triage-agent/internal/dbtest"
	"github.com/Andres568/support-triage-agent/internal/handoff"
	"github.com/Andres568/support-triage-agent/internal/obs"
	"github.com/Andres568/support-triage-agent/internal/providers"
	"github.com/Andres568/support-triage-agent/internal/queue"
	"github.com/Andres568/support-triage-agent/internal/triage"
	"github.com/Andres568/support-triage-agent/internal/worker"
)

// The whole support pipeline over a real database and the commerce API over
// HTTP; only the model is scripted. Three tickets cover the three paths:
//   - HD-2005: tool call, verified reprint follow-up          → drafted + task
//   - HD-2012: refund follow-up plus an injected follow-up for
//     another customer's order                                → drafted + only the refund task
//   - HD-2020: "chargeback" pre-check                         → escalated, model never called
func TestWorkerRun_SupportEndToEnd(t *testing.T) {
	db := dbtest.Fresh(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `DELETE FROM tickets WHERE external_id NOT IN ('HD-2005', 'HD-2012', 'HD-2020')`); err != nil {
		t.Fatal(err)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	api := httptest.NewServer(commerce.NewHandler(commerce.NewStore(db), quiet))
	t.Cleanup(api.Close)

	refund := handoff.FollowUp{Type: handoff.RefundReview, Reason: handoff.ReasonDamaged, OrderNumber: "ORD-100115"}
	injected := handoff.FollowUp{Type: handoff.AddressChange, Reason: handoff.ReasonCustomerRequest, OrderNumber: "ORD-100105"} // erin's order, not oscar's
	router := agenttest.Router{Scripts: map[string]*agenttest.Script{
		"HD-2005": agenttest.NewScript(
			agenttest.Call("c1", "get_order", map[string]string{"order_number": "ORD-100105"}),
			agenttest.Submit("c2", decision(triage.CategoryDamagedOrMisprint, triage.ActionDraftForReview, reprint105)),
		),
		"HD-2012": agenttest.NewScript(
			agenttest.Submit("c1", decision(triage.CategoryRefundRequest, triage.ActionDraftForReview, refund, injected)),
		),
	}}

	cfg := worker.Config{Agent: "support", Batch: 10, Concurrency: 2, MaxAttempts: 3,
		ItemTimeout: 10 * time.Second, FinalizeTimeout: 5 * time.Second, Lease: time.Minute}
	store := NewTicketStore(db, cfg.Lease, cfg.MaxAttempts)
	h := &Handler{
		Store: store, Commerce: commerce.NewClient(api.URL), ModelFor: router.ModelFor, ModelID: "fake/test",
		Policy: triage.PolicyFor(triage.AutonomyShadow, nil), Limits: agent.Config{MaxSteps: 4},
		FinalizeTimeout: cfg.FinalizeTimeout,
	}
	run, err := queue.StartRun(ctx, db, queue.Run{Agent: "support", Model: "fake/test", Autonomy: "shadow", PromptSHA: PromptSHA})
	if err != nil {
		t.Fatal(err)
	}

	stats, err := worker.Run(ctx, cfg, store, run, h, &obs.Factory{DB: db, Spec: testSpec}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if stats != (worker.Stats{Claimed: 3, Finalized: 3}) {
		t.Errorf("stats = %+v, want 3 claimed and finalized", stats)
	}

	type ticketRow struct {
		Status, Source, Action, Autonomy string
		FollowUps                        int
		HasProposed                      bool
	}
	want := map[string]ticketRow{
		"HD-2005": {"drafted", "agent", "draft_for_review", "shadow", 1, true},
		"HD-2012": {"drafted", "agent", "draft_for_review", "shadow", 1, true},
		"HD-2020": {"escalated", "precheck", "escalate", "shadow", 0, false},
	}
	got := map[string]ticketRow{}
	rows, err := db.Query(ctx, `
		SELECT external_id, status, decision->>'source', decision->'final'->>'action', decision->>'autonomy',
		       jsonb_array_length(decision->'final'->'follow_ups'), decision ? 'proposed'
		FROM tickets WHERE claimed_by = $1`, run)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var r ticketRow
		if err := rows.Scan(&id, &r.Status, &r.Source, &r.Action, &r.Autonomy, &r.FollowUps, &r.HasProposed); err != nil {
			t.Fatal(err)
		}
		got[id] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s = %+v, want %+v", id, got[id], w)
		}
	}

	var overrides string
	if err := db.QueryRow(ctx, `SELECT decision->>'overrides' FROM tickets WHERE external_id = 'HD-2012'`).Scan(&overrides); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(overrides, "order ORD-100105 not verified for sender") {
		t.Error("HD-2012 has no override for the dropped injected follow-up")
	}

	type task struct{ Ticket, Type, Order, Email string }
	trows, err := db.Query(ctx, `
		SELECT t.external_id, k.type, k.order_number, k.customer_email
		FROM tasks k JOIN tickets t ON t.id = k.ticket_id ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := pgx.CollectRows(trows, func(r pgx.CollectableRow) (task, error) {
		var k task
		return k, r.Scan(&k.Ticket, &k.Type, &k.Order, &k.Email)
	})
	if err != nil {
		t.Fatal(err)
	}
	wantTasks := []task{
		{"HD-2005", "reprint_request", "ORD-100105", "erin@example.com"},
		{"HD-2012", "refund_review", "ORD-100115", "oscar@example.com"},
	}
	if len(tasks) != len(wantTasks) || tasks[0] != wantTasks[0] || tasks[1] != wantTasks[1] {
		t.Errorf("tasks = %+v, want %+v", tasks, wantTasks)
	}

	// Observability rows: one item per ticket, one step per model or tool
	// call, cost frozen from testSpec's price (scripted calls bill 100 in +
	// 20 out: 100 + 40 micro-USD each).
	var items, steps, cost int64
	if err := db.QueryRow(ctx, `SELECT count(*), sum(steps), sum(cost_micros) FROM run_items WHERE run_id = $1`, run).
		Scan(&items, &steps, &cost); err != nil {
		t.Fatal(err)
	}
	if items != 3 || steps != 3 || cost != 3*140 {
		t.Errorf("run_items: %d items, %d steps, %d micros; want 3, 3, 420", items, steps, cost)
	}
	var trail, outcome string
	if err := db.QueryRow(ctx, `
		SELECT string_agg(s.kind || ':' || s.name, ',' ORDER BY s.seq), min(i.outcome)
		FROM run_steps s JOIN run_items i ON i.id = s.run_item_id JOIN tickets t ON t.id = i.subject_id
		WHERE i.subject_kind = 'ticket' AND t.external_id = 'HD-2005'`).Scan(&trail, &outcome); err != nil {
		t.Fatal(err)
	}
	if trail != "model:fake/test,tool:get_order,model:fake/test" || outcome != "drafted" {
		t.Errorf("HD-2005 steps = %q, outcome %q", trail, outcome)
	}
}

var testSpec = providers.ModelSpec{ID: "fake/test", Provider: providers.Fake,
	Price: providers.Price{InputMicrosPerMTok: 1_000_000, OutputMicrosPerMTok: 2_000_000}}

// Telemetry is best-effort: a run_items write that fails (here, a closed
// pool) is logged, and the ticket is still finalized.
func TestHandle_FinalizesWhenRunItemsWriteFails(t *testing.T) {
	db := dbtest.Fresh(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `DELETE FROM tickets WHERE external_id <> 'HD-2020'`); err != nil {
		t.Fatal(err)
	}
	closed, err := pgxpool.New(ctx, db.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := worker.Config{Agent: "support", Batch: 1, Concurrency: 1, MaxAttempts: 3,
		ItemTimeout: 10 * time.Second, FinalizeTimeout: 5 * time.Second, Lease: time.Minute}
	store := NewTicketStore(db, cfg.Lease, cfg.MaxAttempts)
	h := &Handler{
		Store: store, ModelFor: agenttest.Router{}.ModelFor, ModelID: "fake/test",
		Policy: triage.PolicyFor(triage.AutonomyShadow, nil), Limits: agent.Config{MaxSteps: 4},
		FinalizeTimeout: cfg.FinalizeTimeout,
	}
	run, err := queue.StartRun(ctx, db, queue.Run{Agent: "support", Model: "fake/test", Autonomy: "shadow", PromptSHA: PromptSHA})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := worker.Run(ctx, cfg, store, run, h, &obs.Factory{DB: closed, Spec: testSpec, Log: quiet}, quiet)
	if err != nil || stats != (worker.Stats{Claimed: 1, Finalized: 1}) {
		t.Fatalf("stats = %+v, err %v; want 1 claimed and finalized", stats, err)
	}
	var status string
	var items int
	if err := db.QueryRow(ctx, `SELECT status, (SELECT count(*) FROM run_items) FROM tickets WHERE external_id = 'HD-2020'`).
		Scan(&status, &items); err != nil {
		t.Fatal(err)
	}
	if status != "escalated" || items != 0 {
		t.Errorf("status %q with %d run_items; want escalated and none", status, items)
	}
}

// The worker records items the handler could not finalize: a decide error
// and a panic both leave an abandoned row with the error.
func TestWorkerRun_RecordsFailedItems(t *testing.T) {
	db := dbtest.Fresh(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `DELETE FROM tickets WHERE external_id NOT IN ('HD-2005', 'HD-2012')`); err != nil {
		t.Fatal(err)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := worker.Config{Agent: "support", Batch: 2, Concurrency: 1, MaxAttempts: 3,
		ItemTimeout: 10 * time.Second, FinalizeTimeout: 5 * time.Second, Lease: time.Minute}
	store := NewTicketStore(db, cfg.Lease, cfg.MaxAttempts)
	modelFor := func(subject string, _ int) agent.Model {
		return modelFunc(func(context.Context, agent.Request) (agent.Response, error) {
			if subject == "HD-2005" {
				panic("model exploded")
			}
			return agent.Response{}, errors.New("provider down")
		})
	}
	h := &Handler{Store: store, Commerce: newFakeCommerce(), ModelFor: modelFor, ModelID: "fake/test",
		Policy: triage.PolicyFor(triage.AutonomyShadow, nil), Limits: agent.Config{MaxSteps: 4}, FinalizeTimeout: cfg.FinalizeTimeout}
	run, err := queue.StartRun(ctx, db, queue.Run{Agent: "support", Model: "fake/test", Autonomy: "shadow", PromptSHA: PromptSHA})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := worker.Run(ctx, cfg, store, run, h, &obs.Factory{DB: db, Spec: testSpec}, quiet)
	if err != nil || stats != (worker.Stats{Claimed: 2, Abandoned: 2}) {
		t.Fatalf("stats = %+v, err %v; want 2 claimed and abandoned", stats, err)
	}
	rows, err := db.Query(ctx, `
		SELECT t.external_id || ' ' || i.outcome || ' ' || i.error
		FROM run_items i JOIN tickets t ON t.id = i.subject_id WHERE i.run_id = $1 ORDER BY 1`, run)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"HD-2005 abandoned handler panic: model exploded", "HD-2012 abandoned agent: model call 1: provider down"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("run_items =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A ticket inserted without sender_verified is unverified (migration 00015):
// the same decision that hands HD-2005's reprint to the orders agent becomes
// a human draft with no follow-ups. Side by side with the verified original,
// this also proves the store reads the column.
func TestWorkerRun_UnverifiedSenderGetsDraftWithoutFollowUps(t *testing.T) {
	db := dbtest.Fresh(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `DELETE FROM tickets WHERE external_id <> 'HD-2005'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO tickets (external_id, order_number, customer_email, subject, body)
		SELECT 'HD-9005', order_number, customer_email, subject, body FROM tickets WHERE external_id = 'HD-2005'`); err != nil {
		t.Fatal(err)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	api := httptest.NewServer(commerce.NewHandler(commerce.NewStore(db), quiet))
	t.Cleanup(api.Close)
	script := func() *agenttest.Script {
		return agenttest.NewScript(
			agenttest.Call("c1", "get_order", map[string]string{"order_number": "ORD-100105"}),
			agenttest.Submit("c2", decision(triage.CategoryDamagedOrMisprint, triage.ActionDraftForReview, reprint105)),
		)
	}
	router := agenttest.Router{Scripts: map[string]*agenttest.Script{"HD-2005": script(), "HD-9005": script()}}

	cfg := worker.Config{Agent: "support", Batch: 10, Concurrency: 1, MaxAttempts: 3,
		ItemTimeout: 10 * time.Second, FinalizeTimeout: 5 * time.Second, Lease: time.Minute}
	store := NewTicketStore(db, cfg.Lease, cfg.MaxAttempts)
	h := &Handler{
		Store: store, Commerce: commerce.NewClient(api.URL), ModelFor: router.ModelFor, ModelID: "fake/test",
		Policy: triage.PolicyFor(triage.AutonomyShadow, nil), Limits: agent.Config{MaxSteps: 4},
		FinalizeTimeout: cfg.FinalizeTimeout,
	}
	run, err := queue.StartRun(ctx, db, queue.Run{Agent: "support", Model: "fake/test", Autonomy: "shadow", PromptSHA: PromptSHA})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.Run(ctx, cfg, store, run, h, &obs.Factory{DB: db, Spec: testSpec}, quiet); err != nil {
		t.Fatal(err)
	}

	type row struct {
		Verified       bool
		Status, Action string
		FollowUps      int
		Tasks          int
	}
	want := map[string]row{
		"HD-2005": {true, "drafted", "draft_for_review", 1, 1},
		"HD-9005": {false, "drafted", "draft_for_review", 0, 0},
	}
	for id, w := range want {
		var g row
		err := db.QueryRow(ctx, `
			SELECT t.sender_verified, t.status, t.decision->'final'->>'action',
			       jsonb_array_length(t.decision->'final'->'follow_ups'),
			       (SELECT count(*) FROM tasks k WHERE k.ticket_id = t.id)
			FROM tickets t WHERE external_id = $1`, id).Scan(&g.Verified, &g.Status, &g.Action, &g.FollowUps, &g.Tasks)
		if err != nil {
			t.Fatal(err)
		}
		if g != w {
			t.Errorf("%s = %+v, want %+v", id, g, w)
		}
	}
}
