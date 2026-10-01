package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Andres568/support-triage-agent/internal/agent"
	"github.com/Andres568/support-triage-agent/internal/providers/httpjson"
)

var update = flag.Bool("update", false, "rewrite testdata/*.request.golden.json from the current code")

const testKey = "sk-ant-test-secret"

var getOrder = agent.ToolSpec{
	Name:        "get_order",
	Description: "Look up an order.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"order_number":{"type":"string"}},"required":["order_number"]}`),
}

var (
	simpleReq = agent.Request{
		System:   "You are a support agent.",
		Messages: []agent.Message{{Role: agent.RoleUser, Text: "Where is order ORD-100103?"}},
		Tools:    []agent.ToolSpec{getOrder},
	}
	roundTripReq = agent.Request{
		System: simpleReq.System,
		Tools:  simpleReq.Tools,
		Messages: []agent.Message{
			simpleReq.Messages[0],
			{Role: agent.RoleAssistant, Text: "Let me check.", ToolCalls: []agent.ToolCall{
				{ID: "toolu_01A", Name: "get_order", Args: json.RawMessage(`{"order_number":"ORD-100103"}`)},
				// a malformed call kept as a JSON string: echoed as {}
				{ID: "toolu_01B", Name: "search_policy", Args: json.RawMessage(`"{query: late"`)},
			}},
			{Role: agent.RoleUser, Text: "Anything else?", ToolResults: []agent.ToolResult{
				{CallID: "toolu_01A", Content: `{"status":"shipped"}`},
				{CallID: "toolu_01B", Content: "invalid arguments", IsError: true},
			}},
			// a tool_use turn without text sends no empty text block
			{Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{
				{ID: "toolu_01C", Name: "get_order", Args: json.RawMessage(`{"order_number":"ORD-100104"}`)},
			}},
			{Role: agent.RoleUser, ToolResults: []agent.ToolResult{{CallID: "toolu_01C", Content: "not found"}}},
		},
	}
)

// emptyTurnReq is what the loop sends after a reply with neither text nor
// calls: that empty assistant turn, then the nudge.
var emptyTurnReq = agent.Request{
	System: simpleReq.System,
	Tools:  simpleReq.Tools,
	Messages: []agent.Message{
		simpleReq.Messages[0],
		{Role: agent.RoleAssistant},
		{Role: agent.RoleUser, Text: agent.Nudge},
	},
}

type reply struct {
	status     int
	file       string
	retryAfter string
}

// serve plays the Messages API: it checks what every request must look like
// and answers with the replies in order. It returns the base URL, the last
// request body and the request count.
func serve(t *testing.T, replies ...reply) (string, *[]byte, *int) {
	t.Helper()
	var got []byte
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		got, _ = io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
			t.Errorf("request = %s %s, want POST /v1/messages", r.Method, r.URL.Path)
		}
		for h, want := range map[string]string{"X-Api-Key": testKey, "Anthropic-Version": "2023-06-01", "Content-Type": "application/json"} {
			if v := r.Header.Get(h); v != want {
				t.Errorf("%s = %q, want %q", h, v, want)
			}
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("Authorization header sent; Anthropic uses x-api-key")
		}
		var fields map[string]any
		if err := json.Unmarshal(got, &fields); err != nil {
			t.Errorf("request is not JSON: %v", err)
		}
		for _, k := range []string{"temperature", "tool_choice"} {
			if _, ok := fields[k]; ok {
				t.Errorf("request has %q: newer models reject it", k)
			}
		}
		if hits > len(replies) {
			t.Errorf("unexpected request %d", hits)
			w.WriteHeader(http.StatusTeapot)
			return
		}
		rep := replies[hits-1]
		body, err := os.ReadFile(filepath.Join("testdata", rep.file+".response.json"))
		if err != nil {
			t.Error(err)
		}
		if rep.retryAfter != "" {
			w.Header().Set("Retry-After", rep.retryAfter)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rep.status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &got, &hits
}

