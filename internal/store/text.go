package store

import "unicode/utf8"

// truncateBytesRuneSafe returns s truncated to at most maxBytes bytes
// without splitting a multibyte UTF-8 rune.
func truncateBytesRuneSafe(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
