package triage

import (
	"slices"
	"strings"
	"testing"

	"github.com/Andres568/support-triage-agent/internal/handoff"
)

func TestHighRiskTerm(t *testing.T) {
	tests := []struct {
		text string
		want string // empty = no match
	}{
		{"If this is not fixed today I am filing a chargeback with my bank.", "chargeback"},
		{"I will do a CHARGE BACK", "charge back"},
		{"My lawyer will be in touch.", "lawyer"},
		{"This looks like fraud to me", "fraud"},
		{"An unauthorised payment on my card", "unauthorised"},
		{"We will sue you", "sue"},
		// Word boundaries: substrings must not match.
		{"There is an issue with my order", ""},
		{"Is this pursued further?", ""},
		{"Where is my order?", ""},
		// Known false positive, accepted on purpose: escalating is the safe side.
		{"I am NOT going to file a chargeback, just asking", "chargeback"},
		// Evasions folded before matching (see foldForMatch).
		{"My l a w y e r will call", "lawyer"},
		{"My l.a.w.y.e.r will call", "lawyer"},
		{"My lаwyer will call", "lawyer"}, // Cyrillic а
		{"I will file a ch@rgeback", "chargeback"},
		{"This is fr@ud", "fraud"},
		{"My law\u200byer", "lawyer"}, // zero-width space
		{"ｌａｗｙｅｒ", "lawyer"},          // fullwidth
		{"I will s u e you", "sue"},
		{"I will S.U.E. you", "sue"},
		{"s-u-e", "sue"},
		// Out of scope, documented: a word split in two.
		{"my law yer", ""},
		// Short spaced runs are ordinary text.
		{"Plan A or B, a b c", ""},
	}
	for _, tt := range tests {
		got, ok := HighRiskTerm(Ticket{Body: tt.text})
		if got != tt.want || ok != (tt.want != "") {
			t.Errorf("HighRiskTerm(%q) = %q, %v; want %q", tt.text, got, ok, tt.want)
		}
	}
}

func TestPreCheck(t *testing.T) {
	d, ok := PreCheck(Ticket{Subject: "Third time writing", Body: "My lawyer will be in touch."})
	if !ok || d.Action != ActionEscalate || d.Category != "" {
		t.Fatalf("PreCheck = %+v, %v; want escalate with no category", d, ok)
	}
	if _, ok := PreCheck(Ticket{Body: "Where is my order?"}); ok {
		t.Fatal("PreCheck escalated a harmless ticket")
	}
}

// orders builds Facts from "order number → total cents" pairs.
func orders(kv map[string]int) Facts {
	f := Facts{Orders: map[string]OrderFacts{}}
	for num, cents := range kv {
		f.Orders[num] = OrderFacts{Status: "delivered", TotalCents: cents}
	}
	return f
}

