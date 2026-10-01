package triage

import (
	"regexp"
	"strings"
	"unicode"
)

// foldForMatch undoes cheap evasions of the high-risk regex before it runs:
//
//   - zero-width and soft-hyphen characters are dropped ("law​yer");
//   - fullwidth ASCII becomes ASCII ("ｌａｗｙｅｒ");
//   - Cyrillic and Greek letters that look like Latin ones become Latin
//     ("lаwyer" with a Cyrillic а);
//   - a few symbol and digit substitutions become letters ("ch@rgeback",
//     "fr0ud", "$ue", "su3");
//   - runs of four or more single letters split by spaces, dots, dashes,
//     underscores or asterisks are joined ("l a w y e r"); a run of exactly
//     three is joined only when it spells a short high-risk term ("s u e",
//     "s.u.e"), since short runs are ordinary text ("a b c").
//
// Out of scope, on purpose: "1" (it could be l or i), words split in two
// ("law yer"), a short term inside a longer run ("a s u e"), homoglyphs
// outside the table, and misspellings. The pre-check
// is a cheap first filter; the gate's draft rules, autonomy levels and human
// review are the rest of the defense. Folding only ever adds matches, so it
// can only make the outcome more cautious.
func foldForMatch(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case zeroWidth[r]:
			return -1
		case r >= 0xFF01 && r <= 0xFF5E: // fullwidth ASCII block
			r -= 0xFEE0
		}
		if l, ok := confusables[unicode.ToLower(r)]; ok {
			return l
		}
		return r
	}, s)
	s = spacedOut.ReplaceAllStringFunc(s, lettersOnly)
	return spacedThree.ReplaceAllStringFunc(s, func(m string) string {
		if w := lettersOnly(m); shortRiskTerms[strings.ToLower(w)] {
			return w
		}
		return m
	})
}

func lettersOnly(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) {
			return r
		}
		return -1
	}, s)
}

var zeroWidth = map[rune]bool{
	0x00AD: true, 0x200B: true, 0x200C: true, 0x200D: true, 0x2060: true, 0xFEFF: true,
}

// confusables maps look-alikes to the lowercase Latin letter (the regex is
// case-insensitive, so case does not matter).
var confusables = map[rune]rune{
	// Cyrillic
	'а': 'a', 'в': 'b', 'е': 'e', 'ё': 'e', 'і': 'i', 'ј': 'j', 'к': 'k', 'м': 'm', 'н': 'h',
	'о': 'o', 'р': 'p', 'с': 'c', 'т': 't', 'у': 'y', 'х': 'x', 'ѕ': 's', 'ԁ': 'd', 'ԛ': 'q', 'ԝ': 'w',
	// Greek
	'α': 'a', 'ε': 'e', 'ι': 'i', 'κ': 'k', 'ν': 'v', 'ο': 'o', 'ρ': 'p', 'τ': 't', 'υ': 'u', 'χ': 'x',
	// Symbols and digits
	'@': 'a', '$': 's', '0': 'o', '3': 'e', '5': 's',
}

// spacedOut is four or more single letters, each followed by separators.
var spacedOut = regexp.MustCompile(`\b(?:\pL[ .\-_*]+){3,}\pL\b`)

// spacedThree is exactly three single letters split by separators; it runs
// after spacedOut has joined the longer runs.
var spacedThree = regexp.MustCompile(`\b\pL[ .\-_*]+\pL[ .\-_*]+\pL\b`)

// shortRiskTerms are the three-letter terms of highRisk.
var shortRiskTerms = map[string]bool{"sue": true}
