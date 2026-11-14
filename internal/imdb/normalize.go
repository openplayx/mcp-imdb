package imdb

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Normalize folds a title or name for matching: NFKD, diacritics removed,
// lower case, "&" → "and", every run of non-letters/digits → one space.
// CJK text keeps its characters, so "霸王别姬" stays "霸王别姬".
func Normalize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range norm.NFKD.String(s) {
		switch {
		case unicode.Is(unicode.Mn, r):
			continue
		case r == '&':
			if b.Len() > 0 && !space {
				b.WriteByte(' ')
			}
			b.WriteString("and ")
			space = true
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
			space = false
		default:
			if b.Len() > 0 && !space {
				b.WriteByte(' ')
				space = true
			}
		}
	}
	return strings.TrimRight(b.String(), " ")
}

// ParseID turns "tt0111161" / "nm0000209" into its number. prefix is "tt" or "nm".
func ParseID(prefix, s string) (int64, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(s), prefix)
	if !ok || len(rest) < 1 || len(rest) > 10 {
		return 0, false
	}
	n, err := strconv.ParseInt(rest, 10, 64)
	return n, err == nil && n > 0
}

// FormatID is the inverse of ParseID: at least 7 digits, zero padded.
func FormatID(prefix string, n int64) string { return fmt.Sprintf("%s%07d", prefix, n) }
