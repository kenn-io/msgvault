package mime

import "regexp"

// This obsolete generator inserts a fixed, undeclared delimiter at the start
// of a CSV part's headers. Restrict recovery to that complete signature, after
// ordinary parsing failed; do not reinterpret arbitrary malformed messages.
var legacyCSVHeader = regexp.MustCompile(`(?m)^(--[^\r\n]{1,200}\r?\n)[ \t]*--------------040908010102080206010104[ \t]*\r?\n(Content-Type: text/csv;[^\r\n]*\r?\nContent-Transfer-Encoding: quoted-printable\r?\nContent-Disposition: attachment;[^\r\n]*\r?\n)`)

func recoverLegacyCSV(raw []byte) *Message {
	if !legacyCSVHeader.Match(raw) {
		return nil
	}
	fixed := legacyCSVHeader.ReplaceAll(raw, []byte("${1}${2}"))
	recovered, err := parse(fixed, nil)
	if err != nil {
		return nil
	}
	return recovered
}
