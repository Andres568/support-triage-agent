// Package orders is the orders agent: it claims the tasks the support agent
// hands off and proposes, for a human on the orders team, whether each one is
// eligible under policy. It has no write tools and never acts by itself.
//
// It imports the handoff contract and the tasks table, never the support
// agent: the two agents only meet in the database.
package orders

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Andres568/support-triage-agent/internal/handoff"
)

type Verdict string

const (
	Eligible    Verdict = "eligible"
	NotEligible Verdict = "not_eligible"
	NeedsHuman  Verdict = "needs_human"
)

var Verdicts = []Verdict{Eligible, NotEligible, NeedsHuman}

// PolicySlugs are the seeded policies a proposal may cite.
var PolicySlugs = []string{
	"shipping-times", "shipping-delay", "reprint-damaged", "reprint-customer-error", "refunds",
	"cancellations-and-changes", "address-changes", "order-privacy", "disputes-and-legal",
	"fraud-reports", "bulk-and-custom-quotes", "materials-and-care", "support-hours",
}

// SubmitProposalTool is the orders agent's terminal tool.
const SubmitProposalTool = "submit_proposal"

type ReprintItem struct {
	SKU string `json:"sku"`
	Qty int    `json:"qty"`
}

// Proposal is the orders agent's answer for one task. Each task type uses
// only its own field: reprint_items for reprints, refund_cents for refunds,
// neither for address changes, and neither unless the verdict is eligible.
type Proposal struct {
	Verdict      Verdict       `json:"verdict"`
	PolicySlug   string        `json:"policy_slug"`
	Reason       string        `json:"reason"`
	ReprintItems []ReprintItem `json:"reprint_items"` // required, may be []
	RefundCents  int           `json:"refund_cents"`  // required, 0 if n/a
}

// ParseProposal decodes strictly (unknown fields are an error the model can
// fix) and validates the proposal for task type t.
func ParseProposal(raw json.RawMessage, t handoff.TaskType) (Proposal, error) {
	var wire struct {
		Verdict      Verdict       `json:"verdict"`
		PolicySlug   string        `json:"policy_slug"`
		Reason       string        `json:"reason"`
		ReprintItems []ReprintItem `json:"reprint_items"`
		RefundCents  *int          `json:"refund_cents"`
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil {
		return Proposal{}, fmt.Errorf("invalid proposal JSON: %w", err)
	}
	var errs []error
	if wire.ReprintItems == nil {
		errs = append(errs, errors.New("reprint_items is required: use [] when there is nothing to reprint"))
	}
	if wire.RefundCents == nil {
		errs = append(errs, errors.New("refund_cents is required: use 0 when there is nothing to refund"))
	}
	if len(errs) > 0 {
		return Proposal{}, errors.Join(errs...)
	}
	p := Proposal{wire.Verdict, wire.PolicySlug, wire.Reason, wire.ReprintItems, *wire.RefundCents}
	return p, p.Validate(t)
}

// Validate reports every problem at once, in words the model can act on.
func (p Proposal) Validate(t handoff.TaskType) error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if !slices.Contains(Verdicts, p.Verdict) {
		add("verdict %q must be one of %v", p.Verdict, Verdicts)
	}
	if !slices.Contains(PolicySlugs, p.PolicySlug) {
		add("policy_slug %q must be the slug of a policy from search_policy, one of %v", p.PolicySlug, PolicySlugs)
	}
	if strings.TrimSpace(p.Reason) == "" {
		add("reason is required")
	}
	if p.RefundCents < 0 {
		add("refund_cents must not be negative")
	}
	seen := map[string]bool{}
	for _, it := range p.ReprintItems {
		if strings.TrimSpace(it.SKU) == "" || it.Qty < 1 {
			add("each reprint item needs a sku and a qty of at least 1, got %+v", it)
		}
		if seen[it.SKU] {
			add("reprint item %q is listed twice", it.SKU)
		}
		seen[it.SKU] = true
	}

	switch {
	case p.Verdict != Eligible:
		if len(p.ReprintItems) > 0 || p.RefundCents != 0 {
			add("when the verdict is not eligible, reprint_items must be [] and refund_cents 0")
		}
	case t == handoff.ReprintRequest:
		if len(p.ReprintItems) == 0 {
			add("an eligible reprint must list the reprint_items (sku and qty from the order)")
		}
		if p.RefundCents != 0 {
			add("a reprint proposes no refund: refund_cents must be 0")
		}
	case t == handoff.RefundReview:
		if p.RefundCents <= 0 {
			add("an eligible refund must have refund_cents > 0")
		}
		if len(p.ReprintItems) > 0 {
			add("a refund proposes no reprint: reprint_items must be []")
		}
	case t == handoff.AddressChange:
		if len(p.ReprintItems) > 0 || p.RefundCents != 0 {
			add("an address change proposes no reprint or refund: reprint_items must be [] and refund_cents 0")
		}
	default:
		add("unknown task type %q", t)
	}
	return errors.Join(errs...)
}

// ProposalSchema is the JSON Schema for submit_proposal's arguments.
var ProposalSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "verdict":       {"type": "string", "enum": ["eligible", "not_eligible", "needs_human"]},
    "policy_slug":   {"type": "string", "description": "Slug of the policy the verdict is based on, as returned by search_policy."},
    "reason":        {"type": "string", "description": "One or two sentences for the orders team, citing the order facts and the policy."},
    "reprint_items": {
      "type": "array",
      "description": "Only for an eligible reprint_request: the order's items to reprint. Otherwise [].",
      "items": {
        "type": "object",
        "properties": {"sku": {"type": "string"}, "qty": {"type": "integer", "minimum": 1}},
        "required": ["sku", "qty"],
        "additionalProperties": false
      }
    },
    "refund_cents":  {"type": "integer", "minimum": 0, "description": "Only for an eligible refund_review: the amount in cents. Otherwise 0."}
  },
  "required": ["verdict", "policy_slug", "reason", "reprint_items", "refund_cents"],
  "additionalProperties": false
}`)
