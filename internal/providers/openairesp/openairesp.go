// Package openairesp adapts the OpenAI Responses API (POST /responses) to
// agent.Model. OpenAI, xAI and Ollama all speak it, so one adapter covers
// three providers.
//
// It is stateless on purpose: every call sends the whole conversation with
// store:false and never a previous_response_id, so no provider keeps our
// customers' tickets and a retry never depends on server-side state.
package openairesp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Andres568/support-triage-agent/internal/agent"
	"github.com/Andres568/support-triage-agent/internal/providers/httpjson"
)

// Options tune one model. Zero values leave the provider's default.
type Options struct {
	MaxOutputTokens int
	ReasoningEffort string   // e.g. "none" for qwen3 on Ollama: 4x faster, and usage is reported correctly
	Temperature     *float64 // nil: not sent
	// ContextWindow enables the silent-truncation guard (0 = off). Ollama
	// drops the head of a too-long prompt instead of failing, which would
	// silently lose the system prompt.
	ContextWindow int
}

// contextGuard is the share of the window the input may fill before we
// assume the head of the prompt was, or is about to be, dropped.
const contextGuard = 0.9

// errorPrefix marks a failed tool result: the Responses API has no is_error.
const errorPrefix = "ERROR: "

type Model struct {
	url, apiKey, model string
	opts               Options
	hc                 *http.Client
}

// New returns a model at baseURL (e.g. https://api.openai.com/v1). With an
// empty apiKey no Authorization header is sent (Ollama needs none). A nil hc
// means http.DefaultClient.
func New(baseURL, apiKey, model string, opts Options, hc *http.Client) *Model {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Model{url: strings.TrimSuffix(baseURL, "/") + "/responses", apiKey: apiKey, model: model, opts: opts, hc: hc}
}

func (m *Model) Generate(ctx context.Context, req agent.Request) (agent.Response, error) {
	headers := http.Header{}
	if m.apiKey != "" {
		headers.Set("Authorization", "Bearer "+m.apiKey)
	}
	var out response
	if err := httpjson.Post(ctx, m.hc, m.url, headers, m.request(req), &out); err != nil {
		var herr *httpjson.HTTPError
		if errors.As(err, &herr) && herr.Code == "context_length_exceeded" {
			// The model could not take the input: an agent limit, not an
			// outage. The HTTPError stays in the chain for debugging.
			return agent.Response{}, fmt.Errorf("openairesp: %w: %w", agent.ErrContextWindow, err)
		}
		return agent.Response{}, fmt.Errorf("openairesp: %w", err)
	}
	return m.parse(out)
}

// Wire types: only the fields we send or read.
type request struct {
	Model           string     `json:"model"`
	Instructions    string     `json:"instructions,omitempty"`
	Input           []any      `json:"input"`
	Tools           []tool     `json:"tools,omitempty"`
	ToolChoice      string     `json:"tool_choice,omitempty"`
	MaxOutputTokens int        `json:"max_output_tokens,omitempty"`
	Store           bool       `json:"store"`
	Reasoning       *reasoning `json:"reasoning,omitempty"`
	Temperature     *float64   `json:"temperature,omitempty"`
}

type reasoning struct {
	Effort string `json:"effort"`
}

type tool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	// Strict mode would require every property in "required" and reject
	// our optional fields; the loop validates arguments itself.
	Strict bool `json:"strict"`
}

type userMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type assistantMessage struct {
	Type    string        `json:"type"`
	Role    string        `json:"role"`
	Content []contentPart `json:"content"`
}

type contentPart struct {
	Type    string `json:"type"`
	Text    string `json:"text,omitempty"`
	Refusal string `json:"refusal,omitempty"`
}

// functionCall echoes a call back without its item id: with store:false the
// server has no item to refer to; call_id is what ties the output to it.
type functionCall struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type functionCallOutput struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