func TestGate(t *testing.T) {
	policy := EvalPolicy()
	none := []handoff.FollowUp{}
	confident := func(c Category, a Action, fus ...handoff.FollowUp) Decision {
		return Decision{Category: c, Action: a, DraftReply: "Hi!", Confidence: 0.95, Reason: "r", FollowUps: append(none, fus...)}
	}
	reprint := handoff.FollowUp{Type: handoff.ReprintRequest, Reason: handoff.ReasonDamaged, OrderNumber: "ORD-100105"}
	refundOnBig := handoff.FollowUp{Type: handoff.RefundReview, Reason: handoff.ReasonDamaged, OrderNumber: "ORD-100108"}
	withOrder := Ticket{OrderNumber: "ORD-100105"}
	draftSays := func(reply string) Decision {
		d := confident(CategoryOrderStatus, ActionAutoReply)
		d.DraftReply = reply
		return d
	}

	tests := []struct {
		name          string
		ticket        Ticket
		decision      Decision
		facts         Facts
		want          Action
		wantFollowUps []handoff.FollowUp
		wantOverrides int
		unverified    bool // the sender did not prove they own the address
	}{
		{
			name:     "confident auto reply in allowed category passes",
			decision: confident(CategoryOrderStatus, ActionAutoReply),
			want:     ActionAutoReply,
		},
		{
			name:          "low confidence downgrades auto reply to draft",
			decision:      Decision{Category: CategoryOrderStatus, Action: ActionAutoReply, DraftReply: "Hi", Confidence: 0.5, Reason: "r", FollowUps: none},
			want:          ActionDraftForReview,
			wantOverrides: 1,
		},
		{
			name:          "category without autonomy gets a draft",
			decision:      confident(CategoryDamagedOrMisprint, ActionAutoReply),
			want:          ActionDraftForReview,
			wantOverrides: 1,
		},
		{
			name:          "refund above limit escalates, using the real order total",
			ticket:        Ticket{OrderNumber: "ORD-100108"},
			decision:      confident(CategoryRefundRequest, ActionDraftForReview),
			facts:         orders(map[string]int{"ORD-100108": 1240_00}),
			want:          ActionEscalate,
			wantOverrides: 1,
		},
		// The rule must not depend on how the customer typed the number.
		{
			name:          "refund above limit escalates with a lowercase ticket order number",
			ticket:        Ticket{OrderNumber: "ord-100108"},
			decision:      confident(CategoryRefundRequest, ActionDraftForReview),
			facts:         orders(map[string]int{"ORD-100108": 1240_00}),
			want:          ActionEscalate,
			wantOverrides: 1,
		},
		{
			name:          "refund above limit escalates with a spaced ticket order number",
			ticket:        Ticket{OrderNumber: "ORD 100108"},
			decision:      confident(CategoryRefundRequest, ActionDraftForReview),
			facts:         orders(map[string]int{"ORD-100108": 1240_00}),
			want:          ActionEscalate,
			wantOverrides: 1,
		},
		{
			name:          "refund above limit escalates with no ticket order number at all",
			decision:      confident(CategoryRefundRequest, ActionDraftForReview),
			facts:         orders(map[string]int{"ORD-100108": 1240_00}),
			want:          ActionEscalate,
			wantOverrides: 1,
		},
		{
			name:     "refund under limit stays a draft",
			ticket:   Ticket{OrderNumber: "ORD-100115"},
			decision: confident(CategoryRefundRequest, ActionDraftForReview),
			facts:    orders(map[string]int{"ORD-100115": 80_00}),
			want:     ActionDraftForReview,
		},
		{
			name:          "refund promised under another category still escalates",
			ticket:        Ticket{OrderNumber: "ORD-100108"},
			decision:      Decision{Category: CategoryGeneral, Action: ActionAutoReply, DraftReply: "We issued a full refund.", Confidence: 0.99, Reason: "r", FollowUps: none},
			facts:         orders(map[string]int{"ORD-100108": 1240_00}),
			want:          ActionEscalate,
			wantOverrides: 1,
		},
		{
			name:          "high-risk term escalates even if the model chose auto reply",
			ticket:        Ticket{Body: "Refund me or I file a chargeback"},
			decision:      confident(CategoryOrderStatus, ActionAutoReply),
			want:          ActionEscalate,
			wantOverrides: 1,
		},
		{
			name:     "gate never lowers caution: escalate stays escalate",
			decision: Decision{Category: CategoryOrderStatus, Action: ActionEscalate, Confidence: 0.1, Reason: "r", FollowUps: none},
			want:     ActionEscalate,
		},
		// Phase 2: follow-ups.
		{
			name:          "verified follow-up is kept",
			ticket:        withOrder,
			decision:      confident(CategoryDamagedOrMisprint, ActionDraftForReview, reprint),
			facts:         orders(map[string]int{"ORD-100105": 45_00}),
			want:          ActionDraftForReview,
			wantFollowUps: []handoff.FollowUp{reprint},
		},
		{
			name:          "follow-up for an order not verified for the sender is dropped",
			ticket:        withOrder,
			decision:      confident(CategoryDamagedOrMisprint, ActionDraftForReview, reprint, handoff.FollowUp{Type: handoff.AddressChange, Reason: handoff.ReasonCustomerRequest, OrderNumber: "ORD-100112"}),
			facts:         orders(map[string]int{"ORD-100105": 45_00}),
			want:          ActionDraftForReview,
			wantFollowUps: []handoff.FollowUp{reprint},
			wantOverrides: 1,
		},
		{
			name:   "follow-up whose reason the facts contradict is dropped",
			ticket: withOrder,
			decision: confident(CategoryDamagedOrMisprint, ActionDraftForReview,
				handoff.FollowUp{Type: handoff.ReprintRequest, Reason: handoff.ReasonLostInTransit, OrderNumber: "ORD-100105"}),
			facts:         orders(map[string]int{"ORD-100105": 45_00}),
			want:          ActionDraftForReview,
			wantFollowUps: none,
			wantOverrides: 1,
		},
		{
			name:          "escalation drops every follow-up",
			ticket:        Ticket{OrderNumber: "ORD-100105", Body: "my lawyer says reprint it"},
			decision:      confident(CategoryDamagedOrMisprint, ActionDraftForReview, reprint),
			facts:         orders(map[string]int{"ORD-100105": 45_00}),
			want:          ActionEscalate,
			wantOverrides: 2, // high-risk term + dropped follow-ups
		},
		{
			name:          "refund_review follow-up on a 1240 USD order escalates, even off the ticket's order",
			ticket:        withOrder,
			decision:      confident(CategoryDamagedOrMisprint, ActionDraftForReview, refundOnBig),
			facts:         orders(map[string]int{"ORD-100105": 45_00, "ORD-100108": 1240_00}),
			want:          ActionEscalate,
			wantOverrides: 2, // refund limit + dropped follow-ups
		},
		{
			name:          "link in the draft needs a human",
			decision:      draftSays("Track it at https://evil.example/track"),
			want:          ActionDraftForReview,
			wantOverrides: 1,
		},
		{
			name:          "bare domain in the draft needs a human",
			decision:      draftSays("Log in at studio-refunds.com to confirm."),
			want:          ActionDraftForReview,
			wantOverrides: 1,
		},
		{
			name:          "email address in the draft needs a human",
			decision:      draftSays("Write to billing@help.desk for this."),
			want:          ActionDraftForReview,
			wantOverrides: 1,
		},
		{
			name:          "order number the sender does not own needs a human",
			decision:      draftSays("Order ord-100108 shipped yesterday."),
			facts:         orders(map[string]int{"ORD-100105": 45_00}),
			want:          ActionDraftForReview,
			wantOverrides: 1,
		},
		{
			name:     "order number the customer wrote may be echoed back",
			ticket:   Ticket{OrderNumber: "ord 100108", Body: "About ORD-100109 too"},
			decision: draftSays("Please write from the email on ORD-100108 and ORD-100109."),
			want:     ActionAutoReply,
		},
		{
			name:     "order number the sender owns may auto-reply",
			decision: draftSays("Order ORD-100105 shipped yesterday."),
			facts:    orders(map[string]int{"ORD-100105": 45_00}),
			want:     ActionAutoReply,
		},
		{
			name:          "promise of compensation needs a human",
			decision:      draftSays("Good news: we’ll reprint it free of charge."),
			want:          ActionDraftForReview,
			wantOverrides: 1,
		},
		{
			name:          "overlong draft needs a human",
			decision:      draftSays(strings.Repeat("a", MaxDraftRunes+1)),
			want:          ActionDraftForReview,
			wantOverrides: 1,
		},
		{
			name:          "compensation follow-up needs a human, even when dropped",
			decision:      confident(CategoryOrderStatus, ActionAutoReply, handoff.FollowUp{Type: handoff.ReprintRequest, Reason: handoff.ReasonLostInTransit, OrderNumber: "ORD-100999"}),
			want:          ActionDraftForReview,
			wantOverrides: 2, // compensation + dropped unverified follow-up
		},
		{
			name:          "unverified sender gets a draft and no follow-ups",
			ticket:        withOrder,
			unverified:    true,
			decision:      confident(CategoryDamagedOrMisprint, ActionDraftForReview, reprint),
			facts:         orders(map[string]int{"ORD-100105": 45_00}),
			want:          ActionDraftForReview,
			wantOverrides: 1, // dropped follow-ups; already a draft
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.ticket.SenderVerified = !tt.unverified
			out := Gate(policy, tt.ticket, tt.decision, tt.facts)
			if out.Decision.Action != tt.want {
				t.Errorf("action = %s, want %s (overrides: %v)", out.Decision.Action, tt.want, out.Overrides)
			}
			if len(out.Overrides) != tt.wantOverrides {
				t.Errorf("overrides = %v, want %d", out.Overrides, tt.wantOverrides)
			}
			if out.Proposed != tt.decision.Action {
				t.Errorf("Proposed = %s, want the model's original %s", out.Proposed, tt.decision.Action)
			}
			want := tt.wantFollowUps
			if want == nil {
				want = none
			}
			if out.Decision.FollowUps == nil || !slices.Equal(out.Decision.FollowUps, want) {
				t.Errorf("follow-ups = %#v, want %v", out.Decision.FollowUps, want)
			}
		})
	}
}

