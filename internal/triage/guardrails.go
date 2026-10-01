package triage

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/Andres568/support-triage-agent/internal/handoff"
)

// Ticket is the customer's message as received. Every field is untrusted.
type Ticket struct {
	ID            int64 // tickets.id; ours, not the customer's
	ExternalID    string
	OrderNumber   string
	CustomerEmail string
	Subject       string
	Body          string
	// SenderVerified is set by intake only after the sender proved they own
	// CustomerEmail (DMARC pass on the inbound mail, or a logged-in form).
	// get_order trusts CustomerEmail, so an unverified sender could read and
	// act on someone else's orders by typing their address (CWE-290). The
	// zero value is false: an unset field fails closed.
	SenderVerified bool
}

// highRisk matches terms that always need a human: payment disputes, legal
// threats, fraud. \b word boundaries keep "sue" from matching "issue".
// Deliberately simple and over-inclusive: escalating too often is cheap,
// auto-replying to a legal threat is not.
var highRisk = regexp.MustCompile(`(?i)\b(` + strings.Join([]string{
	`charge ?backs?`, `disputes?`, `disputing`,
	`lawyers?`, `attorneys?`, `legal action`, `lawsuit`, `sue`, `suing`,
	`fraud(ulent)?`, `unauthori[sz]ed`, `stolen card`,
}, "|") + `)\b`)

// HighRiskTerm returns the first high-risk term in the ticket, if any.
// The text is folded first (see foldForMatch) so look-alike letters, spacing
// and zero-width characters do not hide a term.
func HighRiskTerm(t Ticket) (string, bool) {
	m := highRisk.FindString(foldForMatch(t.Subject + "\n" + t.Body))
	return strings.ToLower(m), m != ""
}

// PreCheck runs before the model. If it returns true, the ticket is escalated
// without calling the model at all: no cost, no latency, and the untrusted
// text is never shown to the model. The decision has no category or draft,
// because nothing classified the ticket.
func PreCheck(t Ticket) (Decision, bool) {
	term, ok := HighRiskTerm(t)
	if !ok {
		return Decision{}, false
	}
	return Decision{
		Action:     ActionEscalate,
		Confidence: 1,
		Reason:     fmt.Sprintf("pre-check: matched high-risk term %q", term),
		FollowUps:  []handoff.FollowUp{},
	}, true
}

// Autonomy is how far the business trusts the support agent right now.
type Autonomy string

const (
	AutonomyShadow  Autonomy = "shadow"  // decisions are recorded; nothing reaches a customer
	AutonomySuggest Autonomy = "suggest" // humans see every draft before it is sent
	AutonomyAuto    Autonomy = "auto"    // allowlisted categories may auto-reply
)

// Policy is the business's current trust in the agent. It is configuration,
// not code: widening autonomy is a one-line change backed by eval results.
type Policy struct {
	Autonomy Autonomy
	// Below this confidence, auto_reply is downgraded to draft_for_review.
	MinAutoReplyConfidence float64
	// Categories allowed to auto-reply; all others get at most a draft.
	AutoReply map[Category]bool
	// Refunds above this amount need a support lead (see policy "refunds").
	RefundLimitCents int
}

// PolicyFor builds the policy for an autonomy level. Only "auto" honors the
// allowlist: in shadow and suggest nothing may auto-reply.
func PolicyFor(level Autonomy, allow []Category) Policy {
	p := Policy{
		Autonomy:               level,
		MinAutoReplyConfidence: MinAutoReplyConfidenceFloor,
		AutoReply:              map[Category]bool{},
		RefundLimitCents:       250_00,
	}
	if level == AutonomyAuto {
		for _, c := range allow {
			p.AutoReply[c] = true
		}
	}
	return p
}

// MinAutoReplyConfidenceFloor is the lowest confidence any policy may
// auto-reply at; Record.Check enforces it at the write boundary.
const MinAutoReplyConfidenceFloor = 0.8

// AutoReplyCeiling is every category that may ever auto-reply, whatever the
// configuration says. The others involve money, reprints or changes to an
// order, which a person must always see. Config loading rejects the rest,
// and Record.Check enforces it at the write boundary.
var AutoReplyCeiling = []Category{CategoryOrderStatus, CategoryShippingDelay, CategoryGeneral}