func ok(file string) reply { return reply{status: http.StatusOK, file: file} }

func TestGenerate(t *testing.T) {
	tests := []struct {
		name    string
		req     agent.Request
		golden  string // request golden to compare, if any
		replies []reply
		check   func(t *testing.T, resp agent.Response, err error)
	}{
		{
			name: "text blocks are concatenated, thinking dropped", req: simpleReq, golden: "simple", replies: []reply{ok("text")},
			check: func(t *testing.T, resp agent.Response, err error) {
				if err != nil || resp.Message.Text != "I will look into your order." || len(resp.Message.ToolCalls) != 0 {
					t.Fatalf("resp = %+v, err = %v", resp, err)
				}
				if resp.ID != "msg_01text" || resp.Model != "claude-sonnet-5-5" || resp.StopReason != "end_turn" ||
					resp.Usage != (agent.Usage{InputTokens: 120, OutputTokens: 15}) || resp.Message.Role != agent.RoleAssistant {
					t.Errorf("metadata = %+v", resp)
				}
			},
		},
		{
			name: "two parallel tool_use blocks", req: simpleReq, replies: []reply{ok("tool_use")},
			check: func(t *testing.T, resp agent.Response, err error) {
				want := []agent.ToolCall{
					{ID: "toolu_01A", Name: "get_order", Args: json.RawMessage(`{"order_number": "ORD-100103"}`)},
					{ID: "toolu_01B", Name: "search_policy", Args: json.RawMessage(`{"query": "late delivery"}`)},
				}
				if err != nil || resp.Message.Text != "Let me check." || !reflect.DeepEqual(resp.Message.ToolCalls, want) {
					t.Fatalf("resp = %+v, err = %v", resp, err)
				}
			},
		},
		{
			name: "round trip: tool_use echo, then tool_result first with is_error", req: roundTripReq, golden: "round_trip",
			replies: []reply{ok("round_trip")},
			check: func(t *testing.T, resp agent.Response, err error) {
				if err != nil || resp.Message.Text != "Your order shipped." {
					t.Fatalf("resp = %+v, err = %v", resp, err)
				}
			},
		},
		{
			name: "empty assistant turn is dropped and the user turns merged", req: emptyTurnReq, golden: "empty_turn",
			replies: []reply{ok("text")},
			check: func(t *testing.T, _ agent.Response, err error) {
				if err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "cache usage maps 1:1", req: simpleReq, replies: []reply{ok("cached")},
			check: func(t *testing.T, resp agent.Response, err error) {
				want := agent.Usage{InputTokens: 50, OutputTokens: 100, CacheWriteTokens: 400, CacheReadTokens: 1500}
				if err != nil || resp.Usage != want || resp.Usage.Total() != 2050 {
					t.Fatalf("usage = %+v, err = %v; want %+v", resp.Usage, err, want)
				}
			},
		},
		{
			name: "max_tokens is ErrTruncated, with its usage", req: simpleReq, replies: []reply{ok("max_tokens")},
			check: func(t *testing.T, resp agent.Response, err error) {
				wantErr(agent.ErrTruncated)(t, resp, err)
				if resp.Usage.OutputTokens == 0 {
					t.Errorf("usage = %+v, want the billed tokens alongside the error", resp.Usage)
				}
			},
		},
		{name: "refusal is ErrRefused", req: simpleReq, replies: []reply{ok("refusal")}, check: wantErr(agent.ErrRefused)},
		{name: "context window exceeded", req: simpleReq, replies: []reply{ok("context_window")}, check: wantErr(agent.ErrContextWindow)},
		{
			name: "pause_turn is an error", req: simpleReq, replies: []reply{ok("pause_turn")},
			check: func(t *testing.T, _ agent.Response, err error) {
				if err == nil || !strings.Contains(err.Error(), "pause_turn") {
					t.Fatalf("err = %v, want unexpected stop_reason", err)
				}
			},
		},
		{
			name: "429 with retry-after is retried", req: simpleReq,
			replies: []reply{{status: http.StatusTooManyRequests, file: "rate_limit", retryAfter: "0.01"}, ok("text")},
			check: func(t *testing.T, resp agent.Response, err error) {
				if err != nil || resp.ID != "msg_01text" {
					t.Fatalf("resp = %+v, err = %v", resp, err)
				}
			},
		},
		{
			name: "400 becomes an HTTPError, not retried, without the key", req: simpleReq,
			replies: []reply{{status: http.StatusBadRequest, file: "error"}},
			check: func(t *testing.T, _ agent.Response, err error) {
				var herr *httpjson.HTTPError
				if !errors.As(err, &herr) || herr.Status != 400 || herr.Type != "invalid_request_error" ||
					!strings.Contains(herr.Message, "tool_use_id") || herr.Retryable() {
					t.Fatalf("err = %v, want a non-retryable HTTPError with the parsed body", err)
				}
				if strings.Contains(err.Error(), testKey) {
					t.Error("error leaks the API key")
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base, sent, hits := serve(t, tt.replies...)
			resp, err := New(base, testKey, "claude-sonnet-5-5", 1024, http.DefaultClient).Generate(context.Background(), tt.req)
			if *hits != len(tt.replies) {
				t.Errorf("requests = %d, want %d", *hits, len(tt.replies))
			}
			tt.check(t, resp, err)
			if tt.golden != "" {
				compareGolden(t, filepath.Join("testdata", tt.golden+".request.golden.json"), *sent)
			}
		})
	}
}

func wantErr(target error) func(*testing.T, agent.Response, error) {
	return func(t *testing.T, _ agent.Response, err error) {
		t.Helper()
		if !errors.Is(err, target) {
			t.Fatalf("err = %v, want %v", err, target)
		}
	}
}

// compareGolden compares JSON canonically: key order and whitespace do not
// matter, values do.
func compareGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.WriteFile(path, pretty(t, got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("request differs from %s (run with -update to accept)\n got: %s\nwant: %s", path, pretty(t, got), pretty(t, want))
	}
}

func pretty(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		t.Fatal(err)
	}
	return append(buf.Bytes(), '\n')
}

// The loop nudges after a reply with neither text nor calls. Sending that
// empty assistant turn would be a 400; dropping it leaves two user turns,
// which are merged with the tool results still first.
func TestRequest_EmptyAssistantTurnDroppedAndUserTurnsMerged(t *testing.T) {
	req := agent.Request{
		Tools: []agent.ToolSpec{{Name: "submit"}}, // no schema: an empty object is sent
		Messages: []agent.Message{
			{Role: agent.RoleUser, Text: "ticket"},
			{Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{{ID: "t1", Name: "get_order", Args: json.RawMessage(`{}`)}}},
			{Role: agent.RoleUser, ToolResults: []agent.ToolResult{{CallID: "t1", Content: "ok"}}},
			{Role: agent.RoleAssistant},
			{Role: agent.RoleUser, Text: agent.Nudge},
		},
	}
	r := New("http://x", "k", "m", 10, nil).request(req)
	if len(r.Messages) != 3 {
		t.Fatalf("messages = %+v, want 3 alternating turns", r.Messages)
	}
	for i, m := range r.Messages {
		if len(m.Content) == 0 || (i > 0 && m.Role == r.Messages[i-1].Role) {
			t.Errorf("message %d = %+v: empty or same role as the previous one", i, m)
		}
	}
	last := r.Messages[2].Content
	if len(last) != 2 || last[0].Type != "tool_result" || last[1].Type != "text" || last[1].Text != agent.Nudge {
		t.Errorf("merged turn = %+v, want [tool_result, nudge text]", last)
	}
	if string(r.Tools[0].InputSchema) != `{"type":"object","properties":{}}` {
		t.Errorf("schema = %s", r.Tools[0].InputSchema)
	}
}
