package wordfilter

import (
	"strings"

	"golang.org/x/text/unicode/norm"
)

// Normalize folds text into a canonical form for matching:
// NFKC, lower case, katakana to hiragana, zero-width characters removed.
func Normalize(s string) string {
	s = strings.ToLower(norm.NFKC.String(s))
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 0x200B && r <= 0x200D, r == 0x2060, r == 0xFEFF:
			continue
		case r >= 0x30A1 && r <= 0x30F6:
			r -= 0x60
		}
		b.WriteRune(r)
	}
	return b.String()
}
