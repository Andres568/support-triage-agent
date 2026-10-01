package orders

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Andres568/support-triage-agent/internal/commerce"
	"github.com/Andres568/support-triage-agent/internal/handoff"
	"github.com/Andres568/support-triage-agent/internal/tasks"
)

// now is a fixed Wednesday, so business-day counts are stable.
var now = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

const limit = 250_00

func daysAgo(n int) *time.Time {
	t := now.AddDate(0, 0, -n)
	return &t
}

func order(status string, total int, shipped, delivered *time.Time) commerce.Order {
	return commerce.Order{OrderNumber: "ORD-100105", Status: status, TotalCents: total, Currency: "USD", Domestic: true,
		Items:     json.RawMessage(`[{"sku":"MUG-CER-11OZ","name":"Ceramic mug","qty":250,"unit_price_cents":78}]`),
		ShippedAt: shipped, DeliveredAt: delivered}
}

func international(o commerce.Order) commerce.Order {
	o.Domestic = false
	return o
}

func TestBaseline(t *testing.T) {
	const (
		damaged   = handoff.ReasonDamaged
		misprint  = handoff.ReasonMisprint
		lost      = handoff.ReasonLostInTransit
		cancelled = handoff.ReasonCancelledBeforeProduction
		request   = handoff.ReasonCustomerRequest
	)
	tests := []struct {
		typ      handoff.TaskType
		reason   handoff.Reason
		o        commerce.Order
		reported *time.Time // nil: now
		want     Verdict
	}{
		{handoff.ReprintRequest, damaged, order("delivered", 195_00, daysAgo(10), daysAgo(5)), nil, Eligible},
		{handoff.ReprintRequest, misprint, order("delivered", 195_00, daysAgo(35), daysAgo(30)), nil, Eligible}, // last day
		{handoff.ReprintRequest, damaged, order("delivered", 195_00, daysAgo(36), daysAgo(31)), nil, NotEligible},
		{handoff.ReprintRequest, damaged, order("delivered", 140_00, daysAgo(52), daysAgo(45)), nil, NotEligible}, // HD-2017
		// Reported on day 28, processed on day 40: the report date counts.
		{handoff.ReprintRequest, damaged, order("delivered", 195_00, daysAgo(45), daysAgo(40)), daysAgo(12), Eligible},
		{handoff.ReprintRequest, request, order("delivered", 195_00, daysAgo(10), daysAgo(5)), nil, NeedsHuman}, // no claim: a person decides
		// Delivered after the report: inconsistent dates, a person looks.
		{handoff.ReprintRequest, damaged, order("delivered", 195_00, daysAgo(10), daysAgo(5)), daysAgo(8), NeedsHuman},
		{handoff.ReprintRequest, damaged, order("delivered", 89_00, nil, nil), nil, NeedsHuman}, // no delivery date: cannot tell
		{handoff.ReprintRequest, damaged, order("shipped", 144_00, daysAgo(23), nil), nil, NeedsHuman},
		{handoff.ReprintRequest, lost, order("shipped", 144_00, daysAgo(23), nil), nil, Eligible},    // 17 business days: lost
		{handoff.ReprintRequest, lost, order("shipped", 144_00, daysAgo(21), nil), nil, Eligible},    // 15: the threshold
		{handoff.ReprintRequest, lost, order("shipped", 144_00, daysAgo(20), nil), nil, NotEligible}, // 14
		{handoff.ReprintRequest, lost, international(order("shipped", 144_00, daysAgo(23), nil)), nil, NotEligible},
		{handoff.ReprintRequest, lost, international(order("shipped", 144_00, daysAgo(42), nil)), nil, Eligible}, // 30
		{handoff.ReprintRequest, lost, order("shipped", 144_00, nil, nil), nil, NeedsHuman},                      // no shipping date
		{handoff.ReprintRequest, lost, order("delivered", 144_00, daysAgo(23), daysAgo(2)), nil, NeedsHuman},
		{handoff.ReprintRequest, damaged, order("paid", 89_00, nil, nil), nil, NeedsHuman},
		{handoff.AddressChange, request, order("paid", 115_00, nil, nil), nil, Eligible},
		{handoff.AddressChange, request, order("in_production", 65_00, nil, nil), nil, Eligible},
		{handoff.AddressChange, request, order("shipped", 144_00, daysAgo(6), nil), nil, NotEligible},
		{handoff.AddressChange, request, order("delivered", 144_00, daysAgo(6), daysAgo(2)), nil, NotEligible},
		{handoff.RefundReview, damaged, order("delivered", 80_00, daysAgo(9), daysAgo(4)), nil, Eligible}, // HD-2012
		{handoff.RefundReview, misprint, order("delivered", 80_00, daysAgo(45), daysAgo(40)), nil, NotEligible},
		{handoff.RefundReview, request, order("delivered", 80_00, daysAgo(9), daysAgo(4)), nil, NeedsHuman},
		{handoff.RefundReview, damaged, order("delivered", 80_00, nil, nil), nil, NeedsHuman},
		{handoff.RefundReview, cancelled, order("cancelled", 79_00, nil, nil), nil, Eligible},
		{handoff.RefundReview, damaged, order("delivered", limit, daysAgo(9), daysAgo(4)), nil, Eligible},
		{handoff.RefundReview, damaged, order("delivered", limit+1, daysAgo(9), daysAgo(4)), nil, NeedsHuman},
		{handoff.RefundReview, misprint, order("delivered", 1240_00, daysAgo(12), daysAgo(8)), nil, NeedsHuman}, // HD-2021
		{handoff.RefundReview, request, order("refunded", 49_50, nil, nil), nil, NotEligible},
		{handoff.RefundReview, request, order("refunded", 1240_00, nil, nil), nil, NotEligible}, // refunded wins over the limit
		{handoff.RefundReview, lost, order("shipped", 144_00, daysAgo(23), nil), nil, Eligible},
		{handoff.RefundReview, lost, order("shipped", 144_00, daysAgo(3), nil), nil, NotEligible},
		{handoff.RefundReview, request, order("shipped", 50_00, daysAgo(3), nil), nil, NeedsHuman},
		{"gift_wrap", request, order("paid", 10_00, nil, nil), nil, NeedsHuman},
	}
	for _, tt := range tests {
		task := tasks.Task{Type: tt.typ, Reason: tt.reason}
		if tt.reported != nil {
			task.ReportedAt = *tt.reported
		}
		name := fmt.Sprintf("%s/%s/%s/%d", tt.typ, tt.reason, tt.o.Status, tt.o.TotalCents)
		got, why := Baseline(task, tt.o, now, limit)
		if got != tt.want || why == "" {
			t.Errorf("%s (shipped %v, delivered %v, domestic %v) = %s (%s), want %s",
				name, tt.o.ShippedAt, tt.o.DeliveredAt, tt.o.Domestic, got, why, tt.want)
		}
	}
}
