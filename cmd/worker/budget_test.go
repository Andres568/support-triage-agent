package main

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/Andres568/support-triage-agent/internal/agent"
	"github.com/Andres568/support-triage-agent/internal/providers"
	"github.com/Andres568/support-triage-agent/internal/queue"
)

type oneCallModel struct{}

func (oneCallModel) Generate(context.Context, agent.Request) (agent.Response, error) {
	return agent.Response{Usage: agent.Usage{InputTokens: 1_000_000}}, nil // 1 USD at the price below
}

type countingQueue struct{ claims int }

func (q *countingQueue) Claim(context.Context, queue.RunID, int) ([]queue.Claimed[int], error) {
	q.claims++
	return nil, nil
}
func (q *countingQueue) Abandon(context.Context, queue.Lease, error) error { return nil }
func (q *countingQueue) FailExhausted(context.Context) (int64, error)      { return 0, nil }

func TestRunBudget_StopsClaimingOnceSpent(t *testing.T) {
	b := &runBudget{max: 1_500_000, price: providers.Price{InputMicrosPerMTok: 1_000_000}}
	inner := &countingQueue{}
	q := capped[int](inner, b, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m := b.models(func(string, int) agent.Model { return oneCallModel{} })("HD-1", 1)

	for i, wantClaims := range []int{1, 2, 2} { // 0, 1 and 2 USD spent before each claim
		if _, err := q.Claim(context.Background(), queue.RunID{}, 1); err != nil {
			t.Fatal(err)
		}
		if inner.claims != wantClaims {
			t.Fatalf("claim %d: inner claims = %d, want %d", i, inner.claims, wantClaims)
		}
		if _, err := m.Generate(context.Background(), agent.Request{}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRunBudget_ZeroIsNoCap(t *testing.T) {
	b := &runBudget{}
	b.spent.Store(1 << 40)
	if b.exhausted() {
		t.Fatal("a zero cap must not stop the run")
	}
}
