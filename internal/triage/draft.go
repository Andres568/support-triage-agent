package triage

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/Andres568/support-triage-agent/internal/handoff"
)

// MaxDraftRunes caps a reply that may go out without a human. Real answers
// to these tickets are a few sentences; a wall of text is a model that was
// steered into writing something else.
const MaxDraftRunes = 2000

var (
	// linkRE is anything a reader could follow: a scheme, "www." or a bare
	// domain with a common TLD, or a bare IPv4 address. Replies never need one, and a link is the
	// usual payload of an injected instruction (phishing, a fake portal).
	linkRE = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://|\bwww\.|\b[a-z0-9-]+\.(com|net|org|io|co|app|info|biz|xyz|link|ru|me|ly)\b|\b\d{1,3}(?:\.\d{1,3}){3}\b`)
	// defang undoes the usual ways of writing a dot so a link survives
	// being typed out ("evil[.]com", "evil dot com").
	defang  = strings.NewReplacer("[.]", ".", "(.)", ".", "[dot]", ".", "(dot)", ".", " dot ", ".")
	emailRE = regexp.MustCompile(`[^\s@]+@[^\s@]+\.[^\s@]+`)
	orderRE = regexp.MustCompile(`(?i)\bORD-\d{6}\b`)

	// promiseRE is a commitment of money or work in our name. Only a person
	// may make one; the orders agent proposes, and a person executes. It
	// matches a first-person or company subject within a few words of a
	// money or remedy word ("we will reship it for free", "I have refunded
	// you", "we'll give you a 20% discount"), the no-cost idioms on their
	// own, "we guarantee", and a remedy reported as done. Informational text
	// has no such subject ("our refund policy allows", "tracking 9400…") and
	// passes. Over-inclusive on purpose: "we can't refund" or "I see your
	// credit card" also match, and only cost a human review.
	promiseRE = regexp.MustCompile(`(?i)\b(?:we|i|our team)(?:\W+\w+){0,5}?\W+(?:refund\w*|re-?ship\w*|replace\w*|reprint\w*|discount\w*|credit\w*|coupon\w*|voucher\w*|compensat\w*|free|no cost|no charge|on us)\b` +
		`|\b(?:free of charge|at no (?:extra |additional )?(?:cost|charge))\b` +
		`|\bwe guarantee\b` +
		`|\b(?:refund|replacement|reprint|credit)s? (?:has|have|was|were|will be) (?:been )?(?:issued|sent|processed|approved)\b`)
)

// draftProblems lists why the draft may not be sent without a human. The
// draft is model output, which the customer's text can steer, so these are
// checks on content we would publish in our name (CWE-1426). An order
// number is fine if the sender owns it or wrote it: echoing the customer's
// own text back reveals nothing.
func draftProblems(draft string, t Ticket, f Facts) []string {
	var out []string
	if linkRE.MatchString(defang.Replace(strings.ToLower(draft))) {
		out = append(out, "draft contains a link")
	}
	if emailRE.MatchString(draft) {
		out = append(out, "draft contains an email address")
	}
	for _, m := range orderRE.FindAllString(draft, -1) {
		if _, ok := f.Orders[strings.ToUpper(m)]; !ok && !customerWrote(t, m) {
			out = append(out, fmt.Sprintf("draft mentions order %s, not verified for sender", strings.ToUpper(m)))
			break
		}
	}
	lower := strings.ReplaceAll(strings.ToLower(draft), "’", "'")
	if m := promiseRE.FindString(lower); m != "" {
		out = append(out, fmt.Sprintf("draft promises %q", m))
	}
	if n := utf8.RuneCountInString(draft); n > MaxDraftRunes {
		out = append(out, fmt.Sprintf("draft is %d characters, over %d", n, MaxDraftRunes))
	}
	return out
}

// customerWrote reports whether the ticket itself contains the order number.
func customerWrote(t Ticket, num string) bool {
	num = strings.ToUpper(num)
	if handoff.CanonicalOrderNumber(t.OrderNumber) == num {
		return true
	}
	return slices.ContainsFunc(orderRE.FindAllString(t.Subject+"\n"+t.Body, -1), func(m string) bool { return strings.EqualFold(m, num) })
}
