// Package providers maps a model id (the MODEL setting, stored with every
// decision) to a configured agent.Model, its price and its limits. One table
// is the single place a model is added, repriced or retired.
package providers

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Andres568/support-triage-agent/internal/agent"
	"github.com/Andres568/support-triage-agent/internal/providers/anthropic"
	"github.com/Andres568/support-triage-agent/internal/providers/openairesp"
)

// Provider is the OpenTelemetry gen_ai.provider.name of a model's API.
type Provider string

const (
	Anthropic Provider = "anthropic"
	OpenAI    Provider = "openai"
	XAI       Provider = "x_ai"
	Ollama    Provider = "ollama"
	Fake      Provider = "fake" // in-process, for pipelines and tests without an LLM
)

// Providers is every known provider; the registry test checks entries against it.
var Providers = []Provider{Anthropic, OpenAI, XAI, Ollama, Fake}

var (
	ErrUnknownModel = errors.New("providers: unknown model")
	ErrMissingKey   = errors.New("providers: API key not set")
)

// OllamaBaseURLEnv overrides DefaultOllamaBaseURL (compose sets it to
// http://host.docker.internal:11434/v1).
const (
	OllamaBaseURLEnv     = "OLLAMA_BASE_URL"
	DefaultOllamaBaseURL = "http://127.0.0.1:11434/v1"
)

// Price is in micro-USD per million tokens, so costs are exact integers.
type Price struct {
	InputMicrosPerMTok      int64
	OutputMicrosPerMTok     int64
	CacheReadMicrosPerMTok  int64
	CacheWriteMicrosPerMTok int64
}

// Cost is the price of u in micro-USD, rounded down once at the end. Apply it
// to aggregated Usage (a run's or an item's total), not per call and then
// summed, so the rounding happens once.
func (p Price) Cost(u agent.Usage) int64 {
	return (int64(u.InputTokens)*p.InputMicrosPerMTok +
		int64(u.OutputTokens)*p.OutputMicrosPerMTok +
		int64(u.CacheReadTokens)*p.CacheReadMicrosPerMTok +
		int64(u.CacheWriteTokens)*p.CacheWriteMicrosPerMTok) / 1_000_000
}

type ModelSpec struct {
	ID              string // what MODEL is set to
	Provider        Provider
	APIModel        string // the name the provider's API expects
	BaseURL         string
	KeyEnv          string // environment variable holding the API key
	Price           Price
	MaxOutputTokens int
	ContextWindow   int // > 0 enables the adapter's silent-truncation guard
	ReasoningEffort string
	Temperature     *float64
	PricesVerified  string // date the price was checked against the provider's page
}

const usd = 1_000_000 // micro-USD

// anthropicPrice builds a price from USD per million tokens. Anthropic bills
// a cache read at 0.1x input and a 5-minute cache write at 1.25x.
func anthropicPrice(inUSD, outUSD int64) Price {
	return Price{InputMicrosPerMTok: inUSD * usd, OutputMicrosPerMTok: outUSD * usd,
		CacheReadMicrosPerMTok: inUSD * usd / 10, CacheWriteMicrosPerMTok: inUSD * usd * 5 / 4}
}

// cloudPrice is for OpenAI and xAI. Their cached-input prices were NOT
// verified, so a cache read is charged at the full input price: the cost
// is overestimated rather than hidden.
func cloudPrice(inMicros, outMicros int64) Price {
	return Price{InputMicrosPerMTok: inMicros, OutputMicrosPerMTok: outMicros, CacheReadMicrosPerMTok: inMicros}
}

// cloudContextFloor is a conservative window for the OpenAI and xAI models:
// each documents a larger one, but these ids were not individually checked,
// so the guard uses a floor every current model exceeds. MAX_TOKENS (60k by
// default) binds long before it. Anthropic has no entry: its API rejects an
// over-long prompt instead of truncating it silently, and the adapter does
// not take a window.
const cloudContextFloor = 128_000

var zeroTemperature = 0.0

