package orders

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Andres568/support-triage-agent/internal/agent"
	"github.com/Andres568/support-triage-agent/internal/agent/agenttest"
	"github.com/Andres568/support-triage-agent/internal/commerce"
	"github.com/Andres568/support-triage-agent/internal/handoff"
	"github.com/Andres568/support-triage-agent/internal/tasks"
)

// fakeCommerce is owner-scoped like the real API; err fails every call.
type fakeCommerce struct {
	orders  map[string]commerce.Order // by number; owner in ownerOf
	owner   map[string]string
	err     error
	okCalls int // Order calls that succeed before err applies
	calls   int
}

func (f *fakeCommerce) Order(_ context.Context, num, email string) (commerce.Order, error) {
	f.calls++
	if f.err != nil && f.calls > f.okCalls {
		return commerce.Order{}, f.err
	}
	if f.owner[num] != email {
		return commerce.Order{}, commerce.ErrNotFound
	}
	return f.orders[num], nil
}

func (f *fakeCommerce) SearchPolicies(context.Context, string) ([]commerce.Policy, error) {
	return []commerce.Policy{{Slug: "reprint-damaged", Title: "Reprints", Body: "..."}}, f.err
}

func newFake() *fakeCommerce {
	return &fakeCommerce{
		orders: map[string]commerce.Order{
			"ORD-100105": order("delivered", 195_00, daysAgo(10), daysAgo(5)),
			"ORD-100107": order("delivered", 140_00, daysAgo(52), daysAgo(45)),
		},
		owner: map[string]string{"ORD-100105": "erin@example.com", "ORD-100107": "grace@example.com"},
	}
}

var (
	task105 = tasks.Task{ID: 1, Ticket: "HD-2005", Type: handoff.ReprintRequest, Reason: handoff.ReasonDamaged, OrderNumber: "ORD-100105", CustomerEmail: "erin@example.com"}
	task107 = tasks.Task{ID: 2, Ticket: "HD-2017", Type: handoff.ReprintRequest, Reason: handoff.ReasonDamaged, OrderNumber: "ORD-100107", CustomerEmail: "grace@example.com"}
)

func submit(id string, p Proposal) agent.Response { return agenttest.Call(id, SubmitProposalTool, p) }

func newHandler(c *fakeCommerce, scripts map[string]*agenttest.Script) *Handler {
	return &Handler{
		Commerce: c, ModelFor: agenttest.Router{Scripts: scripts}.ModelFor, ModelID: "fake/test",
		RefundLimitCents: limit, Limits: agent.Config{MaxSteps: 3, MaxTokens: 60_000},
		Now: func() time.Time { return now },
	}
}