// EvalPolicy is the auto policy the evals score against: the widest autonomy
// the demo asks for. Deployments default to shadow (cmd/worker config).
func EvalPolicy() Policy {
	return PolicyFor(AutonomyAuto, []Category{CategoryOrderStatus, CategoryGeneral})
}

// Cap returns the most autonomous action the policy allows for a category,
// given a wanted action. Evals use it for the expected final action: a golden
// auto_reply in a category that may not auto-reply can only end as a draft.
func (p Policy) Cap(c Category, a Action) Action {
	if a == ActionAutoReply && !p.AutoReply[c] {
		return ActionDraftForReview
	}
	return a
}

// Facts are looked up by our own code, never taken from the model's output.
type Facts struct {
	// Only orders fetched as the ticket's sender, keyed by order number. An
	// order the sender does not own, or that does not exist, is absent.
	Orders map[string]OrderFacts
}

type OrderFacts struct {
	Status     string
	TotalCents int
}

// Outcome is the gated decision plus every rule that changed it, for auditing.
type Outcome struct {
	Decision  Decision
	Proposed  Action // what the model asked for, before the gate
	Overrides []string
}

// severity orders actions from least to most cautious.
var severity = map[Action]int{ActionAutoReply: 0, ActionDraftForReview: 1, ActionEscalate: 2}

// Gate applies deterministic rules to the model's decision after the loop.
//
// Every rule can only make the outcome MORE cautious (auto_reply → draft →
// escalate) or remove follow-up work, never the reverse. So rules cannot
// conflict, their order does not matter, and no model output, however
// manipulated, can talk its way past one.
func Gate(p Policy, t Ticket, d Decision, f Facts) Outcome {
	out := Outcome{Decision: d, Proposed: d.Action}
	require := func(min Action, why string) {
		if severity[min] > severity[out.Decision.Action] {
			out.Decision.Action = min
			out.Overrides = append(out.Overrides, why)
		}
	}

	// Phase 1: the action. These rules do not depend on each other.

	// Same check as PreCheck: defense in depth if the loop ran without it.
	if term, ok := HighRiskTerm(t); ok {
		require(ActionEscalate, fmt.Sprintf("high-risk term %q", term))
	}

	// The model might file a refund under another category, so the draft's
	// wording and a refund_review follow-up count too. The amount always
	// comes from verified orders, never from the model. Every verified order
	// counts, not just the ticket's: which order the refund is about must not
	// depend on how the customer typed its number (fail closed).
	mentionsRefund := d.Category == CategoryRefundRequest ||
		strings.Contains(strings.ToLower(d.DraftReply), "refund") ||
		slices.ContainsFunc(d.FollowUps, func(fu handoff.FollowUp) bool { return fu.Type == handoff.RefundReview })
	if mentionsRefund {
		for _, num := range slices.Sorted(maps.Keys(f.Orders)) {
			if o := f.Orders[num]; o.TotalCents > p.RefundLimitCents {
				require(ActionEscalate, fmt.Sprintf("refund on order %s of %d cents exceeds limit of %d", num, o.TotalCents, p.RefundLimitCents))
			}
		}
	}

	if !p.AutoReply[d.Category] {
		require(ActionDraftForReview, fmt.Sprintf("category %q is not allowed to auto-reply", d.Category))
	}
	if d.Confidence < p.MinAutoReplyConfidence {
		require(ActionDraftForReview, fmt.Sprintf("confidence %.2f below %.2f", d.Confidence, p.MinAutoReplyConfidence))
	}
	for _, why := range draftProblems(d.DraftReply, t, f) {
		require(ActionDraftForReview, why)
	}
	// A reprint or refund is compensation: a person sees the reply that goes
	// with it, even if the follow-up itself is dropped below.
	if slices.ContainsFunc(d.FollowUps, promisesCompensation) {
		require(ActionDraftForReview, "follow-ups propose compensation")
	}
	if !t.SenderVerified {
		require(ActionDraftForReview, "sender not verified")
	}

	// Phase 2: follow-ups. Depends only on the final action, and only removes
	// work. Always a fresh slice: the caller's decision is never mutated.
	out.Decision.FollowUps = []handoff.FollowUp{}
	if out.Decision.Action == ActionEscalate {
		if len(d.FollowUps) > 0 {
			out.Overrides = append(out.Overrides, fmt.Sprintf("dropped %d follow-up(s): escalated, a human owns the next steps", len(d.FollowUps)))
		}
		return out
	}
	// An unverified sender may be using someone else's address: no work is
	// handed to the orders agent on their word. A person reads the draft.
	if !t.SenderVerified {
		if len(d.FollowUps) > 0 {
			out.Overrides = append(out.Overrides, fmt.Sprintf("dropped %d follow-up(s): sender not verified", len(d.FollowUps)))
		}
		return out
	}
	for _, fu := range d.FollowUps {
		o, ok := f.Orders[fu.OrderNumber]
		if !ok {
			out.Overrides = append(out.Overrides, fmt.Sprintf("dropped %s follow-up: order %s not verified for sender", fu.Type, fu.OrderNumber))
			continue
		}
		if why := reasonProblem(d.Category, fu, o.Status); why != "" {
			out.Overrides = append(out.Overrides, fmt.Sprintf("dropped %s follow-up for %s: %s", fu.Type, fu.OrderNumber, why))
			continue
		}
		out.Decision.FollowUps = append(out.Decision.FollowUps, fu)
	}
	return out
}

