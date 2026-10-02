package preprocess

import "strings"

// The letters in "data:" have only ASCII case-fold equivalents, even though
// the rest of the legacy data-URI expression accepts some non-ASCII letters.
func hasDataURIPrefix(s string) bool {
	for i := 0; i+5 <= len(s); i++ {
		if s[i]|0x20 == 'd' && s[i+1]|0x20 == 'a' &&
			s[i+2]|0x20 == 't' && s[i+3]|0x20 == 'a' && s[i+4] == ':' {
			return true
		}
	}
	return false
}

// stripBase64Runs replaces each qualifying maximal ASCII base64 run with
// one space, consuming up to two trailing padding bytes. Keep the slash-free
// and slash-inclusive passes separate: the first can break a run that would
// otherwise qualify in the second.
//
// A counted regexp repetition expands to hundreds of states, imposing a
// large per-character cost even on ordinary prose that never matches. Scan
// each byte once instead and allocate only when a run is actually removed.
func stripBase64Runs(s string, minRun int, allowSlash bool) string {
	var out strings.Builder
	copied := 0
	for i := 0; i < len(s); {
		if !base64Byte(s[i], allowSlash) {
			i++
			continue
		}
		start := i
		for i < len(s) && base64Byte(s[i], allowSlash) {
			i++
		}
		if i-start < minRun {
			continue
		}
		for padding := 0; padding < 2 && i < len(s) && s[i] == '='; padding++ {
			i++
		}
		out.WriteString(s[copied:start])
		out.WriteByte(' ')
		copied = i
	}
	if copied == 0 {
		return s
	}
	out.WriteString(s[copied:])
	return out.String()
}

func base64Byte(c byte, allowSlash bool) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' ||
		c >= '0' && c <= '9' || c == '+' || allowSlash && c == '/'
}
