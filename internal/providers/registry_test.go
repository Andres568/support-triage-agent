package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/Andres568/support-triage-agent/internal/agent"
	"github.com/Andres568/support-triage-agent/internal/triage"
)

func env(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func TestModels_AreComplete(t *testing.T) {
	seen := map[string]bool{}
	for _, m := range Models {
		if seen[m.ID] {
			t.Errorf("%s: duplicate id", m.ID)
		}
		seen[m.ID] = true
		if !slices.Contains(Providers, m.Provider) {
			t.Errorf("%s: unknown provider %q", m.ID, m.Provider)
		}
		local := m.Provider == Ollama || m.Provider == Fake
		if local != (m.KeyEnv == "") {
			t.Errorf("%s: KeyEnv %q; cloud models need one, local ones none", m.ID, m.KeyEnv)
		}
		if local != (m.Price == Price{}) {
			t.Errorf("%s: price %+v; cloud models are priced, local ones free", m.ID, m.Price)
		}
		if !local && (m.PricesVerified == "" || m.BaseURL == "" || m.APIModel == "" || m.MaxOutputTokens == 0) {
			t.Errorf("%s: incomplete spec %+v", m.ID, m)
		}
		if m.Provider == Ollama && (m.ContextWindow == 0 || m.ReasoningEffort != "none") {
			t.Errorf("%s: local models need the context guard and no reasoning", m.ID)
		}
	}
	for id, base := range map[string]string{"grok-4.7": "https://api.x.ai/v1", "gpt-6.1-sol": "https://api.openai.com/v1", "claude-sonnet-5-5": "https://api.anthropic.com"} {
		if m, err := Lookup(id); err != nil || m.BaseURL != base {
			t.Errorf("Lookup(%s) = %+v, %v; want base %s", id, m, err, base)
		}
	}
}

func TestCost(t *testing.T) {
	tests := []struct {
		id   string
		u    agent.Usage
		want int64 // micro-USD
	}{
		{"claude-opus-5-5", agent.Usage{InputTokens: 1_000_000}, 4_000_000},
		// 1000*2 + 500*10 + 2000*0.2 + 400*2.5 USD/MTok = 8400 micro-USD
		{"claude-sonnet-5-5", agent.Usage{InputTokens: 1000, OutputTokens: 500, CacheReadTokens: 2000, CacheWriteTokens: 400}, 8400},
		// 1234*0.10 + 56*0.50 = 151.4 micro-USD, rounded down once
		{"gpt-6-luna", agent.Usage{InputTokens: 1234, OutputTokens: 56}, 151},
		// unverified cache price: charged as full input
		{"grok-4.3", agent.Usage{CacheReadTokens: 1000}, 1250},
		{"ollama/qwen3:8b", agent.Usage{InputTokens: 50_000, OutputTokens: 9000}, 0},
		{"claude-haiku-4-5-20251001", agent.Usage{ReasoningTokens: 1_000_000}, 0}, // reasoning is inside output
	}
	for _, tt := range tests {
		m, err := Lookup(tt.id)
		if err != nil {
			t.Fatal(err)
		}
		if got := m.Price.Cost(tt.u); got != tt.want {
			t.Errorf("%s: Cost(%+v) = %d, want %d", tt.id, tt.u, got, tt.want)
		}
	}
}

func TestNew(t *testing.T) {
	if _, err := Lookup("gpt-5"); !errors.Is(err, ErrUnknownModel) {
		t.Errorf("Lookup(gpt-5) err = %v, want ErrUnknownModel", err)
	}
	for _, id := range []string{"claude-sonnet-5-5", "gpt-6.1-sol", "grok-4.7"} {
		m, _ := Lookup(id)
		if _, err := New(m, env(nil), http.DefaultClient); !errors.Is(err, ErrMissingKey) || !strings.Contains(err.Error(), m.KeyEnv) {
			t.Errorf("New(%s) without a key: err = %v, want ErrMissingKey naming %s", id, err, m.KeyEnv)
		}
	}
	m, _ := Lookup("ollama/qwen3:8b")
	if _, err := New(m, env(nil), http.DefaultClient); err != nil {
		t.Errorf("ollama needs no key: %v", err)
	}
}

// A cloud model posts to its base URL with its key; Ollama's base URL comes
// from the environment when set.
func TestNew_RoutesToBaseURLWithKey(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Path+" "+r.Header.Get("Authorization")+r.Header.Get("X-Api-Key"))
		if strings.HasSuffix(r.URL.Path, "/messages") {
			_, _ = w.Write([]byte(`{"stop_reason":"end_turn","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	grok, _ := Lookup("grok-4.7")
	grok.BaseURL = srv.URL + "/v1"
	claude, _ := Lookup("claude-sonnet-5-5")
	claude.BaseURL = srv.URL
	local, _ := Lookup("ollama/qwen3:8b")
	getenv := env(map[string]string{"XAI_API_KEY": "xai-k", "ANTHROPIC_API_KEY": "ant-k", OllamaBaseURLEnv: srv.URL + "/ollama/v1"})
	for _, spec := range []ModelSpec{grok, claude, local} {
		m, err := New(spec, getenv, http.DefaultClient)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.Generate(context.Background(), agent.Request{}); err != nil {
			t.Fatal(err)
		}
	}
	if want := []string{"/v1/responses Bearer xai-k", "/v1/messages ant-k", "/ollama/v1/responses "}; !slices.Equal(got, want) {
		t.Errorf("requests = %q, want %q", got, want)
	}
}

func TestEscalateAll_SubmitsAValidEscalation(t *testing.T) {
	spec, err := Lookup("fake/escalate-all")
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(spec, env(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := m.Generate(context.Background(), agent.Request{})
	if err != nil {
		t.Fatal(err)
	}
	calls := resp.Message.ToolCalls
	if len(calls) != 1 || calls[0].Name != triage.SubmitDecisionTool {
		t.Fatalf("calls = %+v, want one submit_decision", calls)
	}
	if d, err := triage.ParseDecision(calls[0].Args); err != nil || d.Action != triage.ActionEscalate {
		t.Fatalf("decision = %+v, %v; want a valid escalation", d, err)
	}
}
