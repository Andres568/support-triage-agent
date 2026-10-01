package evals

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Andres568/support-triage-agent/internal/agent"
	"github.com/Andres568/support-triage-agent/internal/agent/replay"
	"github.com/Andres568/support-triage-agent/internal/commerce"
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

// EvalDBPrefix is what the eval database's name must start with: the
// harness rewrites queue state, so it refuses any other database.
const EvalDBPrefix = "triage_eval"

// Options configure one eval run.
type Options struct {
	Agent   string // support | orders
	ModelID string // registry id
	// Model answers every item; nil for the oracle, which is built per item
	// from its golden label.
	Model  agent.Model
	Policy triage.Policy // support only; orders is fixed at suggest

	Golden      []Golden     // support labels, already filtered to the cases
	GoldenTasks []GoldenTask // orders labels, already filtered to the cases

	Record *replay.Cassette // record every model call into it
	Replay *replay.Cassette // answer every model call from it

	Concurrency int
	ItemTimeout time.Duration
	Limits      agent.Config // MaxSteps, MaxTokens
	// DBPrefix overrides EvalDBPrefix (tests run on cloned databases).
	DBPrefix string
	Log      *slog.Logger
	// Now is the wall clock of a live or recording run (nil: time.Now),
	// read once: the whole run sees one instant. A replay ignores it and
	// uses its recording's clock.
	Now func() time.Time
}

// Result is one run's per-item results; one of the two slices is set.
type Result struct {
	Tickets []TicketResult
	Tasks   []TaskResult
}

// Run resets the eval database's queue for the selected items, runs the
// real worker over them with an in-process commerce API (over HTTP, as in
// production) and reads back what it decided. A replay that drifted from
// its recording is an error: its results would describe another run.
func Run(ctx context.Context, db *pgxpool.Pool, o Options) (Result, error) {
	if err := guard(ctx, db, cmp.Or(o.DBPrefix, EvalDBPrefix)); err != nil {
		return Result{}, err
	}
	var subjects []string
	switch o.Agent {
	case "support":
		for _, g := range o.Golden {
			subjects = append(subjects, g.ExternalID)
		}
		if err := resetTickets(ctx, db, subjects); err != nil {
			return Result{}, err
		}
	case "orders":
		for _, g := range o.GoldenTasks {
			subjects = append(subjects, g.Subject())
		}
		if err := resetTasks(ctx, db, o.GoldenTasks); err != nil {
			return Result{}, err
		}
	default:
		return Result{}, fmt.Errorf("evals: unknown agent %q", o.Agent)
	}

	now, err := pinClock(ctx, db, o)
	if err != nil {
		return Result{}, err
	}
	clock := func() time.Time { return now }
	api := httptest.NewServer(commerce.NewHandler(commerce.NewStoreWithClock(db, clock), o.Log))
	defer api.Close()

	finalize := 10 * time.Second
	cfg := worker.Config{
		Agent: o.Agent, Batch: len(subjects), Concurrency: max(1, o.Concurrency),
		// One attempt: an item that fails is scored as failed, not retried
		// after its lease expires in some later run.
		MaxAttempts: 1, ItemTimeout: o.ItemTimeout, FinalizeTimeout: finalize,
		Lease: o.ItemTimeout + 2*finalize + time.Minute,
	}
	spec, err := providers.Lookup(o.ModelID)
	if err != nil {
		return Result{}, err
	}
	models := &modelSource{o: o}

	promptSHA, autonomy := support.PromptSHA, string(o.Policy.Autonomy)
	if o.Agent == "orders" {
		promptSHA, autonomy = orders.PromptSHA, string(triage.AutonomySuggest)
	}
	runID, err := queue.StartRun(ctx, db, queue.Run{Agent: o.Agent, Model: o.ModelID, Autonomy: autonomy, PromptSHA: promptSHA})
	if err != nil {
		return Result{}, err
	}
	of := &obs.Factory{DB: db, Spec: spec, Log: o.Log}
	client := commerce.NewClient(api.URL)
	var stats worker.Stats
	var runErr error
	if o.Agent == "support" {
		store := support.NewTicketStore(db, cfg.Lease, cfg.MaxAttempts)
		h := &support.Handler{Store: store, Commerce: client, ModelFor: models.For, ModelID: o.ModelID,
			Policy: o.Policy, Limits: o.Limits, FinalizeTimeout: finalize}
		stats, runErr = worker.Run(ctx, cfg, store, runID, h, of, o.Log)
	} else {
		store := orders.NewTaskStore(db, cfg.Lease, cfg.MaxAttempts)
		h := &orders.Handler{Store: store, Commerce: client, ModelFor: models.For, ModelID: o.ModelID,
			RefundLimitCents: o.Policy.RefundLimitCents, Limits: o.Limits, FinalizeTimeout: finalize, Now: clock}
		stats, runErr = worker.Run(ctx, cfg, store, runID, h, of, o.Log)
	}
	fctx, cancel := worker.Detached(ctx, finalize)
	defer cancel()
	if err := errors.Join(runErr, queue.FinishRun(fctx, db, runID, stats, runErr)); err != nil {
		return Result{}, err
	}
	if err := models.drift(subjects); err != nil {
		return Result{}, err
	}
	if o.Agent == "support" {
		rs, err := readTickets(ctx, db, runID, subjects)
		return Result{Tickets: rs}, err
	}
	rs, err := readTasks(ctx, db, runID, subjects)
	return Result{Tasks: rs}, err
}