// promisesCompensation is a follow-up that gives the customer something.
func promisesCompensation(fu handoff.FollowUp) bool {
	return fu.Type == handoff.ReprintRequest || fu.Type == handoff.RefundReview
}

// reasonCategories lists the ticket categories each reason may come from. A
// damage, loss or cancellation reason from a ticket filed as something else
// (a general question, an order change) is a model stretching the ticket.
// customer_request is absent: it claims nothing, so any category may carry it.
var reasonCategories = map[handoff.Reason][]Category{
	handoff.ReasonDamaged:                   {CategoryDamagedOrMisprint, CategoryRefundRequest},
	handoff.ReasonMisprint:                  {CategoryDamagedOrMisprint, CategoryRefundRequest},
	handoff.ReasonLostInTransit:             {CategoryShippingDelay, CategoryOrderStatus, CategoryRefundRequest},
	handoff.ReasonCancelledBeforeProduction: {CategoryRefundRequest, CategoryOrderChange},
}

// reasonProblem checks a follow-up's reason against the ticket's category,
// both ways, and against the order status. The reason is the customer's
// claim as the support agent read it, not a verified fact; the orders agent
// acts on it without reading the ticket, so a reason the category or the
// order contradicts never reaches it.
//
// A free reprint is compensation for damage, a misprint or a lost parcel,
// so customer_request never justifies one: it would be a reprint nobody
// claimed a reason for.
func reasonProblem(c Category, fu handoff.FollowUp, status string) string {
	r := fu.Reason
	if fu.Type == handoff.AddressChange {
		if r != handoff.ReasonCustomerRequest {
			return fmt.Sprintf("an address change has reason customer_request, not %q", r)
		}
		return ""
	}
	damage := r == handoff.ReasonDamaged || r == handoff.ReasonMisprint
	cats, claims := reasonCategories[r]
	switch {
	case c == CategoryDamagedOrMisprint && !damage:
		return fmt.Sprintf("reason %q does not fit category %q (want damaged or misprint)", r, c)
	case c == CategoryShippingDelay && r != handoff.ReasonLostInTransit:
		return fmt.Sprintf("reason %q does not fit category %q (want lost_in_transit)", r, c)
	case claims && !slices.Contains(cats, c):
		return fmt.Sprintf("reason %q does not fit category %q (want one of %v)", r, c, cats)
	case fu.Type == handoff.ReprintRequest && r == handoff.ReasonCustomerRequest:
		return "a free reprint needs a damage, misprint or loss reason, not customer_request"
	case damage && status != "delivered":
		return fmt.Sprintf("reason %q needs a delivered order, but it is %s", r, status)
	case r == handoff.ReasonLostInTransit && status != "shipped":
		return fmt.Sprintf("reason %q needs a shipped, undelivered order, but it is %s", r, status)
	case r == handoff.ReasonCancelledBeforeProduction && status != "cancelled":
		return fmt.Sprintf("reason %q needs a cancelled order, but it is %s", r, status)
	case !slices.Contains(handoff.Reasons, r):
		return fmt.Sprintf("unknown reason %q", r)
	}
	return ""
}
