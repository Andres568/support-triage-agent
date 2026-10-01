package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Errors that stop the loop. Tool failures are not here: they go back to the
// model as results. These are failures the model cannot fix by itself.
var (
	ErrMaxSteps       = errors.New("agent: step limit reached without a final answer")
	ErrBudgetExceeded = errors.New("agent: token budget exceeded")
)

// Config bounds one run. Every limit is enforced in code, not in the prompt.
type Config struct {
	System   string
	Tools    []Tool
	MaxSteps int // model calls allowed per run

	// MaxTokens caps input+output tokens across all steps (0 = no cap). The
	// conversation is resent every step, so cost grows with each one.
	MaxTokens int

	// Finish is the terminal tool: when the model calls it with arguments that
	// pass Validate, the run ends and those arguments are the result. If
	// Validate fails, the error goes back to the model so it can correct them.
	Finish   ToolSpec
	Validate func(args json.RawMessage) error
}

type Result struct {
	Final      json.RawMessage // validated arguments of the Finish call
	Steps      int
	Usage      Usage
	Transcript []Message // full conversation, for auditing and debugging
}

// Nudge is sent when the model answers in plain text instead of calling a tool.
const Nudge = "You must finish by calling the submit tool. Do not answer in plain text."

// Run executes the tool loop for one input until the model calls Finish with
// valid arguments, or a limit stops it. On error, the partial Result is still
// returned so callers can record what happened.
func Run(ctx context.Context, m Model, cfg Config, input string) (Result, error) {
	tools := make(map[string]Tool, len(cfg.Tools))
	specs := make([]ToolSpec, 0, len(cfg.Tools)+1)
	for _, t := range cfg.Tools {
		s := t.Spec()
		tools[s.Name] = t
		specs = append(specs, s)
	}
	specs = append(specs, cfg.Finish)

	res := Result{Transcript: []Message{{Role: RoleUser, Text: input}}}

	// A too-large input fails before any call is billed: every step resends
	// it, so it would only burn the budget and then fail anyway.
	if est := EstimateInputTokens(cfg.System, specs, input); cfg.MaxTokens > 0 && est > cfg.MaxTokens {
		return res, fmt.Errorf("%w: input estimated at %d tokens before step 1, budget %d", ErrBudgetExceeded, est, cfg.MaxTokens)
	}

	for res.Steps < cfg.MaxSteps {
		// Respect cancellation and deadlines between steps, not only inside calls.
		if err := ctx.Err(); err != nil {
			return res, err
		}

		resp, err := m.Generate(ctx, Request{System: cfg.System, Messages: res.Transcript, Tools: specs})
		res.Steps++
		// Before the error check: a failed call (truncated, refused) is billed too.
		res.Usage.Add(resp.Usage)
		if err != nil {
			return res, fmt.Errorf("agent: model call %d: %w", res.Steps, err)
		}
		res.Transcript = append(res.Transcript, resp.Message)

		if cfg.MaxTokens > 0 && res.Usage.Total() > cfg.MaxTokens {
			return res, fmt.Errorf("%w: used %d of %d", ErrBudgetExceeded, res.Usage.Total(), cfg.MaxTokens)
		}

		if len(resp.Message.ToolCalls) == 0 {
			res.Transcript = append(res.Transcript, Message{Role: RoleUser, Text: Nudge})
			continue
		}

		results := make([]ToolResult, 0, len(resp.Message.ToolCalls))
		for _, call := range resp.Message.ToolCalls {
			if call.Name == cfg.Finish.Name {
				if err := cfg.Validate(call.Args); err != nil {
					results = append(results, ToolResult{CallID: call.ID, Content: "invalid arguments: " + err.Error(), IsError: true})
					continue
				}
				res.Final = call.Args
				return res, nil
			}
			results = append(results, runTool(ctx, tools, call))
		}
		// A deadline that expired during a tool call is not the model's problem.
		if err := ctx.Err(); err != nil {
			return res, err
		}
		res.Transcript = append(res.Transcript, Message{Role: RoleUser, ToolResults: results})
	}
	return res, fmt.Errorf("%w (%d)", ErrMaxSteps, cfg.MaxSteps)
}

func runTool(ctx context.Context, tools map[string]Tool, call ToolCall) ToolResult {
	t, ok := tools[call.Name]
	if !ok {
		return ToolResult{CallID: call.ID, Content: fmt.Sprintf("unknown tool %q", call.Name), IsError: true}
	}
	out, err := t.Call(ctx, call.Args)
	if err != nil {
		return ToolResult{CallID: call.ID, Content: err.Error(), IsError: true}
	}
	return ToolResult{CallID: call.ID, Content: out}
}

// limits are failures of the model to decide, not of the system.
var limits = []error{ErrMaxSteps, ErrBudgetExceeded, ErrTruncated, ErrRefused, ErrContextWindow}

// IsLimit reports whether err means the model could not decide within its
// limits. That is a product outcome (a human takes the item), not a retry:
// retrying a model that could not decide only costs money again.
func IsLimit(err error) bool {
	for _, target := range limits {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// bytesPerToken is deliberately low (English averages about 4 bytes per
// token): the estimate should over-count, so a borderline input escalates
// instead of being billed.
const bytesPerToken = 3

// EstimateInputTokens is a cheap upper-bound guess of the first request's
// input tokens, from its size in bytes. No tokenizer: it only has to catch
// an input that cannot fit, not price one that can.
func EstimateInputTokens(system string, tools []ToolSpec, input string) int {
	n := len(system) + len(input)
	for _, t := range tools {
		n += len(t.Name) + len(t.Description) + len(t.Schema())
	}
	return n / bytesPerToken
}
