// Command eval scores an agent against the golden labels on the synthetic
// eval database (EVAL_DATABASE_URL, whose name must start with triage_eval):
//
//	eval -agent support -model ollama/qwen3:8b [-repeat 3] [-cases HD-2001,HD-2005]
//	eval -agent support -model ollama/qwen3:8b -record evals/cassettes/support/ollama-qwen3-8b.json
//	eval -agent support -model ollama/qwen3:8b -replay evals/cassettes/support/ollama-qwen3-8b.json -check
//	eval compare evals/results/*/*/summary.json
//	eval rescore evals/results/*/*/
//
// A live run writes evals/results/<agent>/<model-slug>/{summary.json,results.jsonl,report.md}.
// A recording writes its summary next to the cassette (<cassette>.summary.json);
// a replay must reproduce that summary (ignoring latency) and each item's
// outcome (<cassette>.items.json) unless -update is given.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Andres568/support-triage-agent/internal/agent"
	"github.com/Andres568/support-triage-agent/internal/agent/replay"
	"github.com/Andres568/support-triage-agent/internal/evals"
	"github.com/Andres568/support-triage-agent/internal/obs"
	"github.com/Andres568/support-triage-agent/internal/orders"
	"github.com/Andres568/support-triage-agent/internal/providers"
	"github.com/Andres568/support-triage-agent/internal/support"
	"github.com/Andres568/support-triage-agent/internal/triage"
)

func main() {
	var err error
	switch {
	case len(os.Args) > 1 && os.Args[1] == "compare":
		err = compare(os.Stdout, os.Args[2:])
	case len(os.Args) > 1 && os.Args[1] == "rescore":
		err = rescore(os.Args[2:])
	default:
		err = run(os.Args[1:])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "eval:", err)
		os.Exit(1)
	}
}

type flags struct {
	agent, model, autonomy, cases, record, replay, out, thresholds string
	repeat, limit, concurrency                                     int
	itemTimeout                                                    time.Duration
	check, update                                                  bool
}

func parse(args []string) (flags, error) {
	var f flags
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	fs.StringVar(&f.agent, "agent", "support", "support or orders")
	fs.StringVar(&f.model, "model", evals.OracleModel, "registry model id, e.g. ollama/qwen3:8b or fake/oracle")
	// Auto by default: in shadow nothing may auto-reply, so the false
	// auto-reply rate would be 0 by construction and say nothing.
	fs.StringVar(&f.autonomy, "autonomy", string(triage.AutonomyAuto), "support autonomy: shadow, suggest or auto (allowlist order_status,general)")
	fs.IntVar(&f.repeat, "repeat", 1, "runs over the same items")
	fs.StringVar(&f.cases, "cases", "", "comma-separated ticket ids (orders: a ticket id selects its tasks); empty: all")
	fs.IntVar(&f.limit, "limit", 0, "at most this many items, after -cases; 0: all")
	fs.StringVar(&f.record, "record", "", "record every model call into this cassette")
	fs.StringVar(&f.replay, "replay", "", "answer every model call from this cassette")
	fs.StringVar(&f.out, "out", "evals/results", "results directory")
	fs.StringVar(&f.thresholds, "thresholds", "evals/thresholds.json", "thresholds file for -check")
	fs.BoolVar(&f.check, "check", false, "fail when a threshold (or, for fake/oracle, an expected value) is not met")
	fs.BoolVar(&f.update, "update", false, "with -replay: overwrite the committed results instead of comparing")
	fs.IntVar(&f.concurrency, "concurrency", 1, "items in parallel (Ollama serves one at a time)")
	fs.DurationVar(&f.itemTimeout, "item-timeout", 180*time.Second, "one item's model loop")
	if err := fs.Parse(args); err != nil {
		return f, err
	}
	switch {
	case f.agent != "support" && f.agent != "orders":
		return f, fmt.Errorf("-agent %q: want support or orders", f.agent)
	case f.record != "" && f.replay != "":
		return f, errors.New("-record and -replay are exclusive")
	case (f.record != "" || f.replay != "") && f.repeat != 1:
		return f, errors.New("-record and -replay need -repeat 1: a cassette holds one run")
	case f.repeat < 1:
		return f, errors.New("-repeat must be >= 1")
	}
	return f, nil
}

