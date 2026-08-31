package mime

import (
	"strings"
	"unicode/utf8"
)

// NormalizeMessageID returns the canonical form used for RFC822 Message-ID
// comparison and storage. It unwraps one structurally valid angle-bracket pair,
// preserves malformed bracket structure, and makes invalid bytes safe for SQL
// TEXT.
func NormalizeMessageID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) > 2 && id[0] == '<' && id[len(id)-1] == '>' &&
		!isMessageIDBracketEdge(id[1]) && !isMessageIDBracketEdge(id[len(id)-2]) {
		id = id[1 : len(id)-1]
	}
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

func isMessageIDBracketEdge(b byte) bool {
	return b == '<' || b == '>' || b == ' '
}
