package main

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/Andres568/support-triage-agent/internal/agent"
	"github.com/Andres568/support-triage-agent/internal/providers"
	"github.com/Andres568/support-triage-agent/internal/queue"
	"github.com/Andres568/support-triage-agent/internal/worker"
)

// runBudget is MAX_RUN_COST_MICROS: once a run has spent it, the run stops
// claiming. Items in flight finish, so a run can overshoot by up to
// CONCURRENCY items; the cap bounds a runaway batch, not a single item
// (MAX_TOKENS does that). Zero means no cap.
type runBudget struct {
	max   int64
	price providers.Price
	spent atomic.Int64
	once  sync.Once
}

func (b *runBudget) exhausted() bool { return b.max > 0 && b.spent.Load() >= b.max }

// models counts every call's cost, including failed calls that were billed.
func (b *runBudget) models(f modelFactory) modelFactory {
	return func(subject string, attempt int) agent.Model { return costModel{f(subject, attempt), b} }
}

type costModel struct {
	agent.Model
	b *runBudget
}

func (m costModel) Generate(ctx context.Context, req agent.Request) (agent.Response, error) {
	resp, err := m.Model.Generate(ctx, req)
	m.b.spent.Add(m.b.price.Cost(resp.Usage))
	return resp, err
}

// budgetQueue claims nothing once the budget is spent, which ends the run
// like an empty queue does.
type budgetQueue[T any] struct {
	worker.Queue[T]
	b   *runBudget
	log *slog.Logger
}

func capped[T any](q worker.Queue[T], b *runBudget, log *slog.Logger) worker.Queue[T] {
	return budgetQueue[T]{q, b, log}
}

func (q budgetQueue[T]) Claim(ctx context.Context, run queue.RunID, limit int) ([]queue.Claimed[T], error) {
	if q.b.exhausted() {
		q.b.once.Do(func() {
			q.log.WarnContext(ctx, "run cost cap reached; claiming stopped", "spent_micros", q.b.spent.Load(), "cap_micros", q.b.max)
		})
		return nil, nil
	}
	return q.Queue.Claim(ctx, run, limit)
}
