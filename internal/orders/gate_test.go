package orders

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Andres568/support-triage-agent/internal/commerce"
	"github.com/Andres568/support-triage-agent/internal/handoff"
	"github.com/Andres568/support-triage-agent/internal/triage"
)

var gateOrder = commerce.Order{OrderNumber: "ORD-100105", Status: "delivered", TotalCents: 195_00, Currency: "USD",
	Items: json.RawMessage(`[{"sku":"MUG-CER-11OZ","qty":250},{"sku":"TOT-CNV-STD","qty":10}]`)}

func reprint(items ...ReprintItem) Proposal {
	return Proposal{Verdict: Eligible, PolicySlug: "reprint-damaged", Reason: "r", ReprintItems: items}
}

func refund(cents int) Proposal {
	return Proposal{Verdict: Eligible, PolicySlug: "refunds", Reason: "r", ReprintItems: []ReprintItem{}, RefundCents: cents}
}

func verdict(v Verdict) Proposal {
	return Proposal{Verdict: v, PolicySlug: "refunds", Reason: "r", ReprintItems: []ReprintItem{}}
}

func TestGate(t *testing.T) {
	big := gateOrder
	big.TotalCents = 1240_00
	eur := gateOrder
	eur.Currency = "EUR"
	tests := []struct {
		name     string
		typ      handoff.TaskType
		p        Proposal
		o        commerce.Order
		baseline Verdict
		want     Verdict
		override string
	}{
		{"agreement on a valid reprint passes", handoff.ReprintRequest, reprint(ReprintItem{"MUG-CER-11OZ", 250}), gateOrder, Eligible, Eligible, ""},
		{"partial reprint passes", handoff.ReprintRequest, reprint(ReprintItem{"TOT-CNV-STD", 4}), gateOrder, Eligible, Eligible, ""},
		{"eligible vs baseline not_eligible", handoff.ReprintRequest, reprint(ReprintItem{"MUG-CER-11OZ", 1}), gateOrder, NotEligible, NeedsHuman, "disagrees"},
		{"not_eligible vs baseline eligible", handoff.ReprintRequest, verdict(NotEligible), gateOrder, Eligible, NeedsHuman, "disagrees"},
		{"eligible vs baseline needs_human", handoff.RefundReview, refund(100_00), big, NeedsHuman, NeedsHuman, "disagrees"},
		{"agreement on not_eligible stays", handoff.AddressChange, verdict(NotEligible), gateOrder, NotEligible, NotEligible, ""},
		{"extra SKU", handoff.ReprintRequest, reprint(ReprintItem{"MUG-CER-11OZ", 1}, ReprintItem{"GOLD-FOIL", 1}), gateOrder, Eligible, NeedsHuman, "not on the order"},
		{"more than ordered", handoff.ReprintRequest, reprint(ReprintItem{"TOT-CNV-STD", 11}), gateOrder, Eligible, NeedsHuman, "exceeds the 10 ordered"},
		{"refund within total and limit", handoff.RefundReview, refund(195_00), gateOrder, Eligible, Eligible, ""},
		{"refund above the total", handoff.RefundReview, refund(195_01), gateOrder, Eligible, NeedsHuman, "exceeds the order total"},
		{"refund above the limit", handoff.RefundReview, refund(limit + 1), big, Eligible, NeedsHuman, "limit"},
		{"unreadable order items", handoff.ReprintRequest, reprint(ReprintItem{"MUG-CER-11OZ", 1}),
			commerce.Order{TotalCents: 1, Currency: "USD", Items: json.RawMessage(`"oops"`)}, Eligible, NeedsHuman, "unreadable"},
		{"refund on a reprint task, both agree", handoff.ReprintRequest, refund(100_00), gateOrder, Eligible, NeedsHuman, "does not fit"},
		{"non-USD order", handoff.RefundReview, refund(100_00), eur, Eligible, NeedsHuman, "currency"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := Gate(tt.typ, tt.p, tt.o, tt.baseline, limit)
			if out.Final.Verdict != tt.want || out.Baseline != tt.baseline || out.Proposed == nil || !reflect.DeepEqual(*out.Proposed, tt.p) {
				t.Fatalf("outcome = %+v, want final %s", out, tt.want)
			}
			joined := strings.Join(out.Overrides, "; ")
			if len(out.Overrides) > 0 && out.Final.Reason != out.Overrides[0] {
				t.Errorf("final reason = %q, want the first override", out.Final.Reason)
			}
			if (tt.override == "") != (joined == "") || !strings.Contains(joined, tt.override) {
				t.Errorf("overrides = %q, want one mentioning %q", joined, tt.override)
			}
		})
	}
}