func TestGate_DoesNotMutateInput(t *testing.T) {
	fus := []handoff.FollowUp{
		{Type: handoff.AddressChange, Reason: handoff.ReasonCustomerRequest, OrderNumber: "ORD-100112"},
		{Type: handoff.ReprintRequest, Reason: handoff.ReasonDamaged, OrderNumber: "ORD-100105"},
	}
	d := Decision{Category: CategoryOrderChange, Action: ActionDraftForReview, DraftReply: "x", Confidence: 0.9, Reason: "r", FollowUps: fus}
	Gate(EvalPolicy(), Ticket{}, d, orders(map[string]int{"ORD-100105": 45_00}))
	if fus[0].OrderNumber != "ORD-100112" || fus[1].OrderNumber != "ORD-100105" {
		t.Fatalf("Gate mutated the proposed follow-ups: %v", fus)
	}
}

// Monotonicity is the gate's core promise, so check it exhaustively rather
// than by example: for every input, the gated action is at least as cautious,
// and follow-ups are only ever removed, verified, and gone on escalation.
func TestGate_NeverLowersCaution(t *testing.T) {
	tickets := []Ticket{
		{Body: "where is my order", OrderNumber: "ORD-100001", SenderVerified: true},
		{Body: "where is my order", OrderNumber: "ORD-100001"},
		{Body: "c h a r g e b a c k now", SenderVerified: true},
	}
	facts := []Facts{
		{},
		orders(map[string]int{"ORD-100001": 10_00}),
		orders(map[string]int{"ORD-100001": 5000_00}),
		orders(map[string]int{"ORD-100001": 10_00, "ORD-100002": 5000_00}),
	}
	followUps := [][]handoff.FollowUp{
		{},
		{{Type: handoff.ReprintRequest, Reason: handoff.ReasonDamaged, OrderNumber: "ORD-100001"}},
		{{Type: handoff.RefundReview, Reason: handoff.ReasonDamaged, OrderNumber: "ORD-100002"}, {Type: handoff.AddressChange, Reason: handoff.ReasonCustomerRequest, OrderNumber: "ORD-100003"}},
	}
	policies := []Policy{PolicyFor(AutonomyAuto, Categories), PolicyFor(AutonomyShadow, Categories)}
	// Every 7th combination: 7 divides no dimension's size, so each value of
	// every dimension is still exercised, in about 1/7 of the time (the full
	// product took ~40s under -race).
	const stride = 7
	n := 0
	drafts := []string{"", "refund", "Your order ORD-100001 shipped.", "Order ORD-100002 shipped.", "See www.x.io", "mail a@b.co", "we will reprint it", strings.Repeat("x", MaxDraftRunes+1)}
	for _, p := range policies {
		for _, c := range Categories {
			for _, a := range Actions {
				for _, conf := range []float64{0, 0.79, 0.8, 1} {
					for _, draft := range drafts {
						for _, tk := range tickets {
							for _, f := range facts {
								for _, fus := range followUps {
									if n++; n%stride != 0 {
										continue
									}
									d := Decision{Category: c, Action: a, DraftReply: draft, Confidence: conf, Reason: "r", FollowUps: fus}
									checkMonotonic(t, Gate(p, tk, d, f), tk, d, f)
								}
							}
						}
					}
				}
			}
		}
	}
}

