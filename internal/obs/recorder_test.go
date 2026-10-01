package obs_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/Andres568/support-triage-agent/internal/agent"
	"github.com/Andres568/support-triage-agent/internal/agent/agenttest"
	"github.com/Andres568/support-triage-agent/internal/obs"
	"github.com/Andres568/support-triage-agent/internal/providers"
	"github.com/Andres568/support-triage-agent/internal/queue"
)

// Text that must never reach a span: prompt, customer input, tool output.
const (
	secretSystem = "SYSTEM-PROMPT-TEXT"
	secretInput  = "jane@example.com says my mug arrived broken"
	secretTool   = "order ORD-100105 shipped to 12 Secret Street"
)

type orderTool struct{}

func (orderTool) Spec() agent.ToolSpec { return agent.ToolSpec{Name: "get_order"} }
func (orderTool) Call(context.Context, json.RawMessage) (string, error) {
	return secretTool, nil
}

func TestItemRecorder_Spans(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	f := &obs.Factory{Tracer: tp, Spec: providers.ModelSpec{ID: "claude-x", Provider: providers.Anthropic, APIModel: "claude-x-api"}}

	call := agenttest.Call("c1", "get_order", map[string]string{"order_number": "ORD-100105"})
	call.ID, call.Model, call.StopReason = "msg_1", "claude-x-api-2026", "tool_use"
	call.Usage.CacheReadTokens = 50
	script := agenttest.NewScript(call, agenttest.Call("c2", "done", map[string]string{"text": secretInput}))

	ctx, ir := f.Item(context.Background(), queue.RunID{}, obs.KindTicket, 7, 2, "HD-2005")
	cfg := agent.Config{System: secretSystem, Tools: ir.Tools([]agent.Tool{orderTool{}}), MaxSteps: 4,
		Finish: agent.ToolSpec{Name: "done"}, Validate: func(json.RawMessage) error { return nil }}
	res, err := agent.Run(ctx, ir.Model(script), cfg, secretInput)
	if err != nil {
		t.Fatal(err)
	}
	ir.Loop(res)
	ir.Decided("drafted", "agent")
	ir.Finish(ctx, obs.OutcomeFinalized, nil)

	spans := sr.Ended()
	var names []string
	for _, s := range spans {
		names = append(names, fmt.Sprintf("%s/%s", s.Name(), s.SpanKind()))
		for _, kv := range s.Attributes() {
			for _, secret := range []string{secretSystem, secretInput, secretTool, "jane@", "Secret Street"} {
				if strings.Contains(kv.Value.String(), secret) {
					t.Errorf("span %q attribute %s leaks %q", s.Name(), kv.Key, secret)
				}
			}
		}
	}
	want := "chat claude-x-api/client,execute_tool get_order/internal,chat claude-x-api/client,process ticket/internal"
	if got := strings.Join(names, ","); got != want {
		t.Fatalf("spans = %s\nwant    %s", got, want)
	}
	process := spans[3]
	for _, s := range spans[:3] {
		if s.Parent().SpanID() != process.SpanContext().SpanID() {
			t.Errorf("%s is not a child of the process span", s.Name())
		}
	}

	wantAttrs := map[int][]attribute.KeyValue{
		0: {obs.GenAIOperationName.String("chat"), obs.GenAIProviderName.String("anthropic"),
			obs.GenAIRequestModel.String("claude-x-api"), obs.GenAIResponseModel.String("claude-x-api-2026"),
			obs.GenAIResponseID.String("msg_1"), obs.GenAIFinishReasons.StringSlice([]string{"tool_use"}),
			obs.GenAIUsageInput.Int(150), obs.GenAIUsageOutput.Int(20)},
		1: {obs.GenAIOperationName.String("execute_tool"), obs.GenAIToolName.String("get_order"),
			obs.GenAIToolCallID.String("c1")},
		3: {obs.AppSubject.String("HD-2005"), obs.AppAttempt.Int(2), obs.AppOutcome.String("drafted")},
	}
	for i, kvs := range wantAttrs {
		got := attribute.NewSet(spans[i].Attributes()...)
		for _, kv := range kvs {
			if v, ok := got.Value(kv.Key); !ok || v != kv.Value {
				t.Errorf("%s: %s = %v, want %v", spans[i].Name(), kv.Key, v.String(), kv.Value.String())
			}
		}
	}
}

type billed struct {
	usage agent.Usage
	err   error
}

func (b billed) Generate(context.Context, agent.Request) (agent.Response, error) {
	return agent.Response{Usage: b.usage, StopReason: "length"}, b.err
}

// Cost is rounded once per item, from the summed usage; each step keeps its
// own (rounded) cost. A failed call is billed and marked on its span.
func TestItemRecorder_CostAndErrors(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	f := &obs.Factory{Tracer: sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)),
		Spec: providers.ModelSpec{ID: "m", Price: providers.Price{InputMicrosPerMTok: 1_500_000}}}
	_, ir := f.Item(context.Background(), queue.RunID{}, obs.KindTask, 1, 1, "HD-1/refund_review")

	m := ir.Model(billed{usage: agent.Usage{InputTokens: 1}})
	failing := ir.Model(billed{usage: agent.Usage{InputTokens: 1}, err: fmt.Errorf("call: %w", agent.ErrTruncated)})
	if _, err := m.Generate(context.Background(), agent.Request{}); err != nil {
		t.Fatal(err)
	}
	if _, err := failing.Generate(context.Background(), agent.Request{}); !errors.Is(err, agent.ErrTruncated) {
		t.Fatalf("err = %v", err)
	}

	u, cost := ir.Usage()
	if u.InputTokens != 2 || cost != 3 {
		t.Errorf("usage %+v cost %d; want 2 input tokens and 3 micros (1.5 per token, rounded once)", u, cost)
	}
	for i, s := range ir.Steps() {
		if s.CostMicros != 1 || (s.Err != nil) != (i == 1) {
			t.Errorf("step %d = %+v", i, s)
		}
	}
	ended := sr.Ended()
	if len(ended) != 2 {
		t.Fatalf("%d spans", len(ended))
	}
	errSet := attribute.NewSet(ended[1].Attributes()...)
	if v, _ := errSet.Value(obs.ErrorType); v.AsString() != "truncated" ||
		ended[1].Status().Description != "truncated" {
		t.Errorf("error.type = %v, status %+v", errSet, ended[1].Status())
	}
}

