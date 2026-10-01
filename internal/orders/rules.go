package orders

import (
	"fmt"
	"time"

	"github.com/Andres568/support-triage-agent/internal/commerce"
	"github.com/Andres568/support-triage-agent/internal/handoff"
	"github.com/Andres568/support-triage-agent/internal/tasks"
)

// Policy thresholds, from the seeded policies (reprint-damaged,
// shipping-delay, address-changes, refunds).
const (
	ReprintWindowDays = 30 // calendar days after delivery to report damage or a misprint
	// Shipped, not delivered after this many business days: lost, so reprint
	// and reship, or refund if the customer prefers.
	LostPackageBusinessDays              = 15
	LostPackageBusinessDaysInternational = 30
)

// Baseline is the same decision written as plain rules, no model. The gate
// uses it as a second opinion, and it is the benchmark for retiring the LLM:
// if Baseline alone matches the agent's final accuracy, the model adds cost
// without value. now is injected so tests are deterministic.
//
// The task's reason (checked by the support gate) says which policy applies.
// Damage and misprints are measured at the time the customer reported them
// (the ticket's creation); a lost parcel is measured now, since it may have
// arrived since.
func Baseline(t tasks.Task, o commerce.Order, now time.Time, refundLimitCents int) (Verdict, string) {
	reported := ReportedAt(t, now)
	switch t.Type {
	case handoff.ReprintRequest:
		switch t.Reason {
		case handoff.ReasonDamaged, handoff.ReasonMisprint:
			return damageWindow(t.Reason, o, reported)
		case handoff.ReasonLostInTransit:
			return lostParcel(o, now)
		case handoff.ReasonCustomerRequest:
			return NeedsHuman, noClaim
		}
		return NotEligible, fmt.Sprintf("reason %s: free reprints cover only damage, misprints and lost parcels", t.Reason)
	case handoff.AddressChange:
		if o.Status == "paid" || o.Status == "in_production" {
			return Eligible, fmt.Sprintf("order is %s: not shipped yet", o.Status)
		}
		return NotEligible, fmt.Sprintf("order is %s: the address can only change before shipping", o.Status)
	case handoff.RefundReview:
		switch {
		case o.Status == "refunded":
			return NotEligible, "order is already refunded"
		case o.TotalCents > refundLimitCents:
			return NeedsHuman, fmt.Sprintf("total %d cents is above the %d-cent limit: a support lead must approve", o.TotalCents, refundLimitCents)
		case o.Status == "cancelled":
			return Eligible, "order is cancelled: refunded in full"
		case o.Status == "delivered" && (t.Reason == handoff.ReasonDamaged || t.Reason == handoff.ReasonMisprint):
			return damageWindow(t.Reason, o, reported)
		case o.Status == "delivered" && t.Reason == handoff.ReasonCustomerRequest:
			return NeedsHuman, noClaim
		case o.Status == "delivered":
			return NotEligible, fmt.Sprintf("delivered order, reason %s: refunds after delivery are only for damage or misprints", t.Reason)
		// shipping-delay: a lost parcel is reprinted "or refunded if the
		// customer prefers", so a lost-parcel refund follows the same rule.
		case o.Status == "shipped" && t.Reason == handoff.ReasonLostInTransit:
			return lostParcel(o, now)
		}
		return NeedsHuman, fmt.Sprintf("order is %s, reason %s: a refund needs a human to cancel or stop it first", o.Status, t.Reason)
	}
	return NeedsHuman, fmt.Sprintf("unknown task type %q", t.Type)
}

// noClaim explains a compensation task with reason customer_request. It
// claims no damage, misprint or loss, so no rule grants it; but "not
// eligible" would be a confident denial the rules cannot back either (the
// ticket may hold a claim the reason missed, or merit goodwill). A person
// reads the ticket.
const noClaim = "reason customer_request claims no damage, misprint or loss: a person reads the ticket to decide on compensation"

// isCompensation reports whether a task type compensates the customer. The
// refunds policy pays a refund instead of a reprint, never both, so the two
// types are one job for duplicate checks.
func isCompensation(t handoff.TaskType) bool {
	return t == handoff.ReprintRequest || t == handoff.RefundReview
}

// ReportedAt is when the customer reported the problem: the ticket's
// creation, or now for a task that does not carry it.
func ReportedAt(t tasks.Task, now time.Time) time.Time {
	if t.ReportedAt.IsZero() {
		return now
	}
	return t.ReportedAt
}

// damageWindow applies reprint-damaged: reported within 30 calendar days of
// delivery. A missing delivery date is a fact we cannot check: a person does.
func damageWindow(r handoff.Reason, o commerce.Order, reported time.Time) (Verdict, string) {
	switch {
	case o.Status != "delivered":
		return NeedsHuman, fmt.Sprintf("reason %s but the order is %s", r, o.Status)
	case o.DeliveredAt == nil:
		return NeedsHuman, "delivered, but the delivery date is missing"
	}
	d := commerce.DaysBetween(*o.DeliveredAt, reported)
	if d < 0 {
		return NeedsHuman, fmt.Sprintf("%s reported %d days before the recorded delivery: the dates disagree", r, -d)
	}
	if d <= ReprintWindowDays {
		return Eligible, fmt.Sprintf("%s reported %d days after delivery, within the %d-day window", r, d, ReprintWindowDays)
	}
	return NotEligible, fmt.Sprintf("%s reported %d days after delivery, outside the %d-day window", r, d, ReprintWindowDays)
}

// lostParcel applies shipping-delay: shipped, not delivered, and past the
// domestic or international limit.
func lostParcel(o commerce.Order, now time.Time) (Verdict, string) {
	switch {
	case o.Status != "shipped" || o.DeliveredAt != nil:
		return NeedsHuman, fmt.Sprintf("reason lost_in_transit but the order is %s", o.Status)
	case o.ShippedAt == nil:
		return NeedsHuman, "shipped, but the shipping date is missing"
	}
	limit, where := LostPackageBusinessDays, "domestic"
	if !o.Domestic {
		limit, where = LostPackageBusinessDaysInternational, "international"
	}
	d := commerce.BusinessDaysBetween(*o.ShippedAt, now)
	if d >= limit {
		return Eligible, fmt.Sprintf("%s parcel shipped %d business days ago and not delivered (lost after %d)", where, d, limit)
	}
	return NotEligible, fmt.Sprintf("%s parcel shipped %d business days ago: not lost until %d", where, d, limit)
}
