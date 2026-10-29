package store

import (
	"strings"
	"unicode/utf8"
)

// SnippetFromBody renders a message body as the messages snippet column
// value: surrounding whitespace trimmed and at most 200 runes. Importers
// filling Message.Snippet call this one helper so every source formats
// snippets alike.
func SnippetFromBody(body string) string {
	s := strings.TrimSpace(body)
	if utf8.RuneCountInString(s) > 200 {
		s = string([]rune(s)[:200])
	}
	return s
}
