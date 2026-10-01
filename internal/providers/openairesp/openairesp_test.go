package openairesp

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
	"time"

	"github.com/Andres568/support-triage-agent/internal/agent"
	"github.com/Andres568/support-triage-agent/internal/providers/httpjson"
)

var (
	update = flag.Bool("update", false, "rewrite testdata/*.request.golden.json from the current code")
	record = flag.Bool("record", false, "TestOllamaLive: record a real exchange with a local Ollama into testdata")
)

var getOrder = agent.ToolSpec{
	Name:        "get_order",
	Description: "Look up an order.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"order_number":{"type":"string"}},"required":["order_number"]}`),
}

var (
	zero      = 0.0
	ollamaOpt = Options{MaxOutputTokens: 2048, ReasoningEffort: "none", Temperature: &zero}

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
				{ID: "call_1", Name: "get_order", Args: json.RawMessage(`{"order_number":"ORD-100103"}`)},
				{ID: "call_2", Name: "search_policy", Args: json.RawMessage(`{"query":"late"}`)},
			}},
			{Role: agent.RoleUser, ToolResults: []agent.ToolResult{
				{CallID: "call_1", Content: `{"status":"shipped"}`},
				{CallID: "call_2", Content: `no policy matches "late"`, IsError: true},
			}},
		},
	}
)

// serve plays a provider: it checks what every request must look like and
// replies with testdata/<response>. It returns the base URL (with /v1, like
// the real ones) and the last request body.
func serve(t *testing.T, key string, status int, response string) (string, *[]byte, *int) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", response))
	if err != nil {
		t.Fatal(err)
	}
	var got []byte
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		got, _ = io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			t.Errorf("request = %s %s, want POST /v1/responses", r.Method, r.URL.Path)
		}
		if auth, want := r.Header.Get("Authorization"), bearer(key); auth != want {
			t.Errorf("Authorization = %q, want %q", auth, want)
		}
		var fields map[string]any
		if err := json.Unmarshal(got, &fields); err != nil {
			t.Errorf("request is not JSON: %v", err)
		}
		if fields["store"] != false {
			t.Errorf("store = %v, want false", fields["store"])
		}
		if _, ok := fields["previous_response_id"]; ok {
			t.Error("request has previous_response_id: the adapter must be stateless")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1", &got, &hits
}

func bearer(key string) string {
	if key == "" {
		return ""
	}
	return "Bearer " + key
}

