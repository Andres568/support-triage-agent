// Package agent implements a provider-neutral tool-calling loop.
//
// It knows nothing about support tickets: callers supply the prompt, the
// tools and a terminal "finish" tool whose arguments are the final answer.
package agent

import (
	"context"
	"encoding/json"
	"errors"
)

// Model is one LLM provider. Each call is stateless: the whole conversation
// is sent every time. Adapters (Anthropic, OpenAI-compatible, fakes) translate
// these neutral types to and from their wire format.
type Model interface {
	// Generate makes one call. The returned Response's Usage is meaningful
	// even when err != nil: a truncated, refused or over-window reply was
	// still billed, so adapters fill Usage whenever the provider reported it
	// and callers must count it before checking err.
	Generate(ctx context.Context, req Request) (Response, error)
}

// Provider outcomes that mean the model could not produce a usable answer.
// Adapters wrap them; callers treat them as the model's failure to decide
// (escalate), not as an outage to retry.
var (
	ErrTruncated     = errors.New("agent: model output truncated")
	ErrRefused       = errors.New("agent: model refused")
	ErrContextWindow = errors.New("agent: context window exceeded")
)

type Request struct {
	System   string
	Messages []Message
	Tools    []ToolSpec
}

type Response struct {
	Message    Message // always RoleAssistant
	Usage      Usage
	ID         string // provider response id; not unique across providers (Ollama counts from 1)
	Model      string // model name the provider reports, which may differ from the one asked for
	StopReason string // provider's own vocabulary, for debugging only
}

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one turn. An assistant turn may carry text and/or tool calls;
// a user turn carries text and/or the results of the previous tool calls.
type Message struct {
	Role        Role         `json:"role"`
	Text        string       `json:"text,omitempty"`
	ToolCalls   []ToolCall   `json:"tool_calls,omitempty"`
	ToolResults []ToolResult `json:"tool_results,omitempty"`
}

// ToolCall is the model asking to run a tool. The model never runs anything
// itself; the loop decides whether and how to execute it.
type ToolCall struct {
	ID   string          `json:"id"` // provider-assigned; ties the result back to the call
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type ToolResult struct {
	CallID  string `json:"call_id"`
	Content string `json:"content"`
	IsError bool   `json:"is_error,omitempty"` // lets the model see that the call failed and try to recover
}

// ToolSpec describes a tool to the model.
type ToolSpec struct {
	Name        string
	Description string
	InputSchema json.RawMessage // JSON Schema of Args; nil means "no arguments"
}

// emptySchema is sent for a tool without an InputSchema: providers require
// an object schema.
var emptySchema = json.RawMessage(`{"type":"object","properties":{}}`)

// Schema is InputSchema, or an empty object schema when it is unset.
func (s ToolSpec) Schema() json.RawMessage {
	if len(s.InputSchema) == 0 {
		return emptySchema
	}
	return s.InputSchema
}

// Usage is billed tokens for one or more calls. InputTokens is UNCACHED input
// (Anthropic reports it that way; OpenAI adapters subtract cached tokens), so
// each field has its own price and they add up without double counting.
type Usage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
	ReasoningTokens  int `json:"reasoning_tokens,omitempty"` // a subset of OutputTokens, informational
}

// Total is every token the provider processed, cached or not: the budget
// counts work, not price.
func (u Usage) Total() int {
	return u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens + u.OutputTokens
}

func (u *Usage) Add(o Usage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.CacheReadTokens += o.CacheReadTokens
	u.CacheWriteTokens += o.CacheWriteTokens
	u.ReasoningTokens += o.ReasoningTokens
}

// Tool is something the model may ask the loop to run.
type Tool interface {
	Spec() ToolSpec
	// Call runs the tool. A returned error is shown to the model as a failed
	// result (it can correct its arguments or try something else); it does
	// not stop the loop.
	Call(ctx context.Context, args json.RawMessage) (string, error)
}
