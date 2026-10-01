package evals

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/Andres568/support-triage-agent/internal/agent"
	"github.com/Andres568/support-triage-agent/internal/handoff"
	"github.com/Andres568/support-triage-agent/internal/orders"
	"github.com/Andres568/support-triage-agent/internal/triage"
)

// OracleModel is the registry id of the oracle (see providers.Models).
const OracleModel = "fake/oracle"

// oracleConfidence is high enough to pass the auto-reply confidence floor,
// so only the policy's category allowlist caps an oracle's auto_reply.
const oracleConfidence = 0.95

// oracleDraft avoids every must_not_contain term and the word "refund" (the
// gate reads a draft that mentions one as a refund request).
const oracleDraft = "Thanks for contacting us. We looked into your message, and a teammate will follow up if anything else is needed."

// SupportOracle answers one ticket with its golden decision. Like a careful
// model, it first looks up the order the customer named, so the tools, the
// HTTP boundary and the gate's facts all run. It is the CI stand-in for a
// model: every metric must come out at its expected value.
func SupportOracle(g Golden) agent.Model {
	return oracleFunc(func(req agent.Request) (agent.ToolCall, error) {
		var in struct {
			Order string `json:"order_number_claimed"`
		}
		if err := inputJSON(req, &in); err != nil {
			return agent.ToolCall{}, err
		}
		if in.Order != "" && !calledAny(req) {
			args, err := json.Marshal(map[string]string{"order_number": in.Order})
			return agent.ToolCall{ID: "oracle-1", Name: "get_order", Args: args}, err
		}
		draft := oracleDraft
		if g.Action == triage.ActionEscalate {
			draft = ""
		}
		args, err := json.Marshal(triage.Decision{
			Category: g.Category, Action: g.Action, DraftReply: draft, Confidence: oracleConfidence,
			Reason: "oracle: golden label", FollowUps: append([]handoff.FollowUp{}, g.FollowUps...),
		})
		return agent.ToolCall{ID: "oracle-2", Name: triage.SubmitDecisionTool, Args: args}, err
	})
}

// OrdersOracle answers one task with its golden verdict after reading the
// task's order, whose items and total make an eligible proposal valid.
func OrdersOracle(g GoldenTask) agent.Model {
	return oracleFunc(func(req agent.Request) (agent.ToolCall, error) {
		if !calledAny(req) {
			return agent.ToolCall{ID: "oracle-1", Name: "get_task_order", Args: json.RawMessage(`{}`)}, nil
		}
		var o struct {
			Items      []orders.ReprintItem `json:"items"`
			TotalCents int                  `json:"total_cents"`
		}
		last := req.Messages[len(req.Messages)-1]
		if len(last.ToolResults) > 0 && !last.ToolResults[0].IsError {
			if err := json.Unmarshal([]byte(last.ToolResults[0].Content), &o); err != nil {
				return agent.ToolCall{}, err
			}
		}
		p := orders.Proposal{Verdict: g.Verdict, PolicySlug: oracleSlug(g), Reason: "oracle: golden label", ReprintItems: []orders.ReprintItem{}}
		if g.Verdict == orders.Eligible {
			switch g.Type {
			case handoff.ReprintRequest:
				p.ReprintItems = o.Items
			case handoff.RefundReview:
				p.RefundCents = o.TotalCents
				if g.RefundCents != nil {
					p.RefundCents = *g.RefundCents
				}
			}
		}
		args, err := json.Marshal(p)
		return agent.ToolCall{ID: "oracle-2", Name: orders.SubmitProposalTool, Args: args}, err
	})
}

func oracleSlug(g GoldenTask) string {
	switch {
	case g.Type == handoff.AddressChange:
		return "address-changes"
	case g.Type == handoff.RefundReview:
		return "refunds"
	case g.Reason == handoff.ReasonLostInTransit:
		return "shipping-delay"
	}
	return "reprint-damaged"
}

type oracleFunc func(agent.Request) (agent.ToolCall, error)

func (f oracleFunc) Generate(ctx context.Context, req agent.Request) (agent.Response, error) {
	if err := ctx.Err(); err != nil {
		return agent.Response{}, err
	}
	call, err := f(req)
	if err != nil {
		return agent.Response{}, err
	}
	return agent.Response{Model: OracleModel, Message: agent.Message{Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{call}}}, nil
}

// calledAny reports whether the oracle already made its lookup.
func calledAny(req agent.Request) bool {
	for _, m := range req.Messages {
		if len(m.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

// inputJSON decodes the JSON object in the first user message (after the
// agent's preamble).
func inputJSON(req agent.Request, v any) error {
	if len(req.Messages) == 0 {
		return errors.New("oracle: no input message")
	}
	text := req.Messages[0].Text
	i := strings.Index(text, "{")
	if i < 0 {
		return errors.New("oracle: input has no JSON")
	}
	return json.Unmarshal([]byte(text[i:]), v)
}