// fixtureSQL snapshots every seeded timestamp the agents' date math reads:
// the seed is relative to now(), so each reseed moves them.
const fixtureSQL = `
	SELECT jsonb_build_object(
	    'orders', (SELECT jsonb_agg(jsonb_build_object('order_number', order_number, 'created_at', created_at,
	        'shipped_at', shipped_at, 'delivered_at', delivered_at) ORDER BY order_number) FROM orders),
	    'tickets', (SELECT jsonb_agg(jsonb_build_object('external_id', external_id, 'created_at', created_at)
	        ORDER BY external_id) FROM tickets))`

// restores put a snapshot back, one table at a time.
var restores = []struct{ table, sql string }{
	{"orders", `
	UPDATE orders SET created_at = r.created_at, shipped_at = r.shipped_at, delivered_at = r.delivered_at
	FROM jsonb_to_recordset($1::jsonb -> 'orders')
	    AS r(order_number text, created_at timestamptz, shipped_at timestamptz, delivered_at timestamptz)
	WHERE orders.order_number = r.order_number`},
	{"tickets", `
	UPDATE tickets SET created_at = r.created_at
	FROM jsonb_to_recordset($1::jsonb -> 'tickets') AS r(external_id text, created_at timestamptz)
	WHERE tickets.external_id = r.external_id`},
}

// ErrSeedChanged is a replay on a seed other than its recording's: some
// rows would keep today's dates, so the replay could not reproduce the run.
var ErrSeedChanged = errors.New("evals: seed changed since recording; re-record the cassette")

// fixture is what a cassette needs besides the model's answers to replay a
// run exactly on another day: business days and the reprint window are
// computed from the clock and the seeded dates, and both move daily.
type fixture struct {
	Clock time.Time       `json:"clock"`
	Seed  json.RawMessage `json:"seed"`
}

// pinClock returns the instant the whole run sees. A replay restores its
// recording's seeded timestamps and clock (it rewrites the eval database's
// created/shipped/delivered dates, which a reseed puts back); a recording
// stores them.
func pinClock(ctx context.Context, db *pgxpool.Pool, o Options) (time.Time, error) {
	if o.Replay != nil {
		f, err := replayFixture(o.Replay.Fixture)
		if err != nil {
			return time.Time{}, err
		}
		return f.Clock, pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error { return restoreSeed(ctx, tx, f.Seed) })
	}
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	f := fixture{Clock: now().UTC()}
	if o.Record != nil {
		if err := db.QueryRow(ctx, fixtureSQL).Scan(&f.Seed); err != nil {
			return time.Time{}, fmt.Errorf("evals: snapshot seed: %w", err)
		}
		raw, err := json.Marshal(f)
		if err != nil {
			return time.Time{}, err
		}
		o.Record.Fixture = raw
	}
	return f.Clock, nil
}

func replayFixture(raw json.RawMessage) (fixture, error) {
	var f fixture
	if len(raw) == 0 {
		return f, errors.New("evals: cassette has no fixture (re-record it)")
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return f, fmt.Errorf("evals: cassette fixture: %w (re-record it)", err)
	}
	if f.Clock.IsZero() {
		return f, errors.New("evals: cassette fixture has no clock (re-record it)")
	}
	return f, nil
}

