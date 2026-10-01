// Package evals scores the agents against the golden labels: the support
// agent's decisions before and after the gate, and the orders agent's
// verdicts against the labels and the rules baseline. The harness (Run)
// drives the real worker over a synthetic eval database.
package evals

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"github.com/Andres568/support-triage-agent/internal/handoff"
	"github.com/Andres568/support-triage-agent/internal/orders"
	"github.com/Andres568/support-triage-agent/internal/triage"
)

// Case types in golden.jsonl.
const CaseInjection = "prompt_injection"

// Golden is one support ticket's label (evals/golden.jsonl).
type Golden struct {
	ExternalID string             `json:"external_id"`
	CaseType   string             `json:"case_type"`
	Category   triage.Category    `json:"category"`
	Action     triage.Action      `json:"action"`
	Rationale  string             `json:"rationale"`
	FollowUps  []handoff.FollowUp `json:"follow_ups"`
	// MustNotContain lists terms a draft must never include (case-insensitive):
	// what an injection tried to extract or promise.
	MustNotContain []string `json:"must_not_contain,omitempty"`
}

// GoldenTask is one orders task's label (evals/golden_tasks.jsonl).
type GoldenTask struct {
	Ticket      string           `json:"ticket"`
	Type        handoff.TaskType `json:"type"`
	Reason      handoff.Reason   `json:"reason"`
	OrderNumber string           `json:"order_number"`
	Verdict     orders.Verdict   `json:"verdict"`
	RefundCents *int             `json:"refund_cents,omitempty"` // an eligible refund's amount; default the order total
}

// Subject is the task's eval key, as tasks.Task.Subject names it.
func (g GoldenTask) Subject() string { return g.Ticket + "/" + string(g.Type) }

// LoadJSONL decodes one JSON object per non-empty line, strictly.
func LoadJSONL[T any](path string) ([]T, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []T
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for n := 1; sc.Scan(); n++ {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var v T
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		out = append(out, v)
	}
	return out, sc.Err()
}
