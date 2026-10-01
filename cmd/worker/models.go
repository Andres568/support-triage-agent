package main

import (
	"net/http"

	"github.com/Andres568/support-triage-agent/internal/agent"
	"github.com/Andres568/support-triage-agent/internal/providers"
)

// newModel resolves a MODEL id through the provider registry to a per-item
// model factory. Adapters are stateless, so every item shares one model; the
// factory is the hook for evals to wrap items with record/replay.
//
// The HTTP client has no timeout of its own: each call is bounded by the
// item's context (ITEM_TIMEOUT), which also covers retries.
func newModel(id string, getenv func(string) string) (func(subject string, attempt int) agent.Model, error) {
	spec, err := providers.Lookup(id)
	if err != nil {
		return nil, err
	}
	m, err := providers.New(spec, getenv, &http.Client{})
	if err != nil {
		return nil, err
	}
	return func(string, int) agent.Model { return m }, nil
}