func run(args []string) error {
	f, err := parse(args)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := obs.NewLogger(os.Stderr, "")

	o, err := options(f, log)
	if err != nil {
		return err
	}
	url := os.Getenv("EVAL_DATABASE_URL")
	if url == "" {
		return errors.New("EVAL_DATABASE_URL is required (a database named triage_eval*)")
	}
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		return err
	}
	defer db.Close()

	promptSHA, toolsSHA := support.PromptSHA, support.ToolsSHA
	if f.agent == "orders" {
		promptSHA, toolsSHA = orders.PromptSHA, orders.ToolsSHA
	}
	switch {
	case f.record != "":
		o.Record = replay.New(f.model, promptSHA, toolsSHA)
	case f.replay != "":
		if o.Replay, err = replay.Load(f.replay, f.model, promptSHA, toolsSHA); err != nil {
			return err
		}
	}

	s := evals.Summary{Agent: f.agent, Model: f.model, Mode: "live", PromptSHA: promptSHA, Autonomy: string(o.Policy.Autonomy)}
	if f.agent == "orders" {
		s.Autonomy = string(triage.AutonomySuggest)
		for _, g := range o.GoldenTasks {
			s.Cases = append(s.Cases, g.Subject())
		}
	} else {
		for _, g := range o.Golden {
			s.Cases = append(s.Cases, g.ExternalID)
		}
	}
	if f.replay != "" {
		s.Mode = "replay"
	}

	var results []evals.Result
	for i := range f.repeat {
		log.InfoContext(ctx, "eval run", "agent", f.agent, "model", f.model, "repeat", i+1, "items", len(s.Cases))
		res, err := evals.Run(ctx, db, o)
		if err != nil {
			return err
		}
		if f.agent == "orders" {
			s.Runs = append(s.Runs, evals.ScoreOrders(o.GoldenTasks, res.Tasks, f.replay != ""))
		} else {
			s.Runs = append(s.Runs, evals.ScoreSupport(o.Golden, res.Tickets, o.Policy, f.replay != ""))
		}
		results = append(results, res)
	}
	s.Aggregate()
	if o.Record != nil {
		if err := os.MkdirAll(filepath.Dir(f.record), 0o755); err != nil {
			return err
		}
		if err := o.Record.Save(f.record); err != nil {
			return err
		}
	}

	var problems []string
	if f.model == evals.OracleModel {
		for _, r := range results {
			problems = append(problems, evals.OracleProblems(r, o.Golden, o.GoldenTasks, o.Policy)...)
		}
	}
	switch cassette := f.record + f.replay; {
	case cassette == "":
		dir := filepath.Join(f.out, f.agent, slug(f.model))
		if err := write(dir, s, results); err != nil {
			return err
		}
		fmt.Printf("wrote %s\n", dir)
	case f.replay != "" && !f.update:
		p, err := compareReplay(cassette, s, results[0])
		if err != nil {
			return err
		}
		problems = append(problems, p...)
		fmt.Print(evals.Markdown(s, results))
	default: // a recording, or a replay with -update
		raw, err := json.MarshalIndent(s, "", "  ")
		if err != nil {
			return err
		}
		items, err := json.MarshalIndent(evals.Outcomes(results[0]), "", "  ")
		if err != nil {
			return err
		}
		if err := errors.Join(os.WriteFile(summaryPath(cassette), append(raw, '\n'), 0o644),
			os.WriteFile(itemsPath(cassette), append(items, '\n'), 0o644)); err != nil {
			return err
		}
		fmt.Print(evals.Markdown(s, results))
		fmt.Printf("wrote %s\n", summaryPath(cassette))
	}
	if f.check {
		t, err := evals.ReadThresholds(f.thresholds)
		if err != nil {
			return err
		}
		problems = append(problems, t.Check(s)...)
	}
	if len(problems) > 0 {
		return fmt.Errorf("%d problem(s):\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	return nil
}

// compareReplay lists how a replay differs from its recording's committed
// summary and per-item outcomes. The recording is the reference: a replay
// must reproduce it exactly, or gating, parsing or scoring changed. A
// missing or unreadable reference is an error, never a pass.
func compareReplay(cassette string, s evals.Summary, r evals.Result) ([]string, error) {
	const hint = " (make eval-replay UPDATE=1 writes it)"
	path := summaryPath(cassette)
	committed, err := evals.ReadSummary(path)
	if err != nil {
		return nil, fmt.Errorf("%w%s", err, hint)
	}
	var problems []string
	if !evals.SameOutcome(committed, s) {
		got, _ := json.MarshalIndent(s, "", "  ")
		problems = append(problems, fmt.Sprintf("replay does not reproduce %s (make eval-replay UPDATE=1 if intended); got:\n%s", path, got))
	}
	var want map[string]string
	raw, err := os.ReadFile(itemsPath(cassette))
	if err == nil {
		err = json.Unmarshal(raw, &want)
	}
	if err != nil {
		return nil, fmt.Errorf("%w%s", err, hint)
	}
	got := evals.Outcomes(r)
	for _, id := range slices.Sorted(maps.Keys(want)) {
		if got[id] != want[id] {
			problems = append(problems, fmt.Sprintf("replay item %s: got %q, recorded %q", id, got[id], want[id]))
		}
	}
	if len(got) != len(want) {
		problems = append(problems, fmt.Sprintf("replay has %d items, recording %d", len(got), len(want)))
	}
	return problems, nil
}

// options loads the labels for the selected cases and builds the model.
func options(f flags, log *slog.Logger) (evals.Options, error) {
	o := evals.Options{
		Agent: f.agent, ModelID: f.model, Concurrency: f.concurrency, ItemTimeout: f.itemTimeout,
		Limits: agent.Config{MaxSteps: 8, MaxTokens: 60000}, Log: log,
		Policy: triage.PolicyFor(triage.Autonomy(f.autonomy), []triage.Category{triage.CategoryOrderStatus, triage.CategoryGeneral}),
	}
	if !slices.Contains([]triage.Autonomy{triage.AutonomyShadow, triage.AutonomySuggest, triage.AutonomyAuto}, o.Policy.Autonomy) {
		return o, fmt.Errorf("-autonomy %q: want shadow, suggest or auto", f.autonomy)
	}
	var cases []string
	if f.cases != "" {
		cases = strings.Split(f.cases, ",")
	}
	pick := func(id string) bool { return len(cases) == 0 || slices.Contains(cases, id) }
	var (
		gs  []evals.Golden
		gts []evals.GoldenTask
		err error
	)
	if f.agent == "support" {
		if gs, err = evals.LoadJSONL[evals.Golden]("evals/golden.jsonl"); err != nil {
			return o, err
		}
		for _, g := range gs {
			if pick(g.ExternalID) && (f.limit == 0 || len(o.Golden) < f.limit) {
				o.Golden = append(o.Golden, g)
			}
		}
	} else {
		if gts, err = evals.LoadJSONL[evals.GoldenTask]("evals/golden_tasks.jsonl"); err != nil {
			return o, err
		}
		for _, g := range gts {
			if (pick(g.Ticket) || pick(g.Subject())) && (f.limit == 0 || len(o.GoldenTasks) < f.limit) {
				o.GoldenTasks = append(o.GoldenTasks, g)
			}
		}
	}
	// Against every loaded label, not the selection: -limit may drop a
	// valid case, but must not hide a typo.
	for _, c := range cases {
		known := slices.ContainsFunc(gs, func(g evals.Golden) bool { return g.ExternalID == c }) ||
			slices.ContainsFunc(gts, func(g evals.GoldenTask) bool { return g.Ticket == c || g.Subject() == c })
		if !known {
			return o, fmt.Errorf("-cases: no labeled %s item %q", f.agent, c)
		}
	}
	if len(o.Golden)+len(o.GoldenTasks) == 0 {
		return o, fmt.Errorf("no labeled items match -cases %q", f.cases)
	}

	spec, err := providers.Lookup(f.model)
	if err != nil || f.model == evals.OracleModel || f.replay != "" {
		return o, err // the oracle is built per item; a replay calls no model
	}
	// No client timeout: each call is bounded by the item's context.
	o.Model, err = providers.New(spec, os.Getenv, &http.Client{})
	return o, err
}

// summaryPath is where a cassette's summary lives, next to it: a recording
// is one run for the replay gate, not a measurement, so it never replaces
// the repeated runs under evals/results.
func summaryPath(cassette string) string {
	return strings.TrimSuffix(cassette, ".json") + ".summary.json"
}

// itemsPath holds each item's recorded outcome, next to the summary.
func itemsPath(cassette string) string {
	return strings.TrimSuffix(cassette, ".json") + ".items.json"
}

// slug makes a model id a directory name: ollama/qwen3:8b → ollama-qwen3-8b.
func slug(model string) string {
	return strings.NewReplacer("/", "-", ":", "-", ".", "-").Replace(model)
}

func write(dir string, s evals.Summary, results []evals.Result) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	var lines []byte
	for i, r := range results {
		items := []any{}
		for _, t := range r.Tickets {
			items = append(items, t)
		}
		for _, t := range r.Tasks {
			items = append(items, t)
		}
		for _, it := range items {
			line, err := json.Marshal(struct {
				Run  int `json:"run"`
				Item any `json:"item"`
			}{i + 1, it})
			if err != nil {
				return err
			}
			lines = append(append(lines, line...), '\n')
		}
	}
	return errors.Join(
		os.WriteFile(filepath.Join(dir, "summary.json"), append(raw, '\n'), 0o644),
		os.WriteFile(filepath.Join(dir, "results.jsonl"), lines, 0o644),
		os.WriteFile(filepath.Join(dir, "report.md"), []byte(evals.Markdown(s, results)), 0o644),
	)
}

