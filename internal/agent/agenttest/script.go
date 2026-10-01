// Package agenttest provides a scripted fake Model for deterministic tests.
package agenttest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/Andres568/support-triage-agent/internal/agent"
	"github.com/Andres568/support-triage-agent/internal/triage"
)

// Script replays canned responses in order and records every request, so
// tests can assert both what the loop did and what it sent to the model.
type Script struct {
	mu        sync.Mutex
	responses []agent.Response
	Requests  []agent.Request
}

func NewScript(responses ...agent.Response) *Script {
	return &Script{responses: responses}
}

func (s *Script) Generate(ctx context.Context, req agent.Request) (agent.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return agent.Response{}, err
	}
	s.Requests = append(s.Requests, req)
	if len(s.Requests) > len(s.responses) {
		return agent.Response{}, fmt.Errorf("agenttest: script exhausted after %d responses", len(s.responses))
	}
	return s.responses[len(s.Requests)-1], nil
}

// Call builds an assistant response that calls one tool.
func Call(id, name string, args any) agent.Response {
	raw, err := json.Marshal(args)
	if err != nil {
		panic(err)
	}
	return agent.Response{
		Message: agent.Message{
			Role:      agent.RoleAssistant,
			ToolCalls: []agent.ToolCall{{ID: id, Name: name, Args: raw}},
		},
		Usage: agent.Usage{InputTokens: 100, OutputTokens: 20},
	}
}

// Text builds an assistant response with plain text and no tool calls.
func Text(s string) agent.Response {
	return agent.Response{
		Message: agent.Message{Role: agent.RoleAssistant, Text: s},
		Usage:   agent.Usage{InputTokens: 100, OutputTokens: 20},
	}
}

// Submit builds an assistant response that submits a triage decision.
func Submit(id string, d triage.Decision) agent.Response {
	return Call(id, triage.SubmitDecisionTool, d)
}

// Router hands each ticket its own Script, keyed by subject (e.g. "HD-2005").
// Its ModelFor method fits the handlers' model factory. A subject without a
// script gets a model that fails, so an unexpected model call is visible.
type Router struct {
	Scripts map[string]*Script
}

func (r Router) ModelFor(subject string, attempt int) agent.Model {
	if s, ok := r.Scripts[subject]; ok {
		return s
	}
	return failing(fmt.Sprintf("agenttest: no script for %s (attempt %d)", subject, attempt))
}

type failing string

func (f failing) Generate(context.Context, agent.Request) (agent.Response, error) {
	return agent.Response{}, errors.New(string(f))
}
