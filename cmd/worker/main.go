// Command worker runs one batch of an agent's queue and exits, like a Cloud
// Run Job execution: `worker -agent=support` or `-agent=orders`. Everything else is configured
// from the environment (see envConfig). `worker schedule` runs batches on a
// timer (schedule.go).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/Andres568/support-triage-agent/internal/agent"
	"github.com/Andres568/support-triage-agent/internal/commerce"
	"github.com/Andres568/support-triage-agent/internal/obs"
	"github.com/Andres568/support-triage-agent/internal/orders"
	"github.com/Andres568/support-triage-agent/internal/providers"
	"github.com/Andres568/support-triage-agent/internal/queue"
	"github.com/Andres568/support-triage-agent/internal/support"
	"github.com/Andres568/support-triage-agent/internal/triage"
	"github.com/Andres568/support-triage-agent/internal/worker"
)

func main() {
	log := obs.NewLogger(os.Stdout, os.Getenv("GOOGLE_CLOUD_PROJECT"))
	if len(os.Args) > 1 && os.Args[1] == "schedule" {
		if err := runSchedule(os.Args[2:], log); err != nil {
			log.Error("scheduler failed", "err", err)
			os.Exit(2)
		}
		return
	}

	agentName := flag.String("agent", "", "queue to process: support or orders")
	flag.Parse()

	if err := run(*agentName, os.LookupEnv, log); err != nil {
		log.Error("worker failed", "agent", *agentName, "err", err)
		os.Exit(1)
	}
}

func run(agentName string, lookup func(string) (string, bool), log *slog.Logger) error {
	cfg, err := loadConfig(agentName, lookup)
	if err != nil {
		return err
	}
	modelFor, err := newModel(cfg.Model, func(k string) string { v, _ := lookup(k); return v })
	if err != nil {
		return err
	}

	// SIGTERM (Cloud Run) or Ctrl-C: stop claiming, finish what is in flight.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdown, err := obs.Setup(ctx, "triage-worker")
	if err != nil {
		return err
	}
	// Runs last (defers are LIFO): the run span must end before the flush,
	// and the flush must finish before the process exits.
	defer func() {
		sctx, cancel := worker.Detached(ctx, cfg.Worker.FinalizeTimeout)
		defer cancel()
		if err := shutdown(sctx); err != nil {
			log.Error("trace flush failed", "err", err)
		}
	}()

	pcfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	// Every in-flight item holds a connection to finalize, plus the claim and
	// the run row: a smaller pool would make items wait on each other.
	pcfg.MaxConns = max(pcfg.MaxConns, int32(cfg.Worker.Concurrency)+2) //nolint:gosec // Concurrency is small and validated
	db, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return err
	}
	defer db.Close()

	spec, _ := providers.Lookup(cfg.Model) // newModel validated it
	budget := &runBudget{max: cfg.MaxRunCostMicros, price: spec.Price}
	a, err := agentFor(cfg, budget)
	if err != nil {
		return err
	}
	runID, err := queue.StartRun(ctx, db, queue.Run{
		Agent: cfg.Worker.Agent, Model: cfg.Model, Autonomy: a.autonomy, PromptSHA: a.promptSHA,
	})
	if err != nil {
		return err
	}
	ctx, span := otel.Tracer(obs.TracerName).Start(ctx, "run "+cfg.Worker.Agent,
		trace.WithAttributes(attribute.String("app.run_id", runID.String()), obs.AppModel.String(cfg.Model)))
	defer span.End()

	// worker.Run adds agent and run to its own logs; rlog is for ours.
	rlog := log.With("agent", cfg.Worker.Agent, "run", runID.String())
	of := &obs.Factory{DB: db, Spec: spec, Log: rlog}
	rlog.InfoContext(ctx, "run started", "model", cfg.Model, "autonomy", a.autonomy, "batch", cfg.Worker.Batch,
		"concurrency", cfg.Worker.Concurrency, "prompt_sha", a.promptSHA)

	stats, runErr := a.run(ctx, db, runID, budget.models(modelFor), of, log, rlog)
	if runErr != nil {
		span.SetStatus(codes.Error, "run failed")
	}

	// Detached: the run row must be closed even after a shutdown signal.
	fctx, cancel := worker.Detached(ctx, cfg.Worker.FinalizeTimeout)
	defer cancel()
	finishErr := queue.FinishRun(fctx, db, runID, stats, runErr)
	rlog.InfoContext(ctx, "run finished", "claimed", stats.Claimed, "finalized", stats.Finalized,
		"abandoned", stats.Abandoned, "lost_leases", stats.LostLeases, "ok", runErr == nil)
	return errors.Join(runErr, finishErr)
}

type modelFactory = func(subject string, attempt int) agent.Model

// agentRun is what differs between the two agents: what the run row records
// and how one batch runs.
type agentRun struct {
	autonomy, promptSHA string
	run                 func(ctx context.Context, db *pgxpool.Pool, id queue.RunID, m modelFactory, of *obs.Factory, log, rlog *slog.Logger) (worker.Stats, error)
}

func agentFor(cfg config, budget *runBudget) (agentRun, error) {
	limits := agent.Config{MaxSteps: cfg.MaxSteps, MaxTokens: cfg.MaxTokens}
	switch cfg.Worker.Agent {
	case "orders":
		// Fixed at suggest: the orders agent has no write tools, and a person
		// executes every proposal.
		return agentRun{autonomy: string(triage.AutonomySuggest), promptSHA: orders.PromptSHA,
			run: func(ctx context.Context, db *pgxpool.Pool, id queue.RunID, m modelFactory, of *obs.Factory, log, rlog *slog.Logger) (worker.Stats, error) {
				store := orders.NewTaskStore(db, cfg.Worker.Lease, cfg.Worker.MaxAttempts)
				h := &orders.Handler{
					Store: store, Commerce: newCommerceClient(cfg), ModelFor: m, ModelID: cfg.Model,
					RefundLimitCents: cfg.Policy.RefundLimitCents, Limits: limits,
					FinalizeTimeout: cfg.Worker.FinalizeTimeout, Log: rlog,
				}
				return worker.Run(ctx, cfg.Worker, capped(store, budget, rlog), id, h, of, log)
			}}, nil
	case "support":
		return agentRun{autonomy: string(cfg.Policy.Autonomy), promptSHA: support.PromptSHA,
			run: func(ctx context.Context, db *pgxpool.Pool, id queue.RunID, m modelFactory, of *obs.Factory, log, rlog *slog.Logger) (worker.Stats, error) {
				store := support.NewTicketStore(db, cfg.Worker.Lease, cfg.Worker.MaxAttempts)
				h := &support.Handler{
					Store: store, Commerce: newCommerceClient(cfg), ModelFor: m, ModelID: cfg.Model,
					Policy: cfg.Policy, Limits: limits, FinalizeTimeout: cfg.Worker.FinalizeTimeout, Log: rlog,
				}
				return worker.Run(ctx, cfg.Worker, capped(store, budget, rlog), id, h, of, log)
			}}, nil
	}
	return agentRun{}, fmt.Errorf("unknown agent %q: want support or orders", cfg.Worker.Agent)
}

func newCommerceClient(cfg config) *commerce.Client {
	c := commerce.NewClient(cfg.CommerceURL)
	if cfg.CommerceAudience != "" {
		c.HTTP.Transport = &commerce.IDTokenTransport{Audience: cfg.CommerceAudience}
	}
	return c
}