// A nil factory records nothing and changes nothing.
func TestNilFactory(t *testing.T) {
	var f *obs.Factory
	ctx, ir := f.Item(context.Background(), queue.RunID{}, obs.KindTicket, 1, 1, "x")
	m := billed{}
	if ir != nil || trace.SpanFromContext(ctx).SpanContext().IsValid() || ir.Model(m) != agent.Model(m) {
		t.Error("nil factory recorded")
	}
	ir.Loop(agent.Result{})
	ir.Finish(ctx, "drafted", nil)
	if ir.Steps() != nil {
		t.Error("nil recorder has steps")
	}
	if u, c := ir.Usage(); u != (agent.Usage{}) || c != 0 {
		t.Error("nil recorder has usage")
	}
}

type blockingDB struct{}

func (blockingDB) Begin(ctx context.Context) (pgx.Tx, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// A hung database cannot hold the worker past the write timeout; a second
// Finish does nothing.
func TestFinish_BoundedAndOnce(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	f := &obs.Factory{DB: blockingDB{}, WriteTimeout: 50 * time.Millisecond, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Tracer: sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))}
	ctx, ir := f.Item(context.Background(), queue.RunID{}, obs.KindTicket, 1, 1, "HD-1")
	start := time.Now()
	ir.Finish(ctx, obs.OutcomeAbandoned, nil)
	if d := time.Since(start); d > time.Second {
		t.Errorf("Finish took %v with a 50ms write timeout", d)
	}
	ir.Finish(ctx, obs.OutcomeLostLease, nil)
	if n := len(sr.Ended()); n != 1 {
		t.Errorf("%d spans ended, want the item's once", n)
	}
}

// Two identical calls in one turn are told apart by their call ids.
func TestItemRecorder_IdenticalCallsGetDistinctIDs(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	f := &obs.Factory{Tracer: sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))}
	args := map[string]string{"order_number": "ORD-100105"}
	turn := agenttest.Call("c1", "get_order", args)
	turn.Message.ToolCalls = append(turn.Message.ToolCalls, agenttest.Call("c2", "get_order", args).Message.ToolCalls...)
	script := agenttest.NewScript(turn, agenttest.Call("c3", "done", map[string]string{}))

	ctx, ir := f.Item(context.Background(), queue.RunID{}, obs.KindTicket, 1, 1, "HD-1")
	cfg := agent.Config{Tools: ir.Tools([]agent.Tool{orderTool{}}), MaxSteps: 4,
		Finish: agent.ToolSpec{Name: "done"}, Validate: func(json.RawMessage) error { return nil }}
	if _, err := agent.Run(ctx, ir.Model(script), cfg, "hi"); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range sr.Ended() {
		set := attribute.NewSet(s.Attributes()...)
		if v, ok := set.Value(obs.GenAIToolCallID); ok {
			ids = append(ids, v.AsString())
		}
	}
	if got := strings.Join(ids, ","); got != "c1,c2" {
		t.Errorf("tool call ids = %s, want c1,c2", got)
	}
}

type leakyTool struct{}

func (leakyTool) Spec() agent.ToolSpec { return agent.ToolSpec{Name: "get_order"} }
func (leakyTool) Call(context.Context, json.RawMessage) (string, error) {
	return "", errors.New(secretTool)
}

// Error messages quote customer data: no span attribute, event or status
// description may carry them.
func TestItemRecorder_ErrorsLeakNoPII(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	f := &obs.Factory{Tracer: sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))}
	ctx, ir := f.Item(context.Background(), queue.RunID{}, obs.KindTicket, 1, 1, "HD-1")
	if _, err := ir.Tools([]agent.Tool{leakyTool{}})[0].Call(ctx, json.RawMessage(`{}`)); err == nil {
		t.Fatal("want the tool error")
	}
	_, _ = ir.Model(billed{err: errors.New(secretInput)}).Generate(ctx, agent.Request{})
	ir.Finish(ctx, obs.OutcomeAbandoned, fmt.Errorf("handler panic: %s", secretInput))

	ended := sr.Ended()
	if len(ended) != 3 {
		t.Fatalf("%d spans, want 3", len(ended))
	}
	for _, s := range ended {
		texts := []string{s.Status().Description}
		for _, kv := range s.Attributes() {
			texts = append(texts, kv.Value.String())
		}
		for _, e := range s.Events() {
			texts = append(texts, e.Name)
			for _, kv := range e.Attributes {
				texts = append(texts, kv.Value.String())
			}
		}
		for _, text := range texts {
			if strings.Contains(text, "Secret Street") || strings.Contains(text, "jane@") {
				t.Errorf("span %q leaks %q", s.Name(), text)
			}
		}
		if s.Status().Description != "_OTHER" {
			t.Errorf("span %q status = %+v, want an _OTHER error", s.Name(), s.Status())
		}
	}
}
