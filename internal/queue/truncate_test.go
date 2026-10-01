package queue

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateError_RuneSafeAndBounded(t *testing.T) {
	if got := truncateError("short"); got != "short" {
		t.Errorf("short = %q", got)
	}
	for _, msg := range []string{strings.Repeat("x", 10_000), strings.Repeat("é", 5_000), strings.Repeat("🙂", 3_000)} {
		got := truncateError(msg)
		if len(got) > MaxLastErrorBytes || !utf8.ValidString(got) || !strings.HasSuffix(got, truncatedSuffix) {
			t.Errorf("truncateError(%.8q…) = %d bytes, valid=%v", msg, len(got), utf8.ValidString(got))
		}
	}
}
