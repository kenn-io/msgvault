package mime

import "regexp"

// This obsolete generator inserts a fixed, undeclared delimiter at the start
// of a CSV part's headers. Restrict recovery to that complete signature, after
// ordinary parsing failed; do not reinterpret arbitrary malformed messages.
var legacyCSVHeader = regexp.MustCompile(`(?m)^(--[^\r\n]{1,200}\r?\n)[ \t]*--------------040908010102080206010104[ \t]*\r?\n(Content-Type: text/csv;[^\r\n]*\r?\nContent-Transfer-Encoding: quoted-printable\r?\nContent-Disposition: attachment;[^\r\n]*\r?\n)`)

func recoverLegacyCSV(raw []byte) *Message {
	match := legacyCSVHeader.FindSubmatchIndex(raw)
	if match == nil {
		return nil
	}
	// Remove only the first signature's stray line on a copy. Later matches
	// may be text inside an attachment payload.
	fixed := append([]byte(nil), raw[:match[3]]...)
	fixed = append(fixed, raw[match[4]:]...)
	recovered, err := parse(fixed, nil)
	if err != nil {
		return nil
	}
	return recovered
}