func checkMonotonic(t *testing.T, out Outcome, tk Ticket, d Decision, f Facts) {
	t.Helper()
	got := out.Decision
	if severity[got.Action] < severity[d.Action] {
		t.Fatalf("Gate lowered %s to %s for %+v %+v", d.Action, got.Action, d, f)
	}
	if !slices.Contains(Actions, got.Action) {
		t.Fatalf("Gate produced unknown action %q", got.Action)
	}
	if got.Action == ActionEscalate && len(got.FollowUps) > 0 {
		t.Fatalf("escalated with follow-ups %v", got.FollowUps)
	}
	if got.Action == ActionAutoReply {
		if probs := draftProblems(d.DraftReply, tk, f); len(probs) > 0 {
			t.Fatalf("auto-replied with draft problems %v", probs)
		}
		if !tk.SenderVerified {
			t.Fatal("auto-replied to an unverified sender")
		}
		if slices.ContainsFunc(d.FollowUps, promisesCompensation) {
			t.Fatalf("auto-replied with compensation follow-ups %v", d.FollowUps)
		}
	}
	if !tk.SenderVerified && len(got.FollowUps) > 0 {
		t.Fatalf("kept follow-ups %v for an unverified sender", got.FollowUps)
	}
	for _, fu := range got.FollowUps {
		if !slices.Contains(d.FollowUps, fu) {
			t.Fatalf("Gate added follow-up %v not in %v", fu, d.FollowUps)
		}
		if _, ok := f.Orders[fu.OrderNumber]; !ok {
			t.Fatalf("Gate kept follow-up %v for an unverified order", fu)
		}
	}
}

