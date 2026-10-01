// Package tools implements the agents' read-only tools on top of the
// commerce API.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Andres568/support-triage-agent/internal/agent"
	"github.com/Andres568/support-triage-agent/internal/commerce"
	"github.com/Andres568/support-triage-agent/internal/handoff"
)

// Commerce is what the tools need from the commerce API. Declared here, by
// the consumer, so tests can pass a fake without an HTTP server.
type Commerce interface {
	Order(ctx context.Context, orderNumber, customerEmail string) (commerce.Order, error)
	SearchPolicies(ctx context.Context, query string) ([]commerce.Policy, error)
}

// ForTicket returns the tools for one ticket. The sender's email is bound
// here, by our code, from the ticket record. It is NOT a tool argument: if it
// were, a prompt-injected model could simply pass someone else's email.
func ForTicket(c Commerce, senderEmail string) []agent.Tool {
	return []agent.Tool{
		getOrder{c: c, senderEmail: senderEmail},
		searchPolicy{c: c},
	}
}

type getOrder struct {
	c           Commerce
	senderEmail string
}

func (getOrder) Spec() agent.ToolSpec {
	return agent.ToolSpec{
		Name: "get_order",
		Description: "Look up one of the customer's orders by order number (format ORD-123456). " +
			"Returns status, items, totals, shipping and business days since shipping/delivery. " +
			"Only orders placed with the email that sent this ticket are visible.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {"order_number": {"type": "string", "description": "e.g. ORD-100103"}},
  "required": ["order_number"],
  "additionalProperties": false
}`),
	}
}

func (t getOrder) Call(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		OrderNumber string `json:"order_number"`
	}
	if err := json.Unmarshal(raw, &args); err != nil || strings.TrimSpace(args.OrderNumber) == "" {
		return "", errors.New(`order_number is required, e.g. {"order_number": "ORD-100103"}`)
	}
	// Checked here, before any request: a malformed number is the model's
	// mistake to fix, not a commerce failure to retry the whole ticket for.
	num := handoff.CanonicalOrderNumber(args.OrderNumber)
	if !handoff.OrderNumberRE.MatchString(num) {
		return "", fmt.Errorf("order_number %q is not a valid order number: the format is ORD- followed by 6 digits, e.g. ORD-100103", args.OrderNumber)
	}
	o, err := t.c.Order(ctx, num, t.senderEmail)
	switch {
	case errors.Is(err, commerce.ErrNotFound):
		// Same message whether the order does not exist or belongs to someone
		// else, and a hint the model can act on.
		return "", fmt.Errorf("no order %s found for the sender's email: the number may be mistyped, "+
			"or the order was placed with a different email", num)
	case errors.Is(err, commerce.ErrRejected):
		return "", fmt.Errorf("the order lookup for %s was rejected as invalid; check the order number", num)
	}
	if err != nil {
		return "", err
	}
	return marshal(forModel(o))
}

type searchPolicy struct {
	c Commerce
}

func (searchPolicy) Spec() agent.ToolSpec {
	return agent.ToolSpec{
		Name: "search_policy",
		Description: "Search the support policies (shipping, delays, reprints, refunds, cancellations, " +
			"address changes, privacy, disputes, fraud, bulk orders, materials, support hours). " +
			"Returns up to 3 matching policies with their full text. Use short keyword queries.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {"query": {"type": "string", "description": "keywords, e.g. \"damaged reprint\""}},
  "required": ["query"],
  "additionalProperties": false
}`),
	}
}

func (t searchPolicy) Call(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(raw, &args); err != nil || strings.TrimSpace(args.Query) == "" {
		return "", errors.New(`query is required, e.g. {"query": "late package"}`)
	}
	ps, err := t.c.SearchPolicies(ctx, args.Query)
	if err != nil {
		return "", err
	}
	if len(ps) == 0 {
		return "", fmt.Errorf("no policy matches %q; try different keywords", args.Query)
	}
	return marshal(ps)
}

func marshal(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ForTask returns the orders agent's tools for one task. get_task_order takes
// no arguments: the order number and the customer email are bound here from
// the task row, which our code wrote (the number verified by the support
// gate for that sender, the email copied by SQL from the ticket). So the
// agent can read exactly one order, whatever it is told.
//
// reportedAt is when the customer reported the problem (the ticket's
// creation): the tool adds reported_days_after_delivery, computed here, so
// the model and the rules baseline measure the reprint window the same way.
func ForTask(c Commerce, orderNumber, customerEmail string, reportedAt time.Time) []agent.Tool {
	return []agent.Tool{
		getTaskOrder{c: c, orderNumber: orderNumber, customerEmail: customerEmail, reportedAt: reportedAt},
		searchPolicy{c: c},
	}
}

type getTaskOrder struct {
	c                          Commerce
	orderNumber, customerEmail string
	reportedAt                 time.Time
}

// taskOrder is the order as the orders agent sees it.
type taskOrder struct {
	commerce.Order
	ReportedDaysAfterDelivery *int `json:"reported_days_after_delivery,omitempty"`
}

func (getTaskOrder) Spec() agent.ToolSpec {
	return agent.ToolSpec{
		Name: "get_task_order",
		Description: "Get the order this task is about: status, items (sku, qty), totals, shipping and " +
			"delivery dates, business days since shipping and delivery, calendar days since delivery, whether it ships " +
			"domestically, and reported_days_after_delivery (calendar days from delivery to the customer's report). " +
			"Takes no arguments.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
	}
}

// Call ignores its arguments on purpose: there is nothing the model may choose.
func (t getTaskOrder) Call(ctx context.Context, _ json.RawMessage) (string, error) {
	o, err := t.c.Order(ctx, t.orderNumber, t.customerEmail)
	switch {
	case errors.Is(err, commerce.ErrNotFound), errors.Is(err, commerce.ErrRejected):
		return "", fmt.Errorf("the task's order %s could not be found; propose needs_human", t.orderNumber)
	case err != nil:
		return "", err
	}
	out := taskOrder{Order: forModel(o)}
	if o.DeliveredAt != nil && !t.reportedAt.IsZero() {
		// Reported before the recorded delivery: the dates disagree, so the
		// count is left out, as if the delivery date were missing.
		if d := commerce.DaysBetween(*o.DeliveredAt, t.reportedAt); d >= 0 {
			out.ReportedDaysAfterDelivery = &d
		}
	}
	return marshal(out)
}

// modelItem is what the model sees of an order line. The product name is
// left out: it is free text (a custom product name is whatever the
// customer typed), so it could carry instructions, and nothing the agents
// decide needs it. The orders gate matches reprint items by sku.
type modelItem struct {
	SKU            string `json:"sku"`
	Qty            int    `json:"qty"`
	UnitPriceCents int    `json:"unit_price_cents"`
}

// forModel returns o with its items reduced to modelItem. Unreadable items
// become null rather than being passed through unfiltered.
func forModel(o commerce.Order) commerce.Order {
	var items []modelItem
	if err := json.Unmarshal(o.Items, &items); err != nil {
		o.Items = json.RawMessage("null")
		return o
	}
	b, err := json.Marshal(items)
	if err != nil {
		b = []byte("null")
	}
	o.Items = b
	return o
}