// compare prints a markdown table of the given summaries.
func compare(w io.Writer, paths []string) error {
	if len(paths) == 0 {
		return errors.New("usage: eval compare SUMMARY_JSON [SUMMARY_JSON ...]")
	}
	var ss []evals.Summary
	for _, p := range paths {
		s, err := evals.ReadSummary(p)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		ss = append(ss, s)
	}
	_, err := fmt.Fprint(w, evals.CompareTable(ss))
	return err
}

// rescore re-scores committed live results (results.jsonl) with the current
// scorer and labels, and rewrites summary.json and report.md: a change to a
// metric's definition must not need a new model run to publish.
func rescore(dirs []string) error {
	if len(dirs) == 0 {
		return errors.New("usage: eval rescore RESULTS_DIR [RESULTS_DIR ...]")
	}
	for _, dir := range dirs {
		if err := rescoreDir(dir); err != nil {
			return fmt.Errorf("%s: %w", dir, err)
		}
		fmt.Printf("rescored %s\n", dir)
	}
	return nil
}

func rescoreDir(dir string) error {
	s, err := evals.ReadSummary(filepath.Join(dir, "summary.json"))
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(dir, "results.jsonl"))
	if err != nil {
		return err
	}
	results := make([]evals.Result, len(s.Runs))
	for line := range strings.Lines(string(raw)) {
		var l struct {
			Run  int             `json:"run"`
			Item json.RawMessage `json:"item"`
		}
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			return err
		}
		if l.Run < 1 || l.Run > len(results) {
			return fmt.Errorf("results.jsonl: run %d, summary has %d", l.Run, len(results))
		}
		r := &results[l.Run-1]
		if s.Agent == "orders" {
			var t evals.TaskResult
			err = json.Unmarshal(l.Item, &t)
			r.Tasks = append(r.Tasks, t)
		} else {
			var t evals.TicketResult
			err = json.Unmarshal(l.Item, &t)
			r.Tickets = append(r.Tickets, t)
		}
		if err != nil {
			return err
		}
	}
	o, err := options(flags{agent: s.Agent, model: evals.OracleModel, autonomy: s.Autonomy, cases: strings.Join(s.Cases, ",")}, nil)
	if err != nil {
		return err
	}
	for i, r := range results {
		if s.Agent == "orders" {
			s.Runs[i] = evals.ScoreOrders(o.GoldenTasks, r.Tasks, s.Mode == "replay")
		} else {
			s.Runs[i] = evals.ScoreSupport(o.Golden, r.Tickets, o.Policy, s.Mode == "replay")
		}
	}
	s.Aggregate()
	return write(dir, s, results)
}
