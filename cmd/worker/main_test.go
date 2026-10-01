package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Andres568/support-triage-agent/internal/providers"
	"github.com/Andres568/support-triage-agent/internal/triage"
)

func env(kv map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := kv[k]; return v, ok }
}

func TestLoadConfig_DefaultsAreValidAndSafe(t *testing.T) {
	c, err := loadConfig("support", env(map[string]string{"DATABASE_URL": "postgres://x"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Policy.Autonomy != triage.AutonomyShadow || len(c.Policy.AutoReply) != 0 {
		t.Errorf("default policy = %+v, want shadow with no auto-reply", c.Policy)
	}
	if c.Worker.Batch != 20 || c.Worker.Concurrency != 1 || c.Worker.Lease != 240*time.Second || c.MaxSteps != 8 {
		t.Errorf("defaults = %+v", c)
	}
}

func TestLoadConfig_ReportsEveryProblem(t *testing.T) {
	_, err := loadConfig("billing", env(map[string]string{
		"BATCH_SIZE": "many", "AUTONOMY": "yolo", "AUTO_REPLY_CATEGORIES": "general,refunds", "LEASE": "10s",
	}))
	if err == nil {
		t.Fatal("loadConfig accepted a broken environment")
	}
	for _, want := range []string{"DATABASE_URL", "BATCH_SIZE", "AUTONOMY", `"refunds"`, "lease", `agent "billing"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s:\n%v", want, err)
		}
	}
}

// An empty AUTO_REPLY_CATEGORIES is an explicit "nothing", not the default.
func TestLoadConfig_EmptyAllowlistIsEmpty(t *testing.T) {
	base := map[string]string{"DATABASE_URL": "postgres://x", "AUTONOMY": "auto"}
	c, err := loadConfig("support", env(base))
	if err != nil || len(c.Policy.AutoReply) != 2 {
		t.Fatalf("unset: policy = %+v, err = %v; want the two default categories", c.Policy, err)
	}
	base["AUTO_REPLY_CATEGORIES"] = ""
	c, err = loadConfig("support", env(base))
	if err != nil || len(c.Policy.AutoReply) != 0 {
		t.Fatalf("empty: policy = %+v, err = %v; want no auto-reply category", c.Policy, err)
	}
}

func TestLoadConfig_MaxTokensMustBePositive(t *testing.T) {
	if _, err := loadConfig("support", env(map[string]string{"DATABASE_URL": "postgres://x", "MAX_TOKENS": "0"})); err == nil {
		t.Fatal("MAX_TOKENS=0 accepted")
	}
}

func TestNewModel(t *testing.T) {
	getenv := func(string) string { return "" }
	for _, id := range []string{"fake/escalate-all", "ollama/qwen3:8b"} {
		if modelFor, err := newModel(id, getenv); err != nil || modelFor("HD-2001", 1) == nil {
			t.Errorf("newModel(%s): %v", id, err)
		}
	}
	if _, err := newModel("claude-sonnet-5-5", getenv); !errors.Is(err, providers.ErrMissingKey) {
		t.Errorf("cloud model without a key: err = %v, want ErrMissingKey", err)
	}
	if _, err := newModel("gpt-5", getenv); !errors.Is(err, providers.ErrUnknownModel) {
		t.Errorf("unknown model: err = %v, want ErrUnknownModel", err)
	}
}

func TestAgentFor_UnknownAgent(t *testing.T) {
	var cfg config
	cfg.Worker.Agent = "billing"
	if _, err := agentFor(cfg, &runBudget{}); err == nil {
		t.Fatal("agentFor(billing) = nil error, want unknown agent")
	}
	for _, name := range []string{"support", "orders"} {
		cfg.Worker.Agent = name
		if a, err := agentFor(cfg, &runBudget{}); err != nil || a.run == nil {
			t.Errorf("agentFor(%s) = %+v, %v", name, a, err)
		}
	}
}

func TestLoadConfig_AudienceRequiresHTTPS(t *testing.T) {
	base := map[string]string{"DATABASE_URL": "postgres://x", "COMMERCE_API_AUDIENCE": "https://commerce.example"}
	if _, err := loadConfig("support", env(base)); err == nil { // default URL is http://127.0.0.1
		t.Fatal("http COMMERCE_API_URL accepted with an audience")
	}
	base["COMMERCE_API_URL"] = "https://commerce.example"
	if _, err := loadConfig("support", env(base)); err != nil {
		t.Fatalf("https URL rejected: %v", err)
	}
}

func TestLoadConfig_CategoryOverCeilingRejected(t *testing.T) {
	if _, err := loadConfig("support", env(map[string]string{"DATABASE_URL": "postgres://x", "AUTO_REPLY_CATEGORIES": "refund_request"})); err == nil {
		t.Fatal("refund_request accepted as an auto-reply category")
	}
}
