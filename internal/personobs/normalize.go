package personobs

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

var placeholderPattern = regexp.MustCompile(`(?i)\[(person|owner)\]`)

func CanonicalizePlaceholders(s string) string {
	return placeholderPattern.ReplaceAllStringFunc(s, strings.ToUpper)
}
func NormalizeContent(s string) string {
	s = cases.Fold().String(norm.NFKC.String(s))
	return strings.TrimRightFunc(strings.Join(strings.Fields(s), " "), func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(".,;:!?", r)
	})
}
func ContentKey(level Level, subjectRef, content string) string {
	sum := sha256.Sum256([]byte(NormalizationVersion + "\x00" + string(level) + "\x00" + NormalizeContent(subjectRef) + "\x00" + NormalizeContent(CanonicalizePlaceholders(content))))
	return hex.EncodeToString(sum[:])
}