func TestPolicyFor(t *testing.T) {
	allow := []Category{CategoryOrderStatus, CategoryGeneral}
	for _, level := range []Autonomy{AutonomyShadow, AutonomySuggest} {
		if p := PolicyFor(level, allow); len(p.AutoReply) != 0 || p.Autonomy != level {
			t.Errorf("PolicyFor(%s) = %+v, want no auto-reply categories", level, p)
		}
	}
	if p := PolicyFor(AutonomyAuto, allow); !p.AutoReply[CategoryOrderStatus] || p.AutoReply[CategoryRefundRequest] {
		t.Errorf("PolicyFor(auto) = %+v, want exactly the allowlist", p)
	}
}

func TestPolicy_Cap(t *testing.T) {
	auto, shadow := EvalPolicy(), PolicyFor(AutonomyShadow, Categories)
	tests := []struct {
		p    Policy
		c    Category
		a    Action
		want Action
	}{
		{auto, CategoryOrderStatus, ActionAutoReply, ActionAutoReply},
		{auto, CategoryRefundRequest, ActionAutoReply, ActionDraftForReview}, // HD-2008's golden label
		{auto, CategoryOrderChange, ActionEscalate, ActionEscalate},
		{shadow, CategoryGeneral, ActionAutoReply, ActionDraftForReview},
		{shadow, CategoryGeneral, ActionDraftForReview, ActionDraftForReview},
	}
	for _, tt := range tests {
		if got := tt.p.Cap(tt.c, tt.a); got != tt.want {
			t.Errorf("Cap(%s, %s) under %s = %s, want %s", tt.c, tt.a, tt.p.Autonomy, got, tt.want)
		}
	}
}

func TestRecord_Status(t *testing.T) {
	for a, want := range map[Action]string{
		ActionAutoReply: "auto_replied", ActionDraftForReview: "drafted", ActionEscalate: "escalated", "bogus": "",
	} {
		if got := (Record{Final: Decision{Action: a}}).Status(); got != want {
			t.Errorf("Status() for %s = %q, want %q", a, got, want)
		}
	}
}

