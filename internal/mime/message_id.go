package mime

import (
	"strings"
	"unicode/utf8"
)

// NormalizeMessageID returns the canonical bare form used for RFC822
// Message-ID comparison and storage, with invalid bytes made safe for SQL TEXT.
func NormalizeMessageID(id string) string {
	id = strings.TrimSpace(id)
	id = strings.Trim(id, "<>")
	var normalized strings.Builder
	normalized.Grow(len(id))
	for len(id) > 0 {
		r, size := utf8.DecodeRuneInString(id)
		if r == utf8.RuneError && size == 1 {
			normalized.WriteRune(utf8.RuneError)
			id = id[1:]
			continue
		}
		normalized.WriteRune(r)
		id = id[size:]
	}
	return normalized.String()
}
