package handoff

import "testing"

func TestCanonicalOrderNumber(t *testing.T) {
	for in, want := range map[string]string{
		"ORD-100115": "ORD-100115", "ord-100115": "ORD-100115", "ORD 100115": "ORD-100115",
		" ord100115 ": "ORD-100115", "ORD-10011": "ORD-10011", "": "", "order 5": "order 5",
	} {
		if got := CanonicalOrderNumber(in); got != want {
			t.Errorf("CanonicalOrderNumber(%q) = %q, want %q", in, got, want)
		}
	}
}
