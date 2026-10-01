package orders

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/Andres568/support-triage-agent/internal/commerce"
	"github.com/Andres568/support-triage-agent/internal/handoff"
	"github.com/Andres568/support-triage-agent/internal/triage"
)

// Outcome is what a task stores in tasks.proposal: the model's proposal
// before and after the gate, the rules' opinion, and why they differ.
type Outcome struct {
	Source         triage.Source `json:"source"` // agent | agent_limit
	Proposed       *Proposal     `json:"proposed,omitempty"`
	Final          Proposal      `json:"final"`
	Baseline       Verdict       `json:"baseline,omitempty"`
	BaselineReason string        `json:"baseline_reason,omitempty"`
	Overrides      []string      `json:"overrides,omitempty"`
	Model          string        `json:"model,omitempty"`
}

// Currency is the one currency the refund limit and policies are written in.
const Currency = "USD"

// Gate checks a proposal against the order our code fetched and the rules
// baseline. It is monotonic: the final verdict is the proposed one or
// needs_human, never more permissive, and an eligible final proposes nothing
// the model did not.
func Gate(t handoff.TaskType, p Proposal, o commerce.Order, baseline Verdict, refundLimitCents int) Outcome {
	out := Outcome{Proposed: &p, Final: p, Baseline: baseline}
	escalate := func(why string) {
		out.Overrides = append(out.Overrides, why)
		out.Final = needsHuman(p.PolicySlug, out.Overrides[0])
	}
	if p.Verdict != baseline {
		escalate(fmt.Sprintf("model verdict %s disagrees with the rules baseline %s", p.Verdict, baseline))
		return out
	}
	if p.Verdict != Eligible {
		return out
	}
	// The loop validated it already; checked again so the gate alone
	// guarantees an eligible final fits its task type.
	if err := p.Validate(t); err != nil {
		escalate(fmt.Sprintf("proposal does not fit a %s task: %v", t, err))
	}
	if o.Currency != Currency {
		escalate(fmt.Sprintf("order currency %q: limits and policies are in %s", o.Currency, Currency))
	}
	if why := itemsProblem(p.ReprintItems, o.Items); why != "" {
		escalate(why)
	}
	if p.RefundCents > o.TotalCents {
		escalate(fmt.Sprintf("refund %d cents exceeds the order total %d", p.RefundCents, o.TotalCents))
	}
	if p.RefundCents > refundLimitCents {
		escalate(fmt.Sprintf("refund %d cents exceeds the %d-cent limit", p.RefundCents, refundLimitCents))
	}
	return out
}

// Check is the invariant every stored outcome must meet, whatever produced
// it. It is checked at the write boundary (FinalizeTask), so a bug in the
// gate or the handler fails the task loudly instead of storing an unsafe
// proposal.
func (o Outcome) Check(t handoff.TaskType) error {
	if o.Source != triage.SourceAgent && o.Source != triage.SourceAgentLimit {
		return fmt.Errorf("source %q must be %s or %s", o.Source, triage.SourceAgent, triage.SourceAgentLimit)
	}
	f := o.Final
	switch f.Verdict {
	case Eligible:
		switch {
		case o.Source != triage.SourceAgent || o.Proposed == nil:
			return errors.New("an eligible final must come from an agent proposal")
		case !reflect.DeepEqual(f, *o.Proposed):
			return errors.New("an eligible final must be the agent's proposal unchanged")
		case o.Baseline != Eligible:
			return fmt.Errorf("an eligible final needs an eligible baseline, got %q", o.Baseline)
		case len(o.Overrides) > 0:
			return fmt.Errorf("an eligible final cannot have overrides: %v", o.Overrides)
		}
		if err := f.Validate(t); err != nil {
			return fmt.Errorf("eligible final is invalid for %s: %w", t, err)
		}
	case NotEligible, NeedsHuman:
		if len(f.ReprintItems) > 0 || f.RefundCents != 0 {
			return fmt.Errorf("a %s final proposes no reprint or refund", f.Verdict)
		}
	default:
		return fmt.Errorf("unknown final verdict %q", f.Verdict)
	}
	return nil
}

// needsHuman is a final proposal that hands the task to a person and
// proposes no action.
func needsHuman(slug, reason string) Proposal {
	return Proposal{Verdict: NeedsHuman, PolicySlug: slug, Reason: reason, ReprintItems: []ReprintItem{}}
}

// itemsProblem checks that every reprint item is on the order, with at most
// the ordered quantity. Unreadable order items are a problem too.
func itemsProblem(items []ReprintItem, orderItems json.RawMessage) string {
	if len(items) == 0 {
		return ""
	}
	var ordered []struct {
		SKU string `json:"sku"`
		Qty int    `json:"qty"`
	}
	if err := json.Unmarshal(orderItems, &ordered); err != nil {
		return "order items are unreadable: " + err.Error()
	}
	qty := map[string]int{}
	for _, it := range ordered {
		qty[it.SKU] += it.Qty
	}
	for _, it := range items {
		switch q, ok := qty[it.SKU]; {
		case !ok:
			return fmt.Sprintf("reprint item %q is not on the order", it.SKU)
		case it.Qty > q:
			return fmt.Sprintf("reprint of %d x %q exceeds the %d ordered", it.Qty, it.SKU, q)
		}
	}
	return ""
}
