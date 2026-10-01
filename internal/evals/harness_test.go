package evals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Andres568/support-triage-agent/internal/agent"
	"github.com/Andres568/support-triage-agent/internal/agent/replay"
	"github.com/Andres568/support-triage-agent/internal/dbtest"
	"github.com/Andres568/support-triage-agent/internal/orders"
	"github.com/Andres568/support-triage-agent/internal/support"
	"github.com/Andres568/support-triage-agent/internal/triage"
)

func loadGolden(t *testing.T) ([]Golden, []GoldenTask) {
	t.Helper()
	gs, err := LoadJSONL[Golden]("../../evals/golden.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	gts, err := LoadJSONL[GoldenTask]("../../evals/golden_tasks.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return gs, gts
}

func oracleOptions(agentName string, gs []Golden, gts []GoldenTask) Options {
	return Options{
		Agent: agentName, ModelID: OracleModel, Policy: triage.EvalPolicy(),
		Golden: gs, GoldenTasks: gts, Concurrency: 4, ItemTimeout: 30 * time.Second,
		Limits: agent.Config{MaxSteps: 8, MaxTokens: 60000}, DBPrefix: "triage_t_",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// wantPreCheck are the seeded tickets the deterministic pre-check escalates
// before any model runs. Listed, not derived, so a weakened pre-check fails.
var wantPreCheck = []string{"HD-2020", "HD-2022"}

// wantBaselineCorrect is how many golden tasks the rules baseline gets right
// on the seed: all of them, so an LLM can only tie it here.
const wantBaselineCorrect = 9

// TestOracle_EndToEnd is the CI gate without a model: the oracle answers
// every item with its golden label through the whole pipeline (pre-check,
// loop, tools over HTTP, facts, gate, outbox, scoring), so every metric must
// come out at its perfect value and every pre-checked ticket must be the
// pre-check's (the oracle would escalate them too, hiding a broken one).
func TestOracle_EndToEnd(t *testing.T) {
	db := dbtest.Fresh(t)
	ctx := context.Background()
	gs, gts := loadGolden(t)
	p := triage.EvalPolicy()

	for _, agentName := range []string{"support", "orders"} {
		t.Run(agentName, func(t *testing.T) {
			res, err := Run(ctx, db, oracleOptions(agentName, gs, gts))
			if err != nil {
				t.Fatal(err)
			}
			var s Scores
			if agentName == "orders" {
				for _, problem := range OracleProblems(res, nil, gts, p) {
					t.Error(problem)
				}
				s = ScoreOrders(gts, res.Tasks, false)
				if m := s.Rates["baseline_accuracy"]; m.K != wantBaselineCorrect || m.N != len(gts) {
					t.Errorf("baseline_accuracy %d/%d, want %d/%d", m.K, m.N, wantBaselineCorrect, len(gts))
				}
			} else {
				for _, problem := range OracleProblems(res, gs, nil, p) {
					t.Error(problem)
				}
				s = ScoreSupport(gs, res.Tickets, p, false)
				checkPreCheck(t, db, res.Tickets)
				if m := s.Rates["injection_pass_rate"]; m.N != 5 {
					t.Errorf("injection cases %d, want 5", m.N)
				}
				if s.Latency == nil {
					t.Error("no latency")
				}
			}
			for name, m := range s.Rates {
				switch {
				case name == "baseline_accuracy":
				case LowerIsBetter(name): // every other rate but the baseline's must be 1
					if m.K != 0 {
						t.Errorf("%s = %d/%d, want 0", name, m.K, m.N)
					}
				case m.N == 0 || m.K != m.N:
					t.Errorf("%s = %d/%d, want all", name, m.K, m.N)
				}
			}
			if c := s.Counts; c["correct"] != c["items"] || c["failed"] != 0 || c["agent_limit"] != 0 {
				t.Errorf("counts %v", c)
			}
		})
	}
}

// checkPreCheck compares each ticket's source with the listed pre-checked
// ids and with triage.PreCheck over the seeded text.
func checkPreCheck(t *testing.T, db *pgxpool.Pool, rs []TicketResult) {
	t.Helper()
	rows, err := db.Query(context.Background(), `SELECT external_id, coalesce(order_number, ''), customer_email, subject, body FROM tickets`)
	if err != nil {
		t.Fatal(err)
	}
	tickets, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (triage.Ticket, error) {
		var k triage.Ticket
		return k, r.Scan(&k.ExternalID, &k.OrderNumber, &k.CustomerEmail, &k.Subject, &k.Body)
	})
	if err != nil {
		t.Fatal(err)
	}
	var derived, got []string
	for _, k := range tickets {
		if _, ok := triage.PreCheck(k); ok {
			derived = append(derived, k.ExternalID)
		}
	}
	for _, r := range rs {
		if r.Source == triage.SourcePreCheck {
			got = append(got, r.ID)
		}
	}
	slices.Sort(derived)
	slices.Sort(got)
	if !slices.Equal(got, wantPreCheck) || !slices.Equal(derived, wantPreCheck) {
		t.Errorf("pre-checked: run %q, triage.PreCheck %q, want %q", got, derived, wantPreCheck)
	}
}

// TestOracle_RecordThenReplay records an oracle run and replays it: the
// replayed run must score the same, and a cassette with a missing item is
// drift.
func TestOracle_RecordThenReplay(t *testing.T) {
	db := dbtest.Fresh(t)
	ctx := context.Background()
	gs, _ := loadGolden(t)
	gs = gs[:6]
	p := triage.EvalPolicy()

	o := oracleOptions("support", gs, nil)
	o.Record = replay.New(OracleModel, support.PromptSHA, support.ToolsSHA)
	recorded, err := Run(ctx, db, o)
	if err != nil {
		t.Fatal(err)
	}
	o.Record, o.Replay = nil, o.Record
	replayed, err := Run(ctx, db, o)
	if err != nil {
		t.Fatal(err)
	}
	a, b := ScoreSupport(gs, recorded.Tickets, p, true), ScoreSupport(gs, replayed.Tickets, p, true)
	if !equalScores(a, b) {
		t.Errorf("replayed %+v, recorded %+v", b, a)
	}

	// The replay fails closed on a cassette without a fixture, and on a seed
	// with more or fewer rows than the recording's.
	cassette, recordedFixture := o.Replay, o.Replay.Fixture
	edit := func(orders func([]json.RawMessage) []json.RawMessage) json.RawMessage {
		var f struct {
			Clock time.Time                    `json:"clock"`
			Seed  map[string][]json.RawMessage `json:"seed"`
		}
		if err := json.Unmarshal(cassette.Fixture, &f); err != nil {
			t.Fatal(err)
		}
		f.Seed["orders"] = orders(f.Seed["orders"])
		raw, _ := json.Marshal(f)
		return raw
	}
	for name, fixture := range map[string]json.RawMessage{
		"no fixture":       nil,
		"extra seeded row": edit(func(rs []json.RawMessage) []json.RawMessage { return rs[1:] }),
		"missing seeded row": edit(func(rs []json.RawMessage) []json.RawMessage {
			return append(slices.Clone(rs), json.RawMessage(`{"order_number":"ORD-NOPE","created_at":"2026-01-01T00:00:00Z"}`))
		}),
	} {
		cassette.Fixture = fixture
		_, err := Run(ctx, db, o)
		if err == nil || (fixture != nil && !errors.Is(err, ErrSeedChanged)) {
			t.Errorf("%s: replay err = %v, want it to fail closed", name, err)
		}
	}
	cassette.Fixture = recordedFixture

	// Recorded but not selected is fine; selected but not recorded is drift.
	empty := replay.New(OracleModel, support.PromptSHA, support.ToolsSHA)
	empty.Fixture = o.Replay.Fixture
	o.Replay = empty
	if _, err := Run(ctx, db, o); !errors.Is(err, replay.ErrCassetteMiss) && !errors.Is(err, replay.ErrReplayDiverged) {
		t.Errorf("replay of an empty cassette: %v, want a miss or divergence", err)
	}
}

func TestGuard_RefusesOtherDatabases(t *testing.T) {
	db := dbtest.Fresh(t)
	o := oracleOptions("support", nil, nil)
	o.DBPrefix = ""
	if _, err := Run(context.Background(), db, o); err == nil {
		t.Error("ran on a database not named triage_eval*")
	}
}

// TestReplay_AnotherDay records an orders run, then moves the orders' dates
// back 60 days, so a live run decides differently (reprint and refund
// windows are measured from them). A replay on "another day" (a wall clock
// 60 days on) restores the recording's dates and clock, so it reproduces
// the recorded results exactly.
func TestReplay_AnotherDay(t *testing.T) {
	db := dbtest.Fresh(t)
	ctx := context.Background()
	_, gts := loadGolden(t)

	o := oracleOptions("orders", nil, gts)
	o.Record = replay.New(OracleModel, orders.PromptSHA, orders.ToolsSHA)
	recorded, err := Run(ctx, db, o)
	if err != nil {
		t.Fatal(err)
	}
	const shift = `
		UPDATE orders SET created_at = created_at - interval '60 days', shipped_at = shipped_at - interval '60 days',
		    delivered_at = delivered_at - interval '60 days'`
	if _, err := db.Exec(ctx, shift); err != nil {
		t.Fatal(err)
	}

	cassette := o.Record
	o.Record = nil
	live, err := Run(ctx, db, o)
	if err != nil {
		t.Fatal(err)
	}
	if baselines(live) == baselines(recorded) {
		t.Fatalf("moving the seed changed no baseline: the test cannot tell pinned from unpinned\n%s", baselines(live))
	}

	o.Replay = cassette
	o.Now = func() time.Time { return time.Now().AddDate(0, 0, 60) } // ignored by a replay
	replayed, err := Run(ctx, db, o)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := baselines(replayed), baselines(recorded); got != want {
		t.Errorf("replay on another day:\n%s\nrecorded:\n%s", got, want)
	}
	if a, b := ScoreOrders(gts, recorded.Tasks, true), ScoreOrders(gts, replayed.Tasks, true); !equalScores(a, b) {
		t.Errorf("replayed %+v, recorded %+v", b, a)
	}
}

func baselines(r Result) string {
	var b strings.Builder
	for _, x := range r.Tasks {
		fmt.Fprintf(&b, "%s baseline=%s final=%s\n", x.ID, x.Baseline, x.Final)
	}
	return b.String()
}
