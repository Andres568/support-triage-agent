// Package triage holds the support-domain vocabulary: the structured decision
// the agent must produce and the deterministic guardrail gate applied to it.
package triage

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Andres568/support-triage-agent/internal/handoff"
)

type Category string

const (
	CategoryOrderStatus       Category = "order_status"
	CategoryShippingDelay     Category = "shipping_delay"
	CategoryDamagedOrMisprint Category = "damaged_or_misprint"
	CategoryRefundRequest     Category = "refund_request"
	CategoryOrderChange       Category = "order_change"
	CategoryGeneral           Category = "general"
)

var Categories = []Category{
	CategoryOrderStatus, CategoryShippingDelay, CategoryDamagedOrMisprint,
	CategoryRefundRequest, CategoryOrderChange, CategoryGeneral,
}

type Action string

const (
	ActionAutoReply      Action = "auto_reply"       // send the reply without a human
	ActionDraftForReview Action = "draft_for_review" // a human reviews, edits and sends
	ActionEscalate       Action = "escalate"         // a human takes over the ticket
)

var Actions = []Action{ActionAutoReply, ActionDraftForReview, ActionEscalate}

// Decision is the agent's final, structured output for one ticket.
//
// There is deliberately no separate "escalate bool": Action already says it,
// and two fields could contradict each other (auto_reply + escalate=true).
type Decision struct {
	Category   Category `json:"category"`
	Action     Action   `json:"action"`
	DraftReply string   `json:"draft_reply"`
	Confidence float64  `json:"confidence"`
	Reason     string   `json:"reason"` // short justification, for auditing
	// Work proposed for the orders agent. Required but may be empty: one
	// shape for small models to follow, and valid under strict schema modes.
	FollowUps []handoff.FollowUp `json:"follow_ups"`
}

// Validate reports every problem at once, so the model can fix them in one step.
func (d Decision) Validate() error {
	var errs []error
	if !slices.Contains(Categories, d.Category) {
		errs = append(errs, fmt.Errorf("category %q is not one of %v", d.Category, Categories))
	}
	if !slices.Contains(Actions, d.Action) {
		errs = append(errs, fmt.Errorf("action %q is not one of %v", d.Action, Actions))
	}
	if d.Confidence < 0 || d.Confidence > 1 {
		errs = append(errs, fmt.Errorf("confidence %v must be between 0 and 1", d.Confidence))
	}
	if d.Action != ActionEscalate && strings.TrimSpace(d.DraftReply) == "" {
		errs = append(errs, errors.New("draft_reply is required unless action is escalate"))
	}
	if strings.TrimSpace(d.Reason) == "" {
		errs = append(errs, errors.New("reason is required"))
	}
	return errors.Join(append(errs, d.validateFollowUps()...)...)
}

// ValidateFollowUps checks only the follow-ups against the action. Writers use
// it at the storage boundary, where the rest of the decision may legitimately
// be partial (a pre-check escalation has no category).
func (d Decision) ValidateFollowUps() error {
	return errors.Join(d.validateFollowUps()...)
}

func (d Decision) validateFollowUps() []error {
	if d.FollowUps == nil {
		return []error{errors.New("follow_ups is required; use [] when there is nothing to follow up")}
	}
	var errs []error
	if d.Action == ActionEscalate && len(d.FollowUps) > 0 {
		errs = append(errs, errors.New("follow_ups must be [] when action is escalate: a human owns the next steps"))
	}
	seen := map[handoff.TaskType]bool{}
	for i, f := range d.FollowUps {
		if !slices.Contains(handoff.TaskTypes, f.Type) {
			errs = append(errs, fmt.Errorf("follow_ups[%d].type %q is not one of %v", i, f.Type, handoff.TaskTypes))
		}
		if !slices.Contains(handoff.Reasons, f.Reason) {
			errs = append(errs, fmt.Errorf("follow_ups[%d].reason %q is not one of %v", i, f.Reason, handoff.Reasons))
		}
		if !handoff.OrderNumberRE.MatchString(f.OrderNumber) {
			errs = append(errs, fmt.Errorf("follow_ups[%d].order_number %q must look like ORD-123456", i, f.OrderNumber))
		}
		if seen[f.Type] {
			errs = append(errs, fmt.Errorf("follow_ups[%d].type %q is repeated; propose each type at most once", i, f.Type))
		}
		seen[f.Type] = true
	}
	return errs
}

// SubmitDecisionTool is the name of the terminal tool whose arguments are a Decision.
const SubmitDecisionTool = "submit_decision"

// ParseDecision decodes and validates the arguments of the submit_decision tool.
// Unknown fields are rejected: a typo like "confidnce" must not silently become 0.
func ParseDecision(raw json.RawMessage) (Decision, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var d Decision
	if err := dec.Decode(&d); err != nil {
		return Decision{}, fmt.Errorf("invalid decision JSON: %w", err)
	}
	return d, d.Validate()
}

// DecisionSchema is the JSON Schema for submit_decision's arguments, sent to
// the model as the tool's input schema.
var DecisionSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "category":    {"type": "string", "enum": ["order_status", "shipping_delay", "damaged_or_misprint", "refund_request", "order_change", "general"]},
    "action":      {"type": "string", "enum": ["auto_reply", "draft_for_review", "escalate"]},
    "draft_reply": {"type": "string", "description": "Reply to the customer. Required unless action is escalate."},
    "confidence":  {"type": "number", "minimum": 0, "maximum": 1},
    "reason":      {"type": "string", "description": "One or two sentences justifying the decision."},
    "follow_ups":  {
      "type": "array",
      "maxItems": 3,
      "description": "Work for the orders team, only for an order verified with get_order. Use [] if none, and always [] when escalating.",
      "items": {
        "type": "object",
        "properties": {
          "type":         {"type": "string", "enum": ["reprint_request", "address_change", "refund_review"]},
          "reason":       {"type": "string", "enum": ["damaged", "misprint", "lost_in_transit", "cancelled_before_production", "customer_request"],
                           "description": "Why the orders team gets this task, as you verified it from the ticket and the order."},
          "order_number": {"type": "string", "pattern": "^ORD-[0-9]{6}$"}
        },
        "required": ["type", "reason", "order_number"],
        "additionalProperties": false
      }
    }
  },
  "required": ["category", "action", "draft_reply", "confidence", "reason", "follow_ups"],
  "additionalProperties": false
}`)
