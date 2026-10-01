package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Andres568/support-triage-agent/internal/agent"
	"github.com/Andres568/support-triage-agent/internal/handoff"
	"github.com/Andres568/support-triage-agent/internal/triage"
)

func newFake(id string) (agent.Model, error) {
	switch id {
	case "fake/escalate-all":
		return escalateAll{}, nil
	case "fake/oracle":
		return nil, ErrOracleOutsideEvals
	}
	return nil, fmt.Errorf("%w %q", ErrUnknownModel, id)
}

// ErrOracleOutsideEvals: fake/oracle answers each item with its golden label,
// so only the eval harness (internal/evals), which holds the labels, can
// build it. It is in the registry for its id and its (zero) price.
var ErrOracleOutsideEvals = errors.New("providers: fake/oracle answers from golden labels; run it with cmd/eval")

// escalateAll submits an escalation for every ticket without reading it. It
// exercises the whole pipeline (claim, loop, gate, finalize, outbox) with no
// LLM, and is always safe: every ticket ends with a human.
type escalateAll struct{}

func (escalateAll) Generate(context.Context, agent.Request) (agent.Response, error) {
	args, err := json.Marshal(triage.Decision{
		Category:  triage.CategoryGeneral,
		Action:    triage.ActionEscalate,
		Reason:    "fake/escalate-all escalates every ticket",
		FollowUps: []handoff.FollowUp{},
	})
	if err != nil {
		return agent.Response{}, err
	}
	return agent.Response{Message: agent.Message{
		Role:      agent.RoleAssistant,
		ToolCalls: []agent.ToolCall{{ID: "fake-1", Name: triage.SubmitDecisionTool, Args: args}},
	}}, nil
}
