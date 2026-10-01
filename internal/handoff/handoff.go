// Package handoff is the contract between the support agent and the orders
// agent: a closed list of task types carried through the tasks table. The
// agents never call each other, and no customer text crosses: a task is only
// a type, a reason from a closed list, a validated order number and the
// sender's email copied by SQL.
//
// It has no dependencies on purpose, so domain packages (triage) can use the
// vocabulary without importing the database. The table itself lives in
// internal/tasks.
package handoff

import (
	"regexp"
	"strings"
)

type TaskType string

const (
	ReprintRequest TaskType = "reprint_request"
	AddressChange  TaskType = "address_change"
	RefundReview   TaskType = "refund_review"
)

// TaskTypes must match the CHECK on tasks.type (migration 00006).
var TaskTypes = []TaskType{ReprintRequest, AddressChange, RefundReview}

// OrderNumberRE must match the CHECK on tasks.order_number (migration 00006).
var OrderNumberRE = regexp.MustCompile(`^ORD-[0-9]{6}$`)

// Reason says why a task exists, in words the orders agent can act on
// without reading the ticket. The support agent read the ticket and chose
// the reason: it is the customer's claim, not a verified fact. The support
// gate checks only that it fits the ticket's category and the order's status.
type Reason string

const (
	ReasonDamaged                   Reason = "damaged"
	ReasonMisprint                  Reason = "misprint"
	ReasonLostInTransit             Reason = "lost_in_transit"
	ReasonCancelledBeforeProduction Reason = "cancelled_before_production"
	ReasonCustomerRequest           Reason = "customer_request"
)

// Reasons must match the CHECK on tasks.reason (migration 00008).
var Reasons = []Reason{ReasonDamaged, ReasonMisprint, ReasonLostInTransit, ReasonCancelledBeforeProduction, ReasonCustomerRequest}

// FollowUp is work the support agent proposes for the orders agent.
type FollowUp struct {
	Type        TaskType `json:"type"`
	Reason      Reason   `json:"reason"`
	OrderNumber string   `json:"order_number"`
}

// CanonicalOrderNumber turns what a customer or a model typed ("ord 100115",
// "ORD100115") into ORD-123456 form, or returns it unchanged if it cannot.
func CanonicalOrderNumber(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	if c := b.String(); len(c) == 9 && strings.HasPrefix(c, "ORD") {
		return "ORD-" + c[3:]
	}
	return s
}