// The gate can only keep the model's proposal or hand the task to a person:
// never a more permissive verdict, and never more work than proposed.
func TestGate_NeverLessCautious(t *testing.T) {
	proposals := []Proposal{
		verdict(Eligible), verdict(NotEligible), verdict(NeedsHuman),
		reprint(ReprintItem{"MUG-CER-11OZ", 250}), reprint(ReprintItem{"MUG-CER-11OZ", 251}), reprint(ReprintItem{"NOPE", 1}),
		reprint(ReprintItem{"TOT-CNV-STD", 10}, ReprintItem{"MUG-CER-11OZ", 1}), {Verdict: Eligible, PolicySlug: "refunds", Reason: "r"},
		refund(1), refund(195_00), refund(195_01), refund(limit + 1), refund(1240_00),
	}
	orders := []commerce.Order{gateOrder, {TotalCents: 1240_00, Currency: "USD", Items: gateOrder.Items}, {Currency: "USD", Items: json.RawMessage(`[]`)}, {TotalCents: 1240_00, Items: gateOrder.Items}}
	for _, typ := range handoff.TaskTypes {
		for _, p := range proposals {
			for _, o := range orders {
				for _, b := range Verdicts {
					out := Gate(typ, p, o, b, limit)
					f := out.Final
					switch {
					case f.Verdict != p.Verdict && f.Verdict != NeedsHuman:
						t.Errorf("%s %+v baseline %s: final %s is neither the proposal nor needs_human", typ, p, b, f.Verdict)
					case f.Verdict == Eligible && (b != Eligible || !reflect.DeepEqual(f, p)):
						t.Errorf("%s %+v baseline %s: eligible final %+v differs from an agreed proposal", typ, p, b, f)
					case f.Verdict == Eligible && (f.Validate(typ) != nil || itemsProblem(f.ReprintItems, o.Items) != "" || o.Currency != Currency):
						t.Errorf("%s %+v: eligible final is invalid for the type, not a subset of the order, or not in USD", typ, p)
					case f.Verdict == Eligible && (f.RefundCents > o.TotalCents || f.RefundCents > limit):
						t.Errorf("%s %+v: eligible refund above total or limit", typ, p)
					case !reflect.DeepEqual(f, p) && (len(out.Overrides) == 0 || len(f.ReprintItems) > 0 || f.RefundCents != 0):
						t.Errorf("%s %+v baseline %s: changed final %+v must explain itself and propose nothing", typ, p, b, f)
					}
					// Whatever it decides, the gate's outcome passes the
					// write boundary's invariants for a valid proposal.
					out.Source = triage.SourceAgent
					if err := out.Check(typ); err != nil && p.Validate(typ) == nil {
						t.Errorf("%s %+v baseline %s: outcome fails Check: %v", typ, p, b, err)
					}
				}
			}
		}
	}
}

// Check is the write boundary's last word: one bad outcome per rule.
func TestOutcomeCheck(t *testing.T) {
	good := reprint(ReprintItem{"MUG-CER-11OZ", 1})
	ok := Outcome{Source: triage.SourceAgent, Proposed: &good, Final: good, Baseline: Eligible}
	tests := []struct {
		name string
		typ  handoff.TaskType
		edit func(o *Outcome)
		want string // "" = passes
	}{
		{"agreed eligible reprint passes", handoff.ReprintRequest, func(*Outcome) {}, ""},
		{"needs_human from the step limit passes", handoff.ReprintRequest, func(o *Outcome) {
			*o = Outcome{Source: triage.SourceAgentLimit, Final: needsHuman("", "limit")}
		}, ""},
		{"unknown source", handoff.ReprintRequest, func(o *Outcome) { o.Source = "rules" }, "source"},
		{"eligible from the step limit", handoff.ReprintRequest, func(o *Outcome) { o.Source = triage.SourceAgentLimit }, "agent proposal"},
		{"eligible without a proposal", handoff.ReprintRequest, func(o *Outcome) { o.Proposed = nil }, "agent proposal"},
		{"eligible final differs from the proposal", handoff.ReprintRequest, func(o *Outcome) {
			o.Final = reprint(ReprintItem{"MUG-CER-11OZ", 2})
		}, "unchanged"},
		{"eligible against the baseline", handoff.ReprintRequest, func(o *Outcome) { o.Baseline = NeedsHuman }, "baseline"},
		{"eligible with overrides", handoff.ReprintRequest, func(o *Outcome) { o.Overrides = []string{"x"} }, "overrides"},
		{"eligible but wrong for the type", handoff.RefundReview, func(*Outcome) {}, "invalid for refund_review"},
		{"needs_human with items", handoff.ReprintRequest, func(o *Outcome) { o.Final.Verdict = NeedsHuman }, "no reprint or refund"},
		{"not_eligible with a refund", handoff.RefundReview, func(o *Outcome) { o.Final = refund(1); o.Final.Verdict = NotEligible }, "no reprint or refund"},
		{"unknown verdict", handoff.ReprintRequest, func(o *Outcome) { o.Final.Verdict = "maybe" }, "unknown final verdict"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := ok
			tt.edit(&o)
			err := o.Check(tt.typ)
			if (tt.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tt.want)) {
				t.Fatalf("Check = %v, want %q", err, tt.want)
			}
		})
	}
}
