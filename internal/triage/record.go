package triage

import (
	"errors"
	"fmt"
	"slices"
)

// Source says who produced a ticket's decision.
type Source string

const (
	SourcePreCheck   Source = "precheck"    // high-risk term; the model never ran
	SourceAgent      Source = "agent"       // the model submitted a decision
	SourceAgentLimit Source = "agent_limit" // the model could not decide within its limits
)

// Record is what tickets.decision stores. Keeping the model's proposal next
// to the gated outcome lets evals score both, and shows what the gate changed.
type Record struct {
	Source    Source    `json:"source"`
	Proposed  *Decision `json:"proposed,omitempty"` // the model's validated output, before the gate
	Final     Decision  `json:"final"`
	Overrides []string  `json:"overrides,omitempty"`
	Autonomy  Autonomy  `json:"autonomy"`
	Model     string    `json:"model,omitempty"`
}

// Status is the tickets.status for the final action, or "" if the action is
// unknown (the database CHECK then rejects the write).
func (r Record) Status() string {
	switch r.Final.Action {
	case ActionAutoReply:
		return "auto_replied"
	case ActionDraftForReview:
		return "drafted"
	case ActionEscalate:
		return "escalated"
	}
	return ""
}

// Check is the last line of defense for an auto-reply, at the write
// boundary: it re-states what the gate guarantees, with code-level limits
// that no configuration can widen. A record that fails it is a bug (in the
// gate, the policy or a caller), so it is never stored.
func (r Record) Check() error {
	if r.Final.Action != ActionAutoReply {
		return nil
	}
	var errs []error
	if r.Source != SourceAgent {
		errs = append(errs, fmt.Errorf("auto-reply from source %q, want %q", r.Source, SourceAgent))
	}
	if r.Autonomy != AutonomyAuto {
		errs = append(errs, fmt.Errorf("auto-reply under autonomy %q, want %q", r.Autonomy, AutonomyAuto))
	}
	if !slices.Contains(AutoReplyCeiling, r.Final.Category) {
		errs = append(errs, fmt.Errorf("auto-reply in category %q, not one of %v", r.Final.Category, AutoReplyCeiling))
	}
	if r.Final.Confidence < MinAutoReplyConfidenceFloor {
		errs = append(errs, fmt.Errorf("auto-reply at confidence %.2f, below %.2f", r.Final.Confidence, MinAutoReplyConfidenceFloor))
	}
	if slices.ContainsFunc(r.Final.FollowUps, promisesCompensation) {
		errs = append(errs, errors.New("auto-reply with a reprint or refund follow-up"))
	}
	return errors.Join(errs...)
}
