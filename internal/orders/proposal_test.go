package orders

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Andres568/support-triage-agent/internal/handoff"
)

func TestParseProposal(t *testing.T) {
	tests := []struct {
		name, raw string
		typ       handoff.TaskType
		wantErr   string // "" = valid
	}{
		{"eligible reprint", `{"verdict":"eligible","policy_slug":"reprint-damaged","reason":"r","reprint_items":[{"sku":"A","qty":1}],"refund_cents":0}`, handoff.ReprintRequest, ""},
		{"eligible refund", `{"verdict":"eligible","policy_slug":"refunds","reason":"r","reprint_items":[],"refund_cents":8000}`, handoff.RefundReview, ""},
		{"eligible address change", `{"verdict":"eligible","policy_slug":"address-changes","reason":"r","reprint_items":[],"refund_cents":0}`, handoff.AddressChange, ""},
		{"not eligible", `{"verdict":"not_eligible","policy_slug":"refunds","reason":"r","reprint_items":[],"refund_cents":0}`, handoff.RefundReview, ""},
		{"unknown field", `{"verdict":"eligible","policy_slug":"refunds","reason":"r","reprint_items":[],"refund_cents":1,"email":"x"}`, handoff.RefundReview, "unknown field"},
		{"missing items", `{"verdict":"needs_human","policy_slug":"refunds","reason":"r","refund_cents":0}`, handoff.RefundReview, "reprint_items is required"},
		{"missing refund", `{"verdict":"needs_human","policy_slug":"refunds","reason":"r","reprint_items":[]}`, handoff.RefundReview, "refund_cents is required"},
		{"bad verdict and slug", `{"verdict":"yes","policy_slug":"vip","reason":"r","reprint_items":[],"refund_cents":0}`, handoff.RefundReview, "policy_slug"},
		{"empty reason", `{"verdict":"needs_human","policy_slug":"refunds","reason":" ","reprint_items":[],"refund_cents":0}`, handoff.RefundReview, "reason"},
		{"eligible reprint without items", `{"verdict":"eligible","policy_slug":"reprint-damaged","reason":"r","reprint_items":[],"refund_cents":0}`, handoff.ReprintRequest, "must list"},
		{"reprint with a refund", `{"verdict":"eligible","policy_slug":"reprint-damaged","reason":"r","reprint_items":[{"sku":"A","qty":1}],"refund_cents":5}`, handoff.ReprintRequest, "refund_cents must be 0"},
		{"eligible refund of 0", `{"verdict":"eligible","policy_slug":"refunds","reason":"r","reprint_items":[],"refund_cents":0}`, handoff.RefundReview, "refund_cents > 0"},
		{"address change with items", `{"verdict":"eligible","policy_slug":"address-changes","reason":"r","reprint_items":[{"sku":"A","qty":1}],"refund_cents":0}`, handoff.AddressChange, "no reprint or refund"},
		{"not eligible with a refund", `{"verdict":"not_eligible","policy_slug":"refunds","reason":"r","reprint_items":[],"refund_cents":100}`, handoff.RefundReview, "not eligible"},
		{"bad and duplicate items", `{"verdict":"eligible","policy_slug":"reprint-damaged","reason":"r","reprint_items":[{"sku":"A","qty":0},{"sku":"A","qty":1}],"refund_cents":0}`, handoff.ReprintRequest, "listed twice"},
		{"negative refund", `{"verdict":"needs_human","policy_slug":"refunds","reason":"r","reprint_items":[],"refund_cents":-1}`, handoff.RefundReview, "negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseProposal(json.RawMessage(tt.raw), tt.typ)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("err = %v, want valid", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("err = %v, want one mentioning %q", err, tt.wantErr)
			}
		})
	}
}

func TestProposalSchema_IsValidJSON(t *testing.T) {
	var v map[string]any
	if err := json.Unmarshal(ProposalSchema, &v); err != nil {
		t.Fatal(err)
	}
}