// Models is the registry. Local models are free; they run at temperature 0
// and without reasoning (qwen3 is 4x faster without it).
var Models = []ModelSpec{
	{ID: "claude-opus-5-5", Provider: Anthropic, APIModel: "claude-opus-5-5", BaseURL: "https://api.anthropic.com", KeyEnv: "ANTHROPIC_API_KEY", Price: anthropicPrice(4, 20), MaxOutputTokens: 4096, PricesVerified: "2026-09-29"},
	{ID: "claude-sonnet-5-5", Provider: Anthropic, APIModel: "claude-sonnet-5-5", BaseURL: "https://api.anthropic.com", KeyEnv: "ANTHROPIC_API_KEY", Price: anthropicPrice(2, 10), MaxOutputTokens: 4096, PricesVerified: "2026-09-29"},
	{ID: "claude-haiku-4-5-20251001", Provider: Anthropic, APIModel: "claude-haiku-4-5-20251001", BaseURL: "https://api.anthropic.com", KeyEnv: "ANTHROPIC_API_KEY", Price: anthropicPrice(1, 5), MaxOutputTokens: 4096, PricesVerified: "2026-09-29"},
	{ID: "gpt-6.1-sol", Provider: OpenAI, APIModel: "gpt-6.1-sol", BaseURL: "https://api.openai.com/v1", KeyEnv: "OPENAI_API_KEY", Price: cloudPrice(2*usd, 10*usd), MaxOutputTokens: 4096, ContextWindow: cloudContextFloor, PricesVerified: "2026-09-29"},
	{ID: "gpt-6-luna", Provider: OpenAI, APIModel: "gpt-6-luna", BaseURL: "https://api.openai.com/v1", KeyEnv: "OPENAI_API_KEY", Price: cloudPrice(100_000, 500_000), MaxOutputTokens: 4096, ContextWindow: cloudContextFloor, PricesVerified: "2026-09-29"},
	{ID: "grok-4.7", Provider: XAI, APIModel: "grok-4.7", BaseURL: "https://api.x.ai/v1", KeyEnv: "XAI_API_KEY", Price: cloudPrice(2*usd, 6*usd), MaxOutputTokens: 4096, ContextWindow: cloudContextFloor, PricesVerified: "2026-09-29"},
	{ID: "grok-4.3", Provider: XAI, APIModel: "grok-4.3", BaseURL: "https://api.x.ai/v1", KeyEnv: "XAI_API_KEY", Price: cloudPrice(1_250_000, 2_500_000), MaxOutputTokens: 4096, ContextWindow: cloudContextFloor, PricesVerified: "2026-09-29"},
	{ID: "ollama/qwen3:8b", Provider: Ollama, APIModel: "qwen3:8b", BaseURL: DefaultOllamaBaseURL, MaxOutputTokens: 2048, ContextWindow: 16384, ReasoningEffort: "none", Temperature: &zeroTemperature},
	{ID: "ollama/qwen3:4b-instruct", Provider: Ollama, APIModel: "qwen3:4b-instruct", BaseURL: DefaultOllamaBaseURL, MaxOutputTokens: 2048, ContextWindow: 16384, ReasoningEffort: "none", Temperature: &zeroTemperature},
	{ID: "fake/escalate-all", Provider: Fake},
	{ID: "fake/oracle", Provider: Fake}, // golden answers; built by internal/evals, see ErrOracleOutsideEvals
}

func Lookup(id string) (ModelSpec, error) {
	ids := make([]string, 0, len(Models))
	for _, m := range Models {
		if m.ID == id {
			return m, nil
		}
		ids = append(ids, m.ID)
	}
	return ModelSpec{}, fmt.Errorf("%w %q; known: %s", ErrUnknownModel, id, strings.Join(ids, ", "))
}

// New builds the model for spec. It fails fast when a cloud model's key is
// missing, so a misconfigured run stops before claiming any work.
func New(spec ModelSpec, getenv func(string) string, hc *http.Client) (agent.Model, error) {
	key := ""
	if spec.KeyEnv != "" {
		if key = getenv(spec.KeyEnv); key == "" {
			return nil, fmt.Errorf("%w: %s needs %s", ErrMissingKey, spec.ID, spec.KeyEnv)
		}
	}
	switch spec.Provider {
	case Fake:
		return newFake(spec.ID)
	case Anthropic:
		return anthropic.New(spec.BaseURL, key, spec.APIModel, spec.MaxOutputTokens, hc), nil
	case OpenAI, XAI, Ollama:
		base := spec.BaseURL
		if v := getenv(OllamaBaseURLEnv); spec.Provider == Ollama && v != "" {
			base = v
		}
		return openairesp.New(base, key, spec.APIModel, openairesp.Options{
			MaxOutputTokens: spec.MaxOutputTokens,
			ReasoningEffort: spec.ReasoningEffort,
			Temperature:     spec.Temperature,
			ContextWindow:   spec.ContextWindow,
		}, hc), nil
	}
	return nil, fmt.Errorf("providers: %s: unknown provider %q", spec.ID, spec.Provider)
}
