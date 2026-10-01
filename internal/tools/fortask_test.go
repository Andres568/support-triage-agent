package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Andres568/support-triage-agent/internal/commerce"
)

// ownerScoped serves orders only to their owner, like the real API, and
// records every lookup.
type ownerScoped struct {
	owners  map[string]string // order number -> email
	lookups []string
}

func (o *ownerScoped) Order(_ context.Context, num, email string) (commerce.Order, error) {
	o.lookups = append(o.lookups, num+" as "+email)
	if o.owners[num] != email {
		return commerce.Order{}, commerce.ErrNotFound
	}
	delivered := time.Date(2026, 9, 1, 15, 0, 0, 0, time.UTC)
	return commerce.Order{OrderNumber: num, Status: "delivered", DeliveredAt: &delivered}, nil
}

func (o *ownerScoped) SearchPolicies(context.Context, string) ([]commerce.Policy, error) {
	return nil, nil
}

// get_task_order takes no arguments, and whatever the model passes, it reads
// the task's order as the task's customer: no other order is reachable.
func TestForTask_OnlyTheTaskOrder(t *testing.T) {
	c := &ownerScoped{owners: map[string]string{"ORD-100105": "erin@example.com", "ORD-100108": "henry@example.com"}}
	get := ForTask(c, "ORD-100105", "erin@example.com", time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC))[0]
	if spec := get.Spec(); spec.Name != "get_task_order" || !strings.Contains(string(spec.InputSchema), `"properties":{}`) ||
		!strings.Contains(string(spec.InputSchema), `"additionalProperties":false`) {
		t.Fatalf("spec = %+v, want get_task_order with no arguments", spec)
	}
	for _, args := range []string{`{}`, `{"order_number":"ORD-100108"}`, `{"email":"henry@example.com","order_number":"ORD-100108"}`, `"ORD-100108"`, `null`} {
		out, err := get.Call(context.Background(), json.RawMessage(args))
		if err != nil || !strings.Contains(out, "ORD-100105") || strings.Contains(out, "ORD-100108") ||
			!strings.Contains(out, `"reported_days_after_delivery":19`) {
			t.Errorf("args %s: out = %s, err = %v; want the task's order only", args, out, err)
		}
	}
	for _, l := range c.lookups {
		if l != "ORD-100105 as erin@example.com" {
			t.Errorf("lookup %q: the tool read something other than the task's order", l)
		}
	}
}

func TestForTask_MissingOrderIsAnAnswer(t *testing.T) {
	get := ForTask(&ownerScoped{}, "ORD-100105", "erin@example.com", time.Time{})[0]
	if _, err := get.Call(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "needs_human") ||
		strings.Contains(err.Error(), "erin@") {
		t.Errorf("err = %v, want a needs_human hint without the email", err)
	}
}

// A report dated before the recorded delivery has no days-after count: a
// negative number would read as "well within the window".
func TestForTask_NoNegativeReportedDays(t *testing.T) {
	c := &ownerScoped{owners: map[string]string{"ORD-100105": "erin@example.com"}}
	get := ForTask(c, "ORD-100105", "erin@example.com", time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC))[0]
	out, err := get.Call(context.Background(), nil)
	if err != nil || strings.Contains(out, "reported_days_after_delivery") {
		t.Errorf("out = %s, err = %v; want no reported_days_after_delivery", out, err)
	}
}
