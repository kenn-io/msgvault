package preprocess

// IMPORTANT: this file is part of Preprocess(). The preprocessVersion rule
// at the top of preprocess.go applies here too.

import "strings"

// Bare base64 payloads (no `data:` prefix) that leaked into body_text are
// removed in two passes. The split exists because '/' is in both the base64
// alphabet AND in every URL path, so one length threshold cannot both keep
// URL paths and catch real base64 with slashes:
//
//   - minBase64Run (no '/', 200+ chars). Catches dense letter+digit runs
//     that prose never produces. Every '/' resets the run, so URL paths
//     between separators are never matched as a whole.
//   - minBase64RunWithSlash (with '/', 300+ chars). Catches real base64
//     binary residue where '/' appears at the alphabet's natural ~1/64
//     frequency. Even long signed S3 / CloudFront URLs and GitHub blob
//     paths rarely reach 300 unbroken base64-alphabet chars without a `.`,
//     `?`, `&`, `_`, `-`, or `~` breaking the run, while inline images or
//     PDFs routinely produce thousands of chars.
const (
	minBase64Run          = 200
	minBase64RunWithSlash = 300
)

// containsDataScheme reports whether s contains "data:" in any ASCII case.
// It is a cheap prefilter for reDataURI: unlike other letters in that
// expression, the letters in "data:" have no non-ASCII case-fold equivalents.
func containsDataScheme(s string) bool {
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
