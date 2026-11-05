package mime

import "regexp"

// This obsolete generator inserts a fixed, undeclared delimiter at the start
// of a CSV part's headers. Restrict recovery to that complete signature, after
// ordinary parsing failed; do not reinterpret arbitrary malformed messages.
var legacyCSVHeader = regexp.MustCompile(`(?m)^(--[^\r\n]{1,200}\r?\n)[ \t]*--------------040908010102080206010104[ \t]*\r?\n(Content-Type: text/csv;[^\r\n]*\r?\nContent-Transfer-Encoding: quoted-printable\r?\nContent-Disposition: attachment;[^\r\n]*\r?\n)`)

func recoverLegacyCSV(raw []byte) *Message {
	for _, match := range legacyCSVHeader.FindAllSubmatchIndex(raw, -1) {
		// Each attempt removes one stray line from a fresh copy. Other
		// matches may be quoted text in a body or attachment payload.
		fixed := append([]byte(nil), raw[:match[3]]...)
		fixed = append(fixed, raw[match[4]:]...)
		if recovered, err := parse(fixed, nil); err == nil {
			for _, attachment := range recovered.Attachments {
				if attachment.ContentType == "text/csv" && len(attachment.Content) > 0 {
					return recovered
				}
			}
		}
	}
	return nil
}
