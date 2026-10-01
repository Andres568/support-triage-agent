package main

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Andres568/support-triage-agent/internal/triage"
	"github.com/Andres568/support-triage-agent/internal/worker"
)

// config is the worker's whole configuration. It comes from the environment
// (12-factor), so the same image runs locally, in compose and on Cloud Run.
type config struct {
	Worker      worker.Config
	DatabaseURL string
	CommerceURL string
	// CommerceAudience, when set, makes calls carry a metadata-server ID
	// token for it (Cloud Run: commerce-api has no allUsers invoker).
	CommerceAudience string
	Model            string
	Policy           triage.Policy
	MaxSteps         int
	MaxTokens        int
	// MaxRunCostMicros stops claiming once a run has spent it (0 = no cap).
	MaxRunCostMicros int64
}

// Defaults are safe for a laptop running Ollama: one item at a time, and
// shadow autonomy so nothing could reach a customer.
var defaults = map[string]string{
	"COMMERCE_API_URL":      "http://127.0.0.1:18080",
	"MODEL":                 "ollama/qwen3:8b",
	"AUTONOMY":              string(triage.AutonomyShadow),
	"AUTO_REPLY_CATEGORIES": "order_status,general",
	"BATCH_SIZE":            "20",
	"CONCURRENCY":           "1",
	"ITEM_TIMEOUT":          "180s",
	"FINALIZE_TIMEOUT":      "10s",
	"LEASE":                 "240s",
	"MAX_ATTEMPTS":          "3",
	"MAX_STEPS":             "8",
	"MAX_TOKENS":            "60000",
	"MAX_RUN_COST_MICROS":   "0",
}

// loadConfig reports every bad variable at once.
// lookup is os.LookupEnv in production: set-but-empty differs from unset for
// AUTO_REPLY_CATEGORIES.
func loadConfig(agentName string, lookup func(string) (string, bool)) (config, error) {
	var errs []error
	get := func(key string) string {
		if v, _ := lookup(key); strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		return defaults[key]
	}
	num := func(key string) int {
		n, err := strconv.Atoi(get(key))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
		}
		return n
	}
	dur := func(key string) time.Duration {
		d, err := time.ParseDuration(get(key))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
		}
		return d
	}

	c := config{
		DatabaseURL:      get("DATABASE_URL"),
		CommerceURL:      get("COMMERCE_API_URL"),
		CommerceAudience: get("COMMERCE_API_AUDIENCE"),
		Model:            get("MODEL"),
		MaxSteps:         num("MAX_STEPS"),
		MaxTokens:        num("MAX_TOKENS"),
		MaxRunCostMicros: int64(num("MAX_RUN_COST_MICROS")),
		Worker: worker.Config{
			Agent:           agentName,
			Batch:           num("BATCH_SIZE"),
			Concurrency:     num("CONCURRENCY"),
			MaxAttempts:     num("MAX_ATTEMPTS"),
			ItemTimeout:     dur("ITEM_TIMEOUT"),
			FinalizeTimeout: dur("FINALIZE_TIMEOUT"),
			Lease:           dur("LEASE"),
		},
	}
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}
	if c.CommerceAudience != "" && !strings.HasPrefix(c.CommerceURL, "https://") {
		errs = append(errs, fmt.Errorf("COMMERCE_API_URL %q must be https:// when COMMERCE_API_AUDIENCE is set: the ID token must not travel in clear text", c.CommerceURL))
	}
	if c.MaxRunCostMicros < 0 {
		errs = append(errs, fmt.Errorf("MAX_RUN_COST_MICROS %d must be >= 0", c.MaxRunCostMicros))
	}
	if c.MaxSteps < 1 || c.MaxTokens < 1 {
		errs = append(errs, fmt.Errorf("MAX_STEPS %d and MAX_TOKENS %d must be >= 1", c.MaxSteps, c.MaxTokens))
	}

	level := triage.Autonomy(get("AUTONOMY"))
	if !slices.Contains([]triage.Autonomy{triage.AutonomyShadow, triage.AutonomySuggest, triage.AutonomyAuto}, level) {
		errs = append(errs, fmt.Errorf("AUTONOMY %q must be shadow, suggest or auto", level))
	}
	// Set but empty means "auto-reply nothing", the safest allowlist; only an
	// unset variable falls back to the default.
	cats, ok := lookup("AUTO_REPLY_CATEGORIES")
	if !ok {
		cats = defaults["AUTO_REPLY_CATEGORIES"]
	}
	allow := []triage.Category{}
	for s := range strings.SplitSeq(cats, ",") {
		cat := triage.Category(strings.TrimSpace(s))
		if cat == "" {
			continue
		}
		if !slices.Contains(triage.Categories, cat) {
			errs = append(errs, fmt.Errorf("AUTO_REPLY_CATEGORIES: unknown category %q", cat))
		} else if !slices.Contains(triage.AutoReplyCeiling, cat) {
			errs = append(errs, fmt.Errorf("AUTO_REPLY_CATEGORIES: %q may never auto-reply; allowed: %v", cat, triage.AutoReplyCeiling))
		}
		allow = append(allow, cat)
	}
	c.Policy = triage.PolicyFor(level, allow)

	return c, errors.Join(append(errs, c.Worker.Validate())...)
}