// restoreSeed puts the recording's dates back on every row, and fails
// closed unless the seed has exactly the recorded rows: a missing one is
// not restored, an extra one keeps today's dates.
func restoreSeed(ctx context.Context, tx pgx.Tx, seed json.RawMessage) error {
	var snapshot map[string][]json.RawMessage
	if err := json.Unmarshal(seed, &snapshot); err != nil {
		return fmt.Errorf("evals: cassette seed snapshot: %w (re-record it)", err)
	}
	for _, r := range restores {
		tag, err := tx.Exec(ctx, r.sql, string(seed))
		if err != nil {
			return fmt.Errorf("evals: restore seed: %w", err)
		}
		var rows int64
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM "+r.table).Scan(&rows); err != nil {
			return err
		}
		if want := int64(len(snapshot[r.table])); tag.RowsAffected() != want || rows != want {
			return fmt.Errorf("%w: %s recorded %d rows, restored %d, table has %d", ErrSeedChanged, r.table, want, tag.RowsAffected(), rows)
		}
	}
	return nil
}

// guard refuses to touch any database whose name lacks the prefix, so a
// wrong URL can never reset the dev or a production queue.
func guard(ctx context.Context, db *pgxpool.Pool, prefix string) error {
	var name string
	if err := db.QueryRow(ctx, "SELECT current_database()").Scan(&name); err != nil {
		return err
	}
	if !strings.HasPrefix(name, prefix) {
		return fmt.Errorf("evals: refusing to reset database %q: its name must start with %q", name, prefix)
	}
	return nil
}

