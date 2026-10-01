package triage

import (
	"strings"
	"testing"
)

func TestParseDecision(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr string // substring; empty means valid
	}{
		{
			name: "valid auto reply",
			raw:  `{"category":"order_status","action":"auto_reply","draft_reply":"It shipped.","confidence":0.9,"reason":"Tracking available.","follow_ups":[]}`,
		},
		{
			name: "escalate without draft is fine",
			raw:  `{"category":"refund_request","action":"escalate","draft_reply":"","confidence":0.8,"reason":"Above refund limit.","follow_ups":[]}`,
		},
		{
			name:    "unknown category",
			raw:     `{"category":"billing","action":"auto_reply","draft_reply":"x","confidence":0.9,"reason":"r"}`,
			wantErr: `category "billing"`,
		},
		{
			name:    "reply required unless escalating",
			raw:     `{"category":"general","action":"draft_for_review","draft_reply":"  ","confidence":0.5,"reason":"r"}`,
			wantErr: "draft_reply is required",
		},
		{
			name:    "confidence out of range",
			raw:     `{"category":"general","action":"auto_reply","draft_reply":"x","confidence":7,"reason":"r"}`,
			wantErr: "between 0 and 1",
		},
		{
			name:    "typo in field name is rejected, not ignored",
			raw:     `{"category":"general","action":"auto_reply","draft_reply":"x","confidnce":0.9,"reason":"r"}`,
			wantErr: `unknown field "confidnce"`,
		},
		{
			name:    "all problems reported at once",
			raw:     `{"category":"nope","action":"nope","draft_reply":"","confidence":2,"reason":""}`,
			wantErr: "reason is required",
		},
		{
			name: "valid follow-ups",
			raw:  `{"category":"damaged_or_misprint","action":"draft_for_review","draft_reply":"x","confidence":0.9,"reason":"r","follow_ups":[{"type":"reprint_request","reason":"damaged","order_number":"ORD-100105"},{"type":"refund_review","reason":"misprint","order_number":"ORD-100105"}]}`,
		},
		{
			name:    "follow-up reason is required and closed",
			raw:     `{"category":"general","action":"draft_for_review","draft_reply":"x","confidence":0.9,"reason":"r","follow_ups":[{"type":"reprint_request","reason":"vip","order_number":"ORD-100105"}]}`,
			wantErr: `follow_ups[0].reason "vip"`,
		},
		{
			name:    "follow_ups is required, even when empty",
			raw:     `{"category":"general","action":"auto_reply","draft_reply":"x","confidence":0.9,"reason":"r"}`,
			wantErr: "follow_ups is required",
		},
		{
			name:    "null follow_ups counts as missing",
			raw:     `{"category":"general","action":"auto_reply","draft_reply":"x","confidence":0.9,"reason":"r","follow_ups":null}`,
			wantErr: "follow_ups is required",
		},
		{
			name:    "unknown follow-up type",
			raw:     `{"category":"general","action":"draft_for_review","draft_reply":"x","confidence":0.9,"reason":"r","follow_ups":[{"type":"issue_refund","order_number":"ORD-100105"}]}`,
			wantErr: `follow_ups[0].type "issue_refund"`,
		},
		{
			name:    "malformed order number",
			raw:     `{"category":"general","action":"draft_for_review","draft_reply":"x","confidence":0.9,"reason":"r","follow_ups":[{"type":"reprint_request","order_number":"ORD-10105"}]}`,
			wantErr: `follow_ups[0].order_number "ORD-10105"`,
		},
		{
			name:    "repeated follow-up type",
			raw:     `{"category":"general","action":"draft_for_review","draft_reply":"x","confidence":0.9,"reason":"r","follow_ups":[{"type":"reprint_request","order_number":"ORD-100105"},{"type":"reprint_request","order_number":"ORD-100106"}]}`,
			wantErr: `follow_ups[1].type "reprint_request" is repeated`,
		},
		{
			name:    "escalation carries no follow-ups",
			raw:     `{"category":"refund_request","action":"escalate","draft_reply":"","confidence":0.9,"reason":"r","follow_ups":[{"type":"refund_review","order_number":"ORD-100108"}]}`,
			wantErr: "follow_ups must be [] when action is escalate",
		},
		{
			name:    "unknown field inside a follow-up is rejected",
			raw:     `{"category":"general","action":"draft_for_review","draft_reply":"x","confidence":0.9,"reason":"r","follow_ups":[{"type":"reprint_request","order_number":"ORD-100105","email":"x@y.z"}]}`,
			wantErr: `unknown field "email"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseDecision([]byte(tt.raw))
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}