// A follow-up's reason must fit the ticket's category and the verified
// order, because the orders agent trusts it instead of reading the ticket.
func TestReasonProblem(t *testing.T) {
	fu := func(typ handoff.TaskType, r handoff.Reason) handoff.FollowUp {
		return handoff.FollowUp{Type: typ, Reason: r, OrderNumber: "ORD-100105"}
	}
	tests := []struct {
		c      Category
		fu     handoff.FollowUp
		status string
		ok     bool
	}{
		{CategoryDamagedOrMisprint, fu(handoff.ReprintRequest, handoff.ReasonDamaged), "delivered", true},
		{CategoryDamagedOrMisprint, fu(handoff.ReprintRequest, handoff.ReasonMisprint), "delivered", true},
		{CategoryRefundRequest, fu(handoff.RefundReview, handoff.ReasonDamaged), "delivered", true},
		{CategoryShippingDelay, fu(handoff.ReprintRequest, handoff.ReasonLostInTransit), "shipped", true},
		{CategoryRefundRequest, fu(handoff.RefundReview, handoff.ReasonCancelledBeforeProduction), "cancelled", true},
		{CategoryOrderChange, fu(handoff.AddressChange, handoff.ReasonCustomerRequest), "paid", true},
		{CategoryDamagedOrMisprint, fu(handoff.AddressChange, handoff.ReasonCustomerRequest), "paid", true},
		{CategoryDamagedOrMisprint, fu(handoff.ReprintRequest, handoff.ReasonCustomerRequest), "delivered", false},
		{CategoryDamagedOrMisprint, fu(handoff.ReprintRequest, handoff.ReasonLostInTransit), "shipped", false},
		{CategoryShippingDelay, fu(handoff.ReprintRequest, handoff.ReasonDamaged), "delivered", false},
		{CategoryRefundRequest, fu(handoff.RefundReview, handoff.ReasonDamaged), "shipped", false},
		{CategoryOrderStatus, fu(handoff.ReprintRequest, handoff.ReasonLostInTransit), "delivered", false},
		{CategoryRefundRequest, fu(handoff.RefundReview, handoff.ReasonCancelledBeforeProduction), "paid", false},
		{CategoryOrderChange, fu(handoff.AddressChange, handoff.ReasonDamaged), "paid", false},
		{CategoryRefundRequest, fu(handoff.RefundReview, "vip"), "delivered", false},
		{CategoryOrderStatus, fu(handoff.ReprintRequest, handoff.ReasonLostInTransit), "shipped", true},
		{CategoryRefundRequest, fu(handoff.RefundReview, handoff.ReasonLostInTransit), "shipped", true},
		{CategoryOrderChange, fu(handoff.RefundReview, handoff.ReasonCancelledBeforeProduction), "cancelled", true},
		{CategoryGeneral, fu(handoff.RefundReview, handoff.ReasonCustomerRequest), "delivered", true},
		{CategoryGeneral, fu(handoff.ReprintRequest, handoff.ReasonCustomerRequest), "delivered", false},
	}
	for _, tt := range tests {
		if why := reasonProblem(tt.c, tt.fu, tt.status); (why == "") != tt.ok {
			t.Errorf("%s %s/%s on %s: problem %q, want ok=%v", tt.c, tt.fu.Type, tt.fu.Reason, tt.status, why, tt.ok)
		}
	}

	// The order status fits each reason below: only the category is wrong.
	wrongCategory := []struct {
		c      Category
		fu     handoff.FollowUp
		status string
	}{
		{CategoryGeneral, fu(handoff.ReprintRequest, handoff.ReasonDamaged), "delivered"},
		{CategoryOrderStatus, fu(handoff.RefundReview, handoff.ReasonMisprint), "delivered"},
		{CategoryOrderChange, fu(handoff.RefundReview, handoff.ReasonDamaged), "delivered"},
		{CategoryGeneral, fu(handoff.RefundReview, handoff.ReasonLostInTransit), "shipped"},
		{CategoryOrderChange, fu(handoff.ReprintRequest, handoff.ReasonLostInTransit), "shipped"},
		{CategoryGeneral, fu(handoff.RefundReview, handoff.ReasonCancelledBeforeProduction), "cancelled"},
		{CategoryOrderStatus, fu(handoff.RefundReview, handoff.ReasonCancelledBeforeProduction), "cancelled"},
	}
	for _, tt := range wrongCategory {
		if why := reasonProblem(tt.c, tt.fu, tt.status); !strings.Contains(why, "does not fit category") {
			t.Errorf("%s %s/%s on %s: problem %q, want a category mismatch", tt.c, tt.fu.Type, tt.fu.Reason, tt.status, why)
		}
	}
}