func TestDecide(t *testing.T) {
	eligible := reprint(ReprintItem{"MUG-CER-11OZ", 250})
	tests := []struct {
		name    string
		task    tasks.Task
		script  *agenttest.Script
		commErr error
		okCalls int
		check   func(t *testing.T, out Outcome, err error)
	}{
		{
			name: "agreement: eligible reprint passes the gate", task: task105,
			script: agenttest.NewScript(agenttest.Call("c1", "get_task_order", map[string]string{"order_number": "ORD-100107"}), submit("c2", eligible)),
			check: func(t *testing.T, out Outcome, err error) {
				if err != nil || out.Source != "agent" || out.Final.Verdict != Eligible || out.Baseline != Eligible ||
					len(out.Overrides) != 0 || out.Model != "fake/test" || out.BaselineReason == "" {
					t.Fatalf("out = %+v, err = %v", out, err)
				}
			},
		},
		{
			name: "HD-2017: model says eligible, 45 days after delivery: needs_human", task: task107,
			script: agenttest.NewScript(submit("c1", reprint(ReprintItem{"PRT-ART-A4", 40}))),
			check: func(t *testing.T, out Outcome, err error) {
				if err != nil || out.Final.Verdict != NeedsHuman || out.Baseline != NotEligible || out.Proposed.Verdict != Eligible ||
					len(out.Final.ReprintItems) != 0 || !strings.Contains(strings.Join(out.Overrides, ""), "disagrees") {
					t.Fatalf("out = %+v, err = %v", out, err)
				}
			},
		},
		{
			name: "step limit: agent_limit needs_human, baseline still recorded", task: task105,
			script: agenttest.NewScript(agenttest.Text("hmm"), agenttest.Text("hmm"), agenttest.Text("hmm")),
			check: func(t *testing.T, out Outcome, err error) {
				if err != nil || out.Source != "agent_limit" || out.Final.Verdict != NeedsHuman || out.Proposed != nil ||
					out.Baseline != Eligible || !strings.Contains(out.Final.Reason, "step limit") {
					t.Fatalf("out = %+v, err = %v", out, err)
				}
			},
		},
		{
			name: "model outage is an error to retry", task: task105, script: nil, // Router: failing model
			check: func(t *testing.T, _ Outcome, err error) {
				if err == nil || agent.IsLimit(err) {
					t.Fatalf("err = %v, want a retryable error", err)
				}
			},
		},
		{
			name: "commerce outage during the loop is an error to retry", task: task105, commErr: errors.New("commerce api: 502"),
			script: agenttest.NewScript(agenttest.Call("c1", "get_task_order", nil), submit("c2", eligible)),
			check: func(t *testing.T, _ Outcome, err error) {
				if err == nil || !strings.Contains(err.Error(), "502") {
					t.Fatalf("err = %v, want the commerce failure", err)
				}
			},
		},
		{
			name: "commerce fails only on the gate's own fetch: an error to retry", task: task105,
			commErr: errors.New("commerce api: 503"), okCalls: 1,
			script: agenttest.NewScript(agenttest.Call("c1", "get_task_order", nil), submit("c2", eligible)),
			check: func(t *testing.T, _ Outcome, err error) {
				if err == nil || !strings.Contains(err.Error(), "order for the gate") {
					t.Fatalf("err = %v, want the gate fetch failure", err)
				}
			},
		},
		{
			name: "the API rejects the gate's fetch: needs_human like a missing order", task: task105,
			commErr: fmt.Errorf("%w: get order: 422", commerce.ErrRejected), okCalls: 1,
			script: agenttest.NewScript(agenttest.Call("c1", "get_task_order", nil), submit("c2", eligible)),
			check: func(t *testing.T, out Outcome, err error) {
				if err != nil || out.Source != "agent" || out.Final.Verdict != NeedsHuman || len(out.Overrides) != 1 {
					t.Fatalf("out = %+v, err = %v", out, err)
				}
			},
		},
		{
			name: "model proposes eligible but the order is not visible: needs_human", task: tasks.Task{Ticket: "HD-2098", Type: handoff.ReprintRequest,
				Reason: handoff.ReasonDamaged, OrderNumber: "ORD-100105", CustomerEmail: "mallory@example.com"},
			script: agenttest.NewScript(submit("c1", eligible)),
			check: func(t *testing.T, out Outcome, err error) {
				if err != nil || out.Source != "agent" || out.Proposed == nil || out.Proposed.Verdict != Eligible ||
					out.Final.Verdict != NeedsHuman || len(out.Final.ReprintItems) != 0 ||
					len(out.Overrides) != 1 || !strings.Contains(out.Overrides[0], "not visible") || out.Check(handoff.ReprintRequest) != nil {
					t.Fatalf("out = %+v, err = %v", out, err)
				}
			},
		},
		{
			name: "order not visible for the task's customer: needs_human", task: tasks.Task{Ticket: "HD-2099", Type: handoff.ReprintRequest,
				OrderNumber: "ORD-100105", CustomerEmail: "mallory@example.com"},
			script: agenttest.NewScript(submit("c1", verdict(NeedsHuman))),
			check: func(t *testing.T, out Outcome, err error) {
				if err != nil || out.Final.Verdict != NeedsHuman || len(out.Overrides) != 1 || out.Baseline != "" {
					t.Fatalf("out = %+v, err = %v", out, err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newFake()
			c.err, c.okCalls = tt.commErr, tt.okCalls
			scripts := map[string]*agenttest.Script{}
			if tt.script != nil {
				scripts[tt.task.Subject()] = tt.script
			}
			out, err := newHandler(c, scripts).Decide(context.Background(), tt.task, 1)
			tt.check(t, out, err)
		})
	}
}

// The model sees the task's type and order number, and no ticket text or email.
func TestDecide_InputAndPrompt(t *testing.T) {
	s := agenttest.NewScript(submit("c1", reprint(ReprintItem{"MUG-CER-11OZ", 1})))
	if _, err := newHandler(newFake(), map[string]*agenttest.Script{task105.Subject(): s}).Decide(context.Background(), task105, 1); err != nil {
		t.Fatal(err)
	}
	req := s.Requests[0]
	in := req.Messages[0].Text
	if req.System != SystemPrompt || !strings.HasSuffix(in, `{"task_type":"reprint_request","reason":"damaged","order_number":"ORD-100105"}`) ||
		strings.Contains(in, "erin@") || strings.Contains(in, "HD-2005") {
		t.Errorf("input = %q", in)
	}
	var names []string
	for _, spec := range req.Tools {
		names = append(names, spec.Name)
	}
	if strings.Join(names, ",") != "get_task_order,search_policy,submit_proposal" {
		t.Errorf("tools = %v", names)
	}
	if !json.Valid(req.Tools[2].InputSchema) {
		t.Error("submit_proposal schema is not JSON")
	}
}
