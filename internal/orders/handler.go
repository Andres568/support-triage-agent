package orders

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Andres568/support-triage-agent/internal/agent"
	"github.com/Andres568/support-triage-agent/internal/commerce"
	"github.com/Andres568/support-triage-agent/internal/obs"
	"github.com/Andres568/support-triage-agent/internal/queue"
	"github.com/Andres568/support-triage-agent/internal/tasks"
	"github.com/Andres568/support-triage-agent/internal/tools"
	"github.com/Andres568/support-triage-agent/internal/triage"
)

// Handler proposes one task: model loop, our own order fetch, baseline,
// gate, finalize. It mirrors support.Handler.
type Handler struct {
	Store    *TaskStore
	Commerce tools.Commerce
	// ModelFor returns the model for one attempt of one task (keyed by
	// tasks.Task.Subject). A factory, so evals can wrap each item.
	ModelFor         func(subject string, attempt int) agent.Model
	ModelID          string // registry id, stored in the outcome
	RefundLimitCents int
	// Limits bounds the loop; only MaxSteps and MaxTokens are read.
	Limits          agent.Config
	FinalizeTimeout time.Duration
	Log             *slog.Logger     // nil: no per-task log
	Now             func() time.Time // nil: time.Now; for the baseline's date math
}

// Subject names a task for the worker's span and run row.
func (h *Handler) Subject(c queue.Claimed[tasks.Task]) (kind, subject string) {
	return obs.KindTask, c.Item.Subject()
}

// Handle proposes and finalizes. Errors are infrastructure failures for the
// worker to retry; ErrLostLease means another run owns the task now.
func (h *Handler) Handle(ctx context.Context, c queue.Claimed[tasks.Task]) error {
	ir := obs.ItemFrom(ctx)
	out, err := h.decide(ctx, c.Item, c.Attempt, ir)
	if err != nil {
		return err
	}
	ir.Decided(StatusProposed, string(out.Source))
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), h.FinalizeTimeout)
	defer cancel()
	if err := h.Store.FinalizeTask(fctx, c.Lease, c.Item.Type, out); err != nil {
		return err
	}
	if h.Log != nil {
		proposed := Verdict("")
		if out.Proposed != nil {
			proposed = out.Proposed.Verdict
		}
		h.Log.InfoContext(ctx, "task proposed", "task", c.Item.Subject(), "id", c.ID, "attempt", c.Attempt,
			"source", out.Source, "proposed", proposed, "final", out.Final.Verdict,
			"baseline", out.Baseline, "overrides", len(out.Overrides))
	}
	return nil
}

// Decide produces the outcome for one task without touching the database.
func (h *Handler) Decide(ctx context.Context, t tasks.Task, attempt int) (Outcome, error) {
	return h.decide(ctx, t, attempt, nil)
}

// decide is Decide with the item's model and tools decorated by ir.
func (h *Handler) decide(ctx context.Context, t tasks.Task, attempt int, ir *obs.ItemRecorder) (Outcome, error) {
	cm := tools.NewWatched(h.Commerce)
	cfg := agent.Config{
		System:    SystemPrompt,
		Tools:     ir.Tools(tools.ForTask(cm, t.OrderNumber, t.CustomerEmail, ReportedAt(t, h.now()))),
		MaxSteps:  h.Limits.MaxSteps,
		MaxTokens: h.Limits.MaxTokens,
		Finish:    SubmitProposal,
		Validate: func(raw json.RawMessage) error {
			_, err := ParseProposal(raw, t.Type)
			return err
		},
	}
	res, runErr := agent.Run(ctx, ir.Model(h.ModelFor(t.Subject(), attempt)), cfg, Input(t))
	ir.Loop(res)

	// A broken commerce API makes the model's view wrong: retry.
	if err := cm.Failure(); err != nil {
		return Outcome{}, errors.Join(fmt.Errorf("commerce failed during the loop: %w", err), runErr)
	}
	if runErr != nil && !agent.IsLimit(runErr) {
		return Outcome{}, runErr
	}

	// The gate's facts come from our own owner-scoped fetch, never from what
	// the model read or wrote.
	// The API refusing our own request (ErrRejected) is not an outage to
	// retry: like a missing order, a person has to look.
	o, err := h.Commerce.Order(ctx, t.OrderNumber, t.CustomerEmail)
	if err != nil && !errors.Is(err, commerce.ErrNotFound) && !errors.Is(err, commerce.ErrRejected) {
		return Outcome{}, fmt.Errorf("order for the gate: %w", err)
	}
	notFound := err != nil

	if runErr != nil {
		out := Outcome{Source: triage.SourceAgentLimit, Model: h.ModelID,
			Final: needsHuman("", "agent could not decide: "+runErr.Error())}
		if !notFound {
			out.Baseline, out.BaselineReason = Baseline(t, o, h.now(), h.RefundLimitCents)
		}
		return out, nil
	}
	p, err := ParseProposal(res.Final, t.Type)
	if err != nil { // Run already validated it; this is a bug, so retry loudly
		return Outcome{}, fmt.Errorf("parse validated proposal: %w", err)
	}
	if notFound {
		return Outcome{Source: triage.SourceAgent, Model: h.ModelID, Proposed: &p,
			Final:     needsHuman(p.PolicySlug, p.Reason),
			Overrides: []string{"the task's order is not visible for its customer"}}, nil
	}
	baseline, why := Baseline(t, o, h.now(), h.RefundLimitCents)
	out := Gate(t.Type, p, o, baseline, h.RefundLimitCents)
	out.Source, out.Model, out.BaselineReason = triage.SourceAgent, h.ModelID, why
	if out.Final.Verdict == Eligible && h.Store != nil {
		dup, err := h.Store.EligibleDuplicate(ctx, t)
		if err != nil {
			return Outcome{}, err
		}
		if dup != 0 {
			why := fmt.Sprintf("duplicate of task %d, already proposed as eligible for %s", dup, t.OrderNumber)
			out.Overrides = append(out.Overrides, why)
			out.Final = needsHuman(p.PolicySlug, why)
		}
	}
	return out, nil
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}