// Record.Check rejects every auto-reply the gate should never have produced,
// one rule at a time.
func TestRecord_Check(t *testing.T) {
	good := func() Record {
		return Record{Source: SourceAgent, Autonomy: AutonomyAuto, Final: Decision{
			Category: CategoryOrderStatus, Action: ActionAutoReply, DraftReply: "Hi", Confidence: 0.9, Reason: "r",
			FollowUps: []handoff.FollowUp{{Type: handoff.AddressChange, Reason: handoff.ReasonCustomerRequest, OrderNumber: "ORD-100105"}},
		}}
	}
	if err := good().Check(); err != nil {
		t.Fatalf("good record: %v", err)
	}
	bad := map[string]func(*Record){
		"not from the agent":     func(r *Record) { r.Source = SourcePreCheck },
		"shadow autonomy":        func(r *Record) { r.Autonomy = AutonomyShadow },
		"suggest autonomy":       func(r *Record) { r.Autonomy = AutonomySuggest },
		"category over ceiling":  func(r *Record) { r.Final.Category = CategoryRefundRequest },
		"confidence below floor": func(r *Record) { r.Final.Confidence = 0.79 },
		"reprint follow-up": func(r *Record) {
			r.Final.FollowUps = []handoff.FollowUp{{Type: handoff.ReprintRequest, Reason: handoff.ReasonDamaged, OrderNumber: "ORD-100105"}}
		},
		"refund follow-up": func(r *Record) {
			r.Final.FollowUps = []handoff.FollowUp{{Type: handoff.RefundReview, Reason: handoff.ReasonDamaged, OrderNumber: "ORD-100105"}}
		},
	}
	for name, mutate := range bad {
		r := good()
		mutate(&r)
		if err := r.Check(); err == nil {
			t.Errorf("%s: Check accepted it", name)
		}
		r.Final.Action = ActionDraftForReview // only auto-replies are checked
		if err := r.Check(); err != nil {
			t.Errorf("%s as a draft: %v", name, err)
		}
	}
}

func TestDraftProblems_Promises(t *testing.T) {
	tests := []struct {
		draft   string
		promise bool
	}{
		// From recorded model drafts (evals/cassettes).
		{"We are sorry about the damage; we will reship the order for free.", true},
		{"A new set is on its way at no cost to you.", true},
		{"We'll send a replacement this week.", true},
		{"We will issue a refund once the return arrives.", true},
		{"We’ll give you a 20% discount on your next order.", true},
		{"I have refunded you in full.", true},
		{"Your refund has been issued.", true},
		{"We guarantee delivery by Friday.", true},
		{"Good news: we’ll reprint it free of charge.", true},
		// Informational, no promise.
		{"Tracking 9400111899223344550015 is live.", false},
		{"We can't guarantee delivery dates.", false},
		{"Your order shipped via USPS, tracking 9400111899223344550015.", false},
		{"Our refund policy allows returns within 30 days of delivery.", false},
		{"Where is my order? It shipped yesterday.", false},
	}
	for _, tt := range tests {
		probs := draftProblems(tt.draft, Ticket{}, Facts{})
		got := slices.ContainsFunc(probs, func(p string) bool { return strings.HasPrefix(p, "draft promises") })
		if got != tt.promise {
			t.Errorf("draftProblems(%q) = %v; want promise=%v", tt.draft, probs, tt.promise)
		}
	}
}

func TestDraftProblems_Links(t *testing.T) {
	for draft, want := range map[string]bool{
		"Log in at evil[.]com to confirm.":         true,
		"Visit evil dot com today.":                true,
		"Pay at http://10.0.0.1/pay":               true,
		"Pay at 192.168.1.20":                      true,
		"Your order shipped, tracking 9400111899.": false,
		"It ships in 3.5 days.":                    false,
	} {
		probs := draftProblems(draft, Ticket{}, Facts{})
		if got := slices.Contains(probs, "draft contains a link"); got != want {
			t.Errorf("draftProblems(%q) = %v; want link=%v", draft, probs, want)
		}
	}
}