type response struct {
	ID                string `json:"id"`
	Model             string `json:"model"`
	Status            string `json:"status"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	// Error is set when status is "failed", even on a 200.
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Output []struct {
		Type      string        `json:"type"`
		CallID    string        `json:"call_id"`
		Name      string        `json:"name"`
		Arguments string        `json:"arguments"`
		Content   []contentPart `json:"content"`
	} `json:"output"`
	Usage struct {
		InputTokens        int `json:"input_tokens"`
		OutputTokens       int `json:"output_tokens"`
		InputTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
		OutputTokensDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
	} `json:"usage"`
}

func (m *Model) request(req agent.Request) request {
	r := request{
		Model:           m.model,
		Instructions:    req.System,
		Input:           []any{},
		ToolChoice:      "auto",
		MaxOutputTokens: m.opts.MaxOutputTokens,
		Temperature:     m.opts.Temperature,
	}
	if m.opts.ReasoningEffort != "" {
		r.Reasoning = &reasoning{Effort: m.opts.ReasoningEffort}
	}
	for _, t := range req.Tools {
		r.Tools = append(r.Tools, tool{Type: "function", Name: t.Name, Description: t.Description, Parameters: t.Schema()})
	}
	if len(r.Tools) == 0 {
		r.ToolChoice = ""
	}
	for _, msg := range req.Messages {
		r.Input = append(r.Input, inputItems(msg)...)
	}
	return r
}

func inputItems(msg agent.Message) []any {
	var items []any
	if msg.Role == agent.RoleAssistant {
		if msg.Text != "" {
			items = append(items, assistantMessage{Type: "message", Role: "assistant",
				Content: []contentPart{{Type: "output_text", Text: msg.Text}}})
		}
		for _, c := range msg.ToolCalls {
			items = append(items, functionCall{Type: "function_call", CallID: c.ID, Name: c.Name, Arguments: string(c.Args)})
		}
		return items
	}
	// Outputs first: they answer the calls of the previous turn.
	for _, r := range msg.ToolResults {
		out := r.Content
		if r.IsError {
			out = errorPrefix + out
		}
		items = append(items, functionCallOutput{Type: "function_call_output", CallID: r.CallID, Output: out})
	}
	if msg.Text != "" {
		items = append(items, userMessage{Role: "user", Content: msg.Text})
	}
	return items
}

func (m *Model) parse(r response) (agent.Response, error) {
	u := r.Usage
	resp := agent.Response{
		Message:    agent.Message{Role: agent.RoleAssistant},
		ID:         r.ID,
		Model:      r.Model,
		StopReason: r.Status,
		// input_tokens includes the cached prefix, so the uncached part is
		// the difference. Ollama follows this too (checked 2026-09-30: two
		// identical calls gave 619 input, then 619 with 618 cached): a warm
		// Ollama call stores a small input_tokens, which is right.
		Usage: agent.Usage{
			InputTokens:     u.InputTokens - u.InputTokensDetails.CachedTokens,
			CacheReadTokens: u.InputTokensDetails.CachedTokens,
			OutputTokens:    u.OutputTokens,
			ReasoningTokens: u.OutputTokensDetails.ReasoningTokens,
		},
	}
	if w := m.opts.ContextWindow; w > 0 && float64(u.InputTokens) >= contextGuard*float64(w) {
		return resp, fmt.Errorf("openairesp: %w: %d input tokens of a %d window", agent.ErrContextWindow, u.InputTokens, w)
	}
	if r.Status == "incomplete" {
		reason := ""
		if r.IncompleteDetails != nil {
			reason = r.IncompleteDetails.Reason
		}
		resp.StopReason = "incomplete:" + reason
		switch reason {
		case "max_output_tokens":
			return resp, fmt.Errorf("openairesp: %w (max_output_tokens %d)", agent.ErrTruncated, m.opts.MaxOutputTokens)
		case "content_filter":
			return resp, fmt.Errorf("openairesp: %w (content_filter)", agent.ErrRefused)
		default:
			return resp, fmt.Errorf("openairesp: incomplete response: %q", reason)
		}
	}
	// Anything but "completed" (failed, cancelled, in_progress, queued, or
	// a status we do not know) has no answer we can trust. Some compatible
	// servers omit the status: that counts as completed only with output.
	completed := r.Status == "completed" || (r.Status == "" && len(r.Output) > 0)
	if !completed {
		msg := "no error details"
		if r.Error != nil {
			msg = httpjson.Truncate(r.Error.Code + ": " + r.Error.Message)
		}
		return resp, fmt.Errorf("openairesp: response status %q: %s", r.Status, msg)
	}

	var text strings.Builder
	for _, item := range r.Output {
		switch item.Type {
		case "message":
			for _, p := range item.Content {
				switch p.Type {
				case "output_text":
					text.WriteString(p.Text)
				case "refusal":
					return resp, fmt.Errorf("openairesp: %w: %s", agent.ErrRefused, p.Refusal)
				}
			}
		case "function_call":
			resp.Message.ToolCalls = append(resp.Message.ToolCalls,
				agent.ToolCall{ID: item.CallID, Name: item.Name, Args: args(item.Arguments)})
		}
		// "reasoning" and anything else is dropped: with store:false and no
		// encrypted content it could not be replayed anyway.
	}
	resp.Message.Text = text.String()
	return resp, nil
}

// args keeps malformed arguments as a JSON string: still valid JSON for the
// transcript, and the tool's own validation tells the model what is wrong.
func args(s string) json.RawMessage {
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	b, _ := json.Marshal(s) // marshaling a string cannot fail
	return b
}
