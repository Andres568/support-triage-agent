package support

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Andres568/support-triage-agent/internal/agent"
	"github.com/Andres568/support-triage-agent/internal/commerce"
	"github.com/Andres568/support-triage-agent/internal/handoff"
	"github.com/Andres568/support-triage-agent/internal/obs"
	"github.com/Andres568/support-triage-agent/internal/queue"
	"github.com/Andres568/support-triage-agent/internal/tools"
	"github.com/Andres568/support-triage-agent/internal/triage"
)

// Handler triages one ticket: pre-check, model loop, facts, gate, finalize.
type Handler struct {
	Store    *TicketStore
	Commerce tools.Commerce
	// ModelFor returns the model for one attempt of one ticket. A factory,
	// so evals can wrap each item with record/replay.
	ModelFor func(subject string, attempt int) agent.Model
	ModelID  string // registry id, stored in the record
	Policy   triage.Policy
	// Limits bounds the loop; only MaxSteps and MaxTokens are read. The
	// prompt, tools and finish tool are always the support agent's own.
	Limits          agent.Config
	FinalizeTimeout time.Duration
	Log             *slog.Logger // nil: no per-decision log
}

// Subject names a ticket for the worker's span and run row.
func (h *Handler) Subject(c queue.Claimed[triage.Ticket]) (kind, subject string) {
	return obs.KindTicket, c.Item.ExternalID
}

// Handle decides and finalizes. Errors are infrastructure failures for the
// worker to retry; ErrLostLease means another run owns the ticket now.
func (h *Handler) Handle(ctx context.Context, c queue.Claimed[triage.Ticket]) error {
	ir := obs.ItemFrom(ctx)
	rec, err := h.decide(ctx, c.Item, c.Attempt, ir)
	if err != nil {
		return err
	}
	ir.Decided(rec.Status(), string(rec.Source))
	// Detached: the decision is made, only the write is left; keep ctx values
	// (trace ids) but not its cancellation or deadline.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), h.FinalizeTimeout)
	defer cancel()
	if err := h.Store.FinalizeTicket(fctx, c.Lease, rec); err != nil {
		return err
	}
	if h.Log != nil {
		h.Log.InfoContext(ctx, "ticket decided", "ticket", c.Item.ExternalID, "id", c.ID, "attempt", c.Attempt,
			"source", rec.Source, "action", rec.Final.Action, "category", rec.Final.Category,
			"overrides", len(rec.Overrides), "follow_ups", len(rec.Final.FollowUps))
	}
	return nil
}

// Decide produces the record for one ticket without touching the database.
func (h *Handler) Decide(ctx context.Context, t triage.Ticket, attempt int) (triage.Record, error) {
	return h.decide(ctx, t, attempt, nil)
}

// decide is Decide with the item's model and tools decorated by ir.
func (h *Handler) decide(ctx context.Context, t triage.Ticket, attempt int, ir *obs.ItemRecorder) (triage.Record, error) {
	rec := triage.Record{Autonomy: h.Policy.Autonomy}

	// High-risk tickets never reach the model: no cost, and no untrusted
	// text shown to it.
	if d, ok := triage.PreCheck(t); ok {
		rec.Source, rec.Final = triage.SourcePreCheck, d
		return rec, nil
	}
	rec.Model = h.ModelID

	cm := tools.NewWatched(h.Commerce)
	cfg := agent.Config{
		System:    SystemPrompt,
		Tools:     ir.Tools(tools.ForTicket(cm, t.CustomerEmail)),
		MaxSteps:  h.Limits.MaxSteps,
		MaxTokens: h.Limits.MaxTokens,
		Finish:    SubmitDecision,
		Validate: func(raw json.RawMessage) error {
			_, err := triage.ParseDecision(raw)
			return err
		},
	}
	res, runErr := agent.Run(ctx, ir.Model(h.ModelFor(t.ExternalID, attempt)), cfg, Input(t))
	ir.Loop(res)

	// A broken commerce API makes the model's view wrong, whatever it
	// concluded from it: retry rather than record a decision built on errors.
	if err := cm.Failure(); err != nil {
		return triage.Record{}, errors.Join(fmt.Errorf("commerce failed during the loop: %w", err), runErr)
	}
	if runErr != nil {
		if !agent.IsLimit(runErr) {
			return triage.Record{}, runErr
		}
		rec.Source = triage.SourceAgentLimit
		rec.Final = triage.Decision{
			Action:    triage.ActionEscalate,
			Reason:    "agent could not decide: " + runErr.Error(),
			FollowUps: []handoff.FollowUp{},
		}
		return rec, nil
	}

	d, err := triage.ParseDecision(res.Final)
	if err != nil { // Run already validated it; this is a bug, so retry loudly
		return triage.Record{}, fmt.Errorf("parse validated decision: %w", err)
	}
	facts, err := lookupFacts(ctx, h.Commerce, t, d, cm.Fetched())
	if err != nil {
		return triage.Record{}, err
	}
	out := triage.Gate(h.Policy, t, d, facts)
	rec.Source, rec.Proposed, rec.Final, rec.Overrides = triage.SourceAgent, &d, out.Decision, out.Overrides
	return rec, nil
}

// lookupFacts fetches, as the ticket's sender, every order the decision could
// be about: the ticket's own, each follow-up's, and each one the model looked
// up. Our code does this after the loop; nothing the model wrote is trusted.
// An order that is missing or someone else's is simply absent from Facts.
func lookupFacts(ctx context.Context, c tools.Commerce, t triage.Ticket, d triage.Decision, seen []string) (triage.Facts, error) {
	nums := append([]string{handoff.CanonicalOrderNumber(t.OrderNumber)}, seen...)
	for _, fu := range d.FollowUps {
		nums = append(nums, fu.OrderNumber)
	}
	f := triage.Facts{Orders: map[string]triage.OrderFacts{}}
	for _, num := range nums {
		if _, done := f.Orders[num]; done || !handoff.OrderNumberRE.MatchString(num) {
			continue
		}
		o, err := c.Order(ctx, num, t.CustomerEmail)
		if errors.Is(err, commerce.ErrNotFound) {
			continue
		}
		if err != nil {
			return triage.Facts{}, fmt.Errorf("facts for order %s: %w", num, err)
		}
		f.Orders[num] = triage.OrderFacts{Status: o.Status, TotalCents: o.TotalCents}
	}
	return f, nil
}
