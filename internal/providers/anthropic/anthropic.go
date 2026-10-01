// Package anthropic adapts the Anthropic Messages API (POST /v1/messages) to
// agent.Model. Like every adapter it is stateless: each call sends the whole
// conversation.
//
// It sends neither temperature nor tool_choice. Newer models reject a
// non-default temperature, and tool_choice "any"/"tool" returns 400 on
// Opus/Sonnet 5.5; the default ("auto") is what the loop needs anyway, since
// it nudges the model when it answers in plain text.
package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/Andres568/support-triage-agent/internal/agent"
	"github.com/Andres568/support-triage-agent/internal/providers/httpjson"
)

// APIVersion is the anthropic-version header: the wire contract we coded to.
const APIVersion = "2023-06-01"

type Model struct {
	url, apiKey, model string
	maxTokens          int
	hc                 *http.Client
}

// New returns a model at baseURL (e.g. https://api.anthropic.com). maxTokens
// is required by the API: it is the output cap for each call. A nil hc means
// http.DefaultClient.
func New(baseURL, apiKey, model string, maxTokens int, hc *http.Client) *Model {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Model{url: strings.TrimSuffix(baseURL, "/") + "/v1/messages", apiKey: apiKey, model: model, maxTokens: maxTokens, hc: hc}
}

func (m *Model) Generate(ctx context.Context, req agent.Request) (agent.Response, error) {
	headers := http.Header{}
	headers.Set("x-api-key", m.apiKey)
	headers.Set("anthropic-version", APIVersion)
	var out response
	// httpjson errors carry the status and the provider's message, never our
	// headers, so the key cannot leak through them.
	if err := httpjson.Post(ctx, m.hc, m.url, headers, m.request(req), &out); err != nil {
		return agent.Response{}, fmt.Errorf("anthropic: %w", err)
	}
	return m.parse(out)
}

// Wire types: only the fields we send or read.
type request struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	System    string    `json:"system,omitempty"`
	Messages  []message `json:"messages"`
	Tools     []tool    `json:"tools,omitempty"`
}

type tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type message struct {
	Role    string  `json:"role"`
	Content []block `json:"content"`
}

// block is any content block; omitempty keeps each type to its own fields.
// IsError is a pointer so a tool_result always carries it and nothing else does.
type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   *bool           `json:"is_error,omitempty"`
}

type response struct {
	ID         string  `json:"id"`
	Model      string  `json:"model"`
	StopReason string  `json:"stop_reason"`
	Content    []block `json:"content"`
	Usage      struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

func (m *Model) request(req agent.Request) request {
	r := request{Model: m.model, MaxTokens: m.maxTokens, System: req.System, Messages: []message{}}
	for _, t := range req.Tools {
		r.Tools = append(r.Tools, tool{Name: t.Name, Description: t.Description, InputSchema: t.Schema()})
	}
	for _, msg := range req.Messages {
		m := toMessage(msg)
		// The API rejects a turn with empty content. An assistant reply with
		// neither text nor calls (then nudged by the loop) is dropped...
		if len(m.Content) == 0 {
			continue
		}
		// ...which leaves two user turns in a row: merge them. Tool results
		// stay first, since they were first in the earlier turn.
		if n := len(r.Messages); n > 0 && r.Messages[n-1].Role == m.Role {
			r.Messages[n-1].Content = append(r.Messages[n-1].Content, m.Content...)
			continue
		}
		r.Messages = append(r.Messages, m)
	}
	return r
}

func toMessage(msg agent.Message) message {
	var content []block
	if msg.Role == agent.RoleAssistant {
		if msg.Text != "" {
			content = append(content, block{Type: "text", Text: msg.Text})
		}
		for _, c := range msg.ToolCalls {
			content = append(content, block{Type: "tool_use", ID: c.ID, Name: c.Name, Input: input(c.Args)})
		}
		return message{Role: "assistant", Content: content}
	}
	// tool_result blocks must come first in the turn right after the
	// tool_use turn; the API rejects them anywhere else.
	for _, r := range msg.ToolResults {
		isErr := r.IsError
		content = append(content, block{Type: "tool_result", ToolUseID: r.CallID, Content: r.Content, IsError: &isErr})
	}
	if msg.Text != "" {
		content = append(content, block{Type: "text", Text: msg.Text})
	}
	return message{Role: "user", Content: content}
}

// input echoes a call's arguments. The API requires an object; arguments that
// are not one (a malformed call kept as a JSON string) are sent as {}: the
// tool_result that follows already tells the model what was wrong.
func input(args json.RawMessage) json.RawMessage {
	var obj map[string]json.RawMessage
	if json.Unmarshal(args, &obj) != nil || obj == nil {
		return json.RawMessage(`{}`)
	}
	return args
}

func (m *Model) parse(r response) (agent.Response, error) {
	u := r.Usage
	resp := agent.Response{
		Message:    agent.Message{Role: agent.RoleAssistant},
		ID:         r.ID,
		Model:      r.Model,
		StopReason: r.StopReason,
		Usage: agent.Usage{
			InputTokens:      u.InputTokens, // already uncached
			OutputTokens:     u.OutputTokens,
			CacheReadTokens:  u.CacheReadInputTokens,
			CacheWriteTokens: u.CacheCreationInputTokens,
		},
	}
	switch r.StopReason {
	case "end_turn", "tool_use", "stop_sequence":
	case "max_tokens":
		return resp, fmt.Errorf("anthropic: %w (max_tokens %d)", agent.ErrTruncated, m.maxTokens)
	case "refusal":
		return resp, fmt.Errorf("anthropic: %w", agent.ErrRefused)
	case "model_context_window_exceeded":
		return resp, fmt.Errorf("anthropic: %w", agent.ErrContextWindow)
	default:
		// pause_turn only happens with server tools, which we never enable.
		return resp, fmt.Errorf("anthropic: unexpected stop_reason %q", r.StopReason)
	}

	var text strings.Builder
	for _, b := range r.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			resp.Message.ToolCalls = append(resp.Message.ToolCalls, agent.ToolCall{ID: b.ID, Name: b.Name, Args: b.Input})
		}
		// thinking and redacted_thinking are dropped: we never enable thinking.
	}
	resp.Message.Text = text.String()
	return resp, nil
}