func TestGenerate(t *testing.T) {
	tests := []struct {
		name     string
		key      string
		model    string
		opts     Options
		req      agent.Request
		golden   string // request golden to compare, if any
		status   int
		response string
		check    func(t *testing.T, resp agent.Response, err error)
	}{
		{
			name: "function_call with string arguments", key: "sk-test", model: "gpt-6.1-sol",
			opts: Options{MaxOutputTokens: 1024}, req: simpleReq, golden: "simple", response: "function_call",
			check: func(t *testing.T, resp agent.Response, err error) {
				calls := resp.Message.ToolCalls
				if err != nil || len(calls) != 1 || calls[0].ID != "call_abc" || calls[0].Name != "get_order" ||
					string(calls[0].Args) != `{"order_number":"ORD-100103"}` {
					t.Fatalf("resp = %+v, err = %v", resp, err)
				}
				if resp.ID != "resp_1" || resp.Model != "gpt-6.1-sol" || resp.StopReason != "completed" ||
					resp.Usage != (agent.Usage{InputTokens: 120, OutputTokens: 15}) {
					t.Errorf("metadata = %+v", resp)
				}
			},
		},
		{
			name: "echoes calls and outputs by call_id, without a key", model: "qwen3:8b", opts: ollamaOpt,
			req: roundTripReq, golden: "round_trip", response: "round_trip",
			check: func(t *testing.T, resp agent.Response, err error) {
				if err != nil || resp.Message.Text != "Your order shipped." || len(resp.Message.ToolCalls) != 0 {
					t.Fatalf("resp = %+v, err = %v", resp, err)
				}
			},
		},
		{
			name: "reasoning items are dropped", key: "k", req: simpleReq, response: "reasoning_dropped",
			check: func(t *testing.T, resp agent.Response, err error) {
				if err != nil || resp.Message.Text != "Checking now." || resp.Usage.ReasoningTokens != 22 {
					t.Fatalf("resp = %+v, err = %v", resp, err)
				}
			},
		},
		{
			name: "incomplete at max_output_tokens is ErrTruncated, with its usage", key: "k", req: simpleReq, response: "incomplete",
			check: func(t *testing.T, resp agent.Response, err error) {
				wantErr(agent.ErrTruncated)(t, resp, err)
				if resp.Usage != (agent.Usage{InputTokens: 50, OutputTokens: 1024}) {
					t.Errorf("usage = %+v, want the billed tokens alongside the error", resp.Usage)
				}
			},
		},
		{
			name: "status failed is an error carrying the body's error", key: "k", req: simpleReq, response: "failed",
			check: func(t *testing.T, resp agent.Response, err error) {
				if err == nil || !strings.Contains(err.Error(), `"failed"`) || !strings.Contains(err.Error(), "server_error: The model failed") {
					t.Fatalf("err = %v, want the status and the error object", err)
				}
				if errors.Is(err, agent.ErrTruncated) || errors.Is(err, agent.ErrRefused) || resp.Usage.InputTokens != 80 {
					t.Errorf("err = %v, usage = %+v", err, resp.Usage)
				}
			},
		},
		{
			name: "context_length_exceeded is ErrContextWindow, HTTPError kept", key: "k", req: simpleReq,
			status: http.StatusBadRequest, response: "context_length",
			check: func(t *testing.T, _ agent.Response, err error) {
				var herr *httpjson.HTTPError
				if !errors.Is(err, agent.ErrContextWindow) || !errors.As(err, &herr) || herr.Code != "context_length_exceeded" {
					t.Fatalf("err = %v, want ErrContextWindow wrapping the HTTPError", err)
				}
			},
		},
		{
			name: "incomplete by content_filter is ErrRefused", key: "k", req: simpleReq, response: "content_filter",
			check: wantErr(agent.ErrRefused),
		},
		{
			name: "refusal content is ErrRefused", key: "k", req: simpleReq, response: "refusal",
			check: wantErr(agent.ErrRefused),
		},
		{
			name: "cached tokens are subtracted from input", key: "k", req: simpleReq, response: "cached",
			check: func(t *testing.T, resp agent.Response, err error) {
				want := agent.Usage{InputTokens: 500, CacheReadTokens: 1500, OutputTokens: 100, ReasoningTokens: 40}
				if err != nil || resp.Usage != want || resp.Usage.Total() != 2100 {
					t.Fatalf("usage = %+v, err = %v; want %+v", resp.Usage, err, want)
				}
			},
		},
		{
			name: "context-window guard catches silent truncation", req: simpleReq, response: "cached",
			opts:  Options{ContextWindow: 2200}, // 2000 input tokens >= 90% of 2200
			check: wantErr(agent.ErrContextWindow),
		},
		{
			name: "invalid arguments JSON is kept as a JSON string", key: "k", req: simpleReq, response: "invalid_arguments",
			check: func(t *testing.T, resp agent.Response, err error) {
				if err != nil || len(resp.Message.ToolCalls) != 1 {
					t.Fatalf("resp = %+v, err = %v", resp, err)
				}
				var s string
				if err := json.Unmarshal(resp.Message.ToolCalls[0].Args, &s); err != nil || s != "{order_number: ORD-100103" {
					t.Errorf("args = %s, want the raw text as a JSON string", resp.Message.ToolCalls[0].Args)
				}
			},
		},
		{
			name: "error body becomes an HTTPError and 400 is not retried", key: "k", req: simpleReq,
			status: http.StatusBadRequest, response: "error",
			check: func(t *testing.T, _ agent.Response, err error) {
				var herr *httpjson.HTTPError
				if !errors.As(err, &herr) || herr.Status != 400 || herr.Type != "invalid_request_error" ||
					!strings.Contains(herr.Message, "Invalid schema") || herr.Retryable() {
					t.Fatalf("err = %v, want a non-retryable HTTPError with the parsed body", err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := tt.status
			if status == 0 {
				status = http.StatusOK
			}
			base, sent, hits := serve(t, tt.key, status, tt.response+".response.json")
			model := tt.model
			if model == "" {
				model = "gpt-6.1-sol"
			}
			resp, err := New(base, tt.key, model, tt.opts, http.DefaultClient).Generate(context.Background(), tt.req)
			if *hits != 1 {
				t.Errorf("requests = %d, want 1", *hits)
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

// ollamaReq asks for a tool call that a small local model reliably makes.
var ollamaReq = agent.Request{
	System:   "You are a support agent for an online shop. Always look up an order with get_order before answering about it.",
	Messages: []agent.Message{{Role: agent.RoleUser, Text: "Hi, where is my order ORD-100103? It has not arrived."}},
	Tools:    []agent.ToolSpec{getOrder},
}

const ollamaFixture = "ollama_qwen3_8b"

// TestOllamaLive records one real exchange with a local Ollama. It is the
// only fixture not written by hand, so it catches where Ollama's Responses
// API differs from the docs. Opt-in: it needs Ollama with qwen3:8b pulled.
func TestOllamaLive(t *testing.T) {
	if !*record {
		t.Skip("run with -record against a local Ollama to re-record " + ollamaFixture)
	}
	base := os.Getenv("OLLAMA_BASE_URL")
	if base == "" {
		base = "http://127.0.0.1:11434/v1"
	}
	rec := &recorder{next: http.DefaultTransport}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	resp, err := New(base, "", "qwen3:8b", ollamaOpt, &http.Client{Transport: rec}).Generate(ctx, ollamaReq)
	if err != nil {
		t.Fatal(err)
	}
	checkOllamaResponse(t, resp)
	for suffix, raw := range map[string][]byte{".request.golden.json": rec.req, ".response.json": rec.resp} {
		if err := os.WriteFile(filepath.Join("testdata", ollamaFixture+suffix), pretty(t, raw), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestOllamaRecorded replays the recorded exchange offline: our request must
// still be the one Ollama accepted, and its real reply must still parse.
func TestOllamaRecorded(t *testing.T) {
	base, sent, _ := serve(t, "", http.StatusOK, ollamaFixture+".response.json")
	resp, err := New(base, "", "qwen3:8b", ollamaOpt, http.DefaultClient).Generate(context.Background(), ollamaReq)
	if err != nil {
		t.Fatal(err)
	}
	checkOllamaResponse(t, resp)
	compareGolden(t, filepath.Join("testdata", ollamaFixture+".request.golden.json"), *sent)
}

func checkOllamaResponse(t *testing.T, resp agent.Response) {
	t.Helper()
	calls := resp.Message.ToolCalls
	if len(calls) != 1 || calls[0].Name != "get_order" || calls[0].ID == "" {
		t.Fatalf("resp = %+v, want one get_order call with a call id", resp)
	}
	var args struct {
		OrderNumber string `json:"order_number"`
	}
	if err := json.Unmarshal(calls[0].Args, &args); err != nil || args.OrderNumber != "ORD-100103" {
		t.Errorf("args = %s, want order_number ORD-100103", calls[0].Args)
	}
	if resp.Usage.InputTokens == 0 || resp.Usage.OutputTokens == 0 {
		t.Errorf("usage = %+v, want tokens counted", resp.Usage)
	}
}

// recorder keeps the last request and response bodies that went over the wire.
type recorder struct {
	next      http.RoundTripper
	req, resp []byte
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	var err error
	if r.req, err = io.ReadAll(req.Body); err != nil {
		return nil, err
	}
	req.Body = io.NopCloser(bytes.NewReader(r.req))
	resp, err := r.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if r.resp, err = io.ReadAll(resp.Body); err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(r.resp))
	return resp, nil
}

// A missing status is completed only when there is output to trust, and a
// provider's error message is bounded before it reaches logs and the database.
func TestParse_MissingStatusAndLongError(t *testing.T) {
	parse := func(body string) error {
		var r response
		if err := json.Unmarshal([]byte(body), &r); err != nil {
			t.Fatal(err)
		}
		_, err := (&Model{}).parse(r)
		return err
	}
	if err := parse(`{"output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}]}`); err != nil {
		t.Errorf("no status with output: err = %v, want completed", err)
	}
	if err := parse(`{"output":[]}`); err == nil {
		t.Error("no status and no output: want an error")
	}
	long := strings.Repeat("x", 5000)
	err := parse(`{"status":"failed","error":{"code":"server_error","message":"` + long + `"}}`)
	if err == nil || len(err.Error()) > 1000 {
		t.Errorf("failed with a long message: err = %.80v, want a bounded error", err)
	}
}