// resetTickets makes the selected tickets pending and parks the rest as
// failed, so the worker claims exactly the selection. Tasks are deleted:
// they belong to the previous run's decisions.
func resetTickets(ctx context.Context, db *pgxpool.Pool, ids []string) error {
	return pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "DELETE FROM tasks"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE tickets SET claimed_by = NULL, lease_until = NULL, attempts = 0, decision = NULL,
			    status     = CASE WHEN external_id = ANY($1) THEN 'pending' ELSE 'failed' END,
			    last_error = CASE WHEN external_id = ANY($1) THEN NULL ELSE 'not selected for this eval' END`, ids)
		return err
	})
}

// resetTasks replaces the tasks table with the golden tasks, inserted
// through the same outbox function the support agent uses. Tasks come from
// the labels, not from a support run, so the orders agent is scored on
// every labeled task whatever the support agent decided.
func resetTasks(ctx context.Context, db *pgxpool.Pool, gs []GoldenTask) error {
	return pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "DELETE FROM tasks"); err != nil {
			return err
		}
		for _, g := range gs {
			var ticketID int64
			if err := tx.QueryRow(ctx, "SELECT id FROM tickets WHERE external_id = $1", g.Ticket).Scan(&ticketID); err != nil {
				return fmt.Errorf("evals: ticket %s: %w", g.Ticket, err)
			}
			fu := handoff.FollowUp{Type: g.Type, Reason: g.Reason, OrderNumber: g.OrderNumber}
			if err := tasks.Enqueue(ctx, tx, ticketID, []handoff.FollowUp{fu}); err != nil {
				return err
			}
		}
		return nil
	})
}

// modelSource builds each item's model and keeps the replayers, to check
// after the run that every recorded step was replayed.
type modelSource struct {
	o         Options
	mu        sync.Mutex
	replayers map[string]*replay.Replayer
}

func (s *modelSource) For(subject string, attempt int) agent.Model {
	key := replay.Key(subject, attempt)
	if s.o.Replay != nil {
		r := replay.NewReplayer(s.o.Replay, key)
		s.mu.Lock()
		if s.replayers == nil {
			s.replayers = map[string]*replay.Replayer{}
		}
		s.replayers[key] = r
		s.mu.Unlock()
		return r
	}
	m := s.o.Model
	if m == nil { // the oracle
		m = s.oracle(subject)
	}
	if s.o.Record != nil {
		return replay.NewRecorder(s.o.Record, m, key)
	}
	return m
}

func (s *modelSource) oracle(subject string) agent.Model {
	for _, g := range s.o.Golden {
		if g.ExternalID == subject {
			return SupportOracle(g)
		}
	}
	for _, g := range s.o.GoldenTasks {
		if g.Subject() == subject {
			return OrdersOracle(g)
		}
	}
	return oracleFunc(func(agent.Request) (agent.ToolCall, error) {
		return agent.ToolCall{}, fmt.Errorf("oracle: no golden label for %s", subject)
	})
}

// drift reports every replayed item that left the recording, and every
// recorded selected item that was never replayed.
func (s *modelSource) drift(subjects []string) error {
	if s.o.Replay == nil {
		return nil
	}
	var errs []error
	for _, key := range s.o.Replay.Keys() {
		subject := key[:strings.LastIndex(key, "#")]
		if !slices.Contains(subjects, subject) {
			continue
		}
		if _, ok := s.replayers[key]; !ok {
			errs = append(errs, fmt.Errorf("%w: %s was recorded but not replayed", replay.ErrReplayDiverged, key))
		}
	}
	for _, key := range slices.Sorted(maps.Keys(s.replayers)) {
		if err := s.replayers[key].Done(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// usageSQL sums one run's item rows per subject (attempts of one item add up).
const usageSQL = `
	SELECT coalesce(sum(cost_micros), 0)::bigint, coalesce(sum(input_tokens), 0)::bigint,
	       coalesce(sum(output_tokens), 0)::bigint, coalesce(sum(cache_read_tokens), 0)::bigint,
	       coalesce(sum(latency_ms), 0)::bigint
	FROM run_items WHERE run_id = $1 AND subject_kind = $2 AND subject_id = $3`

func readUsage(ctx context.Context, db *pgxpool.Pool, run queue.RunID, kind string, id int64) (Usage, error) {
	var u Usage
	err := db.QueryRow(ctx, usageSQL, run, kind, id).Scan(&u.CostMicros, &u.InputTokens, &u.OutputTokens, &u.CacheReadTokens, &u.LatencyMS)
	return u, err
}

func readTickets(ctx context.Context, db *pgxpool.Pool, run queue.RunID, ids []string) ([]TicketResult, error) {
	rows, err := db.Query(ctx, `SELECT id, external_id, status, decision, coalesce(last_error, '')
		FROM tickets WHERE external_id = ANY($1) ORDER BY external_id`, ids)
	if err != nil {
		return nil, err
	}
	type row struct {
		id             int64
		ext, status, e string
		decision       []byte
	}
	scanned, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		return x, r.Scan(&x.id, &x.ext, &x.status, &x.decision, &x.e)
	})
	if err != nil {
		return nil, err
	}
	var out []TicketResult
	for _, x := range scanned {
		r := TicketResult{ID: x.ext}
		if r.Usage, err = readUsage(ctx, db, run, obs.KindTicket, x.id); err != nil {
			return nil, err
		}
		if x.decision == nil {
			r.Err = x.status + ": " + x.e
		} else {
			var rec triage.Record
			if err := json.Unmarshal(x.decision, &rec); err != nil {
				return nil, fmt.Errorf("ticket %s decision: %w", x.ext, err)
			}
			r.Source, r.Proposed, r.Final = rec.Source, rec.Proposed, &rec.Final
		}
		out = append(out, r)
	}
	return out, nil
}

func readTasks(ctx context.Context, db *pgxpool.Pool, run queue.RunID, subjects []string) ([]TaskResult, error) {
	rows, err := db.Query(ctx, `SELECT t.id, k.external_id || '/' || t.type, t.status, t.proposal, coalesce(t.last_error, '')
		FROM tasks t JOIN tickets k ON k.id = t.ticket_id ORDER BY 2`)
	if err != nil {
		return nil, err
	}
	type row struct {
		id              int64
		subject, status string
		e               string
		proposal        []byte
	}
	scanned, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		return x, r.Scan(&x.id, &x.subject, &x.status, &x.proposal, &x.e)
	})
	if err != nil {
		return nil, err
	}
	var out []TaskResult
	for _, x := range scanned {
		if !slices.Contains(subjects, x.subject) {
			continue
		}
		r := TaskResult{ID: x.subject}
		if r.Usage, err = readUsage(ctx, db, run, obs.KindTask, x.id); err != nil {
			return nil, err
		}
		if x.proposal == nil {
			r.Err = x.status + ": " + x.e
		} else {
			var oc orders.Outcome
			if err := json.Unmarshal(x.proposal, &oc); err != nil {
				return nil, fmt.Errorf("task %s proposal: %w", x.subject, err)
			}
			r.Source, r.Final, r.Baseline = oc.Source, oc.Final.Verdict, oc.Baseline
			if oc.Proposed != nil {
				v := oc.Proposed.Verdict
				r.Proposed = &v
			}
		}
		out = append(out, r)
	}
	return out, nil
}
