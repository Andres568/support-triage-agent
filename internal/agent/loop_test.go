package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Andres568/support-triage-agent/internal/agent"
	"github.com/Andres568/support-triage-agent/internal/agent/agenttest"
)

// funcTool adapts a function to agent.Tool.
type funcTool struct {
	name string
	fn   func(ctx context.Context, args json.RawMessage) (string, error)
}

func (t funcTool) Spec() agent.ToolSpec {
	return agent.ToolSpec{Name: t.name, InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (t funcTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	return t.fn(ctx, args)
}

var echo = funcTool{name: "echo", fn: func(_ context.Context, args json.RawMessage) (string, error) {
	return string(args), nil
}}

type answer struct {
	Answer string `json:"answer"`
}

// config returns a Config whose finish tool accepts {"answer": "<non-empty>"}.
func config(tools ...agent.Tool) agent.Config {
	return agent.Config{
		System:   "test",
		Tools:    tools,
		MaxSteps: 5,
		Finish:   agent.ToolSpec{Name: "submit"},
		Validate: func(raw json.RawMessage) error {
			var a answer
			if err := json.Unmarshal(raw, &a); err != nil {
				return err
			}
			if a.Answer == "" {
				return errors.New("answer is required")
			}
			return nil
		},
	}
}

func TestRun_ToolThenFinish(t *testing.T) {
	m := agenttest.NewScript(
		agenttest.Call("c1", "echo", map[string]string{"q": "hi"}),
		agenttest.Call("c2", "submit", answer{"done"}),
	)

	res, err := agent.Run(context.Background(), m, config(echo), "ticket")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := string(res.Final); got != `{"answer":"done"}` {
		t.Errorf("Final = %s", got)
	}
	if res.Steps != 2 {
		t.Errorf("Steps = %d, want 2", res.Steps)
	}
	if res.Usage.Total() != 240 {
		t.Errorf("Usage.Total = %d, want 240", res.Usage.Total())
	}

	// The second request must carry the tool result from the first step.
	last := m.Requests[1].Messages
	got := last[len(last)-1].ToolResults
	if len(got) != 1 || got[0].CallID != "c1" || got[0].Content != `{"q":"hi"}` || got[0].IsError {
		t.Errorf("tool result sent to model = %+v", got)
	}
	// The finish tool is offered to the model alongside the regular tools.
	if names := toolNames(m.Requests[0].Tools); names != "echo,submit" {
		t.Errorf("tools offered = %s", names)
	}
}

func TestRun_ToolErrorsGoBackToModel(t *testing.T) {
	failing := funcTool{name: "get_order", fn: func(context.Context, json.RawMessage) (string, error) {
		return "", errors.New("order ORD-1 not found")
	}}
	m := agenttest.NewScript(
		agenttest.Call("c1", "get_order", nil),
		agenttest.Call("c2", "no_such_tool", nil),
		agenttest.Call("c3", "submit", answer{"escalating"}),
	)

	res, err := agent.Run(context.Background(), m, config(failing), "ticket")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Steps != 3 {
		t.Errorf("Steps = %d, want 3", res.Steps)
	}
	assertLastResult(t, m.Requests[1], "order ORD-1 not found")
	assertLastResult(t, m.Requests[2], `unknown tool "no_such_tool"`)
}

func TestRun_InvalidFinishArgsCanBeCorrected(t *testing.T) {
	m := agenttest.NewScript(
		agenttest.Call("c1", "submit", answer{""}),
		agenttest.Call("c2", "submit", answer{"fixed"}),
	)

	res, err := agent.Run(context.Background(), m, config(), "ticket")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(res.Final) != `{"answer":"fixed"}` {
		t.Errorf("Final = %s", res.Final)
	}
	assertLastResult(t, m.Requests[1], "invalid arguments: answer is required")
}

func TestRun_PlainTextIsNudged(t *testing.T) {
	m := agenttest.NewScript(
		agenttest.Text("Your order is on its way!"),
		agenttest.Call("c1", "submit", answer{"ok"}),
	)

	if _, err := agent.Run(context.Background(), m, config(), "ticket"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	msgs := m.Requests[1].Messages
	if last := msgs[len(msgs)-1]; last.Role != agent.RoleUser || last.Text != agent.Nudge {
		t.Errorf("last message = %+v, want nudge", last)
	}
}

func TestRun_MaxSteps(t *testing.T) {
	m := agenttest.NewScript(
		agenttest.Call("c1", "echo", nil),
		agenttest.Call("c2", "echo", nil),
		agenttest.Call("c3", "echo", nil),
	)
	cfg := config(echo)
	cfg.MaxSteps = 3

	res, err := agent.Run(context.Background(), m, cfg, "ticket")
	if !errors.Is(err, agent.ErrMaxSteps) {
		t.Fatalf("err = %v, want ErrMaxSteps", err)
	}
	if res.Steps != 3 || res.Final != nil {
		t.Errorf("Steps = %d, Final = %s", res.Steps, res.Final)
	}
}

func TestRun_TokenBudget(t *testing.T) {
	m := agenttest.NewScript(
		agenttest.Call("c1", "echo", nil), // 120 tokens
		agenttest.Call("c2", "echo", nil), // 240 total > 200
		agenttest.Call("c3", "submit", answer{"too late"}),
	)
	cfg := config(echo)
	cfg.MaxTokens = 200

	res, err := agent.Run(context.Background(), m, cfg, "ticket")
	if !errors.Is(err, agent.ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
	if res.Steps != 2 {
		t.Errorf("Steps = %d, want 2", res.Steps)
	}
}

func TestRun_DeadlineDuringToolStopsLoop(t *testing.T) {
	slow := funcTool{name: "slow", fn: func(ctx context.Context, _ json.RawMessage) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}}
	m := agenttest.NewScript(
		agenttest.Call("c1", "slow", nil),
		agenttest.Call("c2", "submit", answer{"never reached"}),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	res, err := agent.Run(ctx, m, config(slow), "ticket")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if res.Steps != 1 {
		t.Errorf("Steps = %d, want 1 (no model call after the deadline)", res.Steps)
	}
}

func assertLastResult(t *testing.T, req agent.Request, want string) {
	t.Helper()
	msgs := req.Messages
	results := msgs[len(msgs)-1].ToolResults
	if len(results) != 1 || !results[0].IsError || results[0].Content != want {
		t.Errorf("last tool result = %+v, want error %q", results, want)
	}
}

func toolNames(specs []agent.ToolSpec) string {
	names := make([]string, len(specs))
	for i, s := range specs {
		names[i] = s.Name
	}
	return strings.Join(names, ",")
}

// modelFunc adapts a function to agent.Model.
type modelFunc func(ctx context.Context, req agent.Request) (agent.Response, error)

func (f modelFunc) Generate(ctx context.Context, req agent.Request) (agent.Response, error) {
	return f(ctx, req)
}

// A truncated reply was still billed: its usage must reach the Result.
func TestRun_UsageOfFailedCallIsCounted(t *testing.T) {
	calls := 0
	m := modelFunc(func(context.Context, agent.Request) (agent.Response, error) {
		calls++
		if calls == 1 {
			return agenttest.Call("c1", "echo", map[string]string{"q": "hi"}), nil
		}
		return agent.Response{Usage: agent.Usage{InputTokens: 300, OutputTokens: 1024}}, agent.ErrTruncated
	})
	res, err := agent.Run(context.Background(), m, config(echo), "go")
	if !errors.Is(err, agent.ErrTruncated) {
		t.Fatalf("err = %v, want ErrTruncated", err)
	}
	want := agent.Usage{InputTokens: 400, OutputTokens: 1044} // 100/20 from agenttest.Call
	if res.Usage != want || res.Steps != 2 {
		t.Errorf("usage = %+v, steps = %d; want %+v, 2", res.Usage, res.Steps, want)
	}
}

func TestToolSpec_SchemaDefaultsToEmptyObject(t *testing.T) {
	if got := string((agent.ToolSpec{}).Schema()); got != `{"type":"object","properties":{}}` {
		t.Errorf("Schema() = %s", got)
	}
	if got := string((agent.ToolSpec{InputSchema: json.RawMessage(`{"type":"object"}`)}).Schema()); got != `{"type":"object"}` {
		t.Errorf("Schema() = %s, want the spec's own", got)
	}
}

// An input that cannot fit the budget fails before the first (billed) call.
func TestRun_OversizedInputFailsBeforeFirstCall(t *testing.T) {
	m := agenttest.NewScript(agenttest.Call("c1", "submit", answer{"never"}))
	cfg := config(echo)
	cfg.MaxTokens = 1000

	res, err := agent.Run(context.Background(), m, cfg, strings.Repeat("x", 3*1000))
	if !errors.Is(err, agent.ErrBudgetExceeded) || !agent.IsLimit(err) {
		t.Fatalf("err = %v, want ErrBudgetExceeded (a limit)", err)
	}
	if res.Steps != 0 {
		t.Errorf("Steps = %d, want 0: no call may be made", res.Steps)
	}
}
