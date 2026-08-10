// Package textutil provides text manipulation and encoding utilities.
package textutil

import (
	"strings"
	"unicode/utf8"

	"github.com/gogs/chardet"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
)

// EnsureUTF8 ensures a string is valid UTF-8.
// If already valid UTF-8, returns as-is.
// Otherwise attempts charset detection and conversion.
// Falls back to replacing invalid bytes with replacement character.
func EnsureUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}

	// Try charset detection and conversion
	data := []byte(s)

	// Try automatic charset detection (works better on longer samples,
	// but we try it even for short strings with lower confidence threshold)
	minConfidence := 30 // Lower threshold for shorter strings
	if len(data) > 50 {
		minConfidence = 50 // Higher threshold for longer strings
	}

	detector := chardet.NewTextDetector()
	result, err := detector.DetectBest(data)
	if err == nil && result.Confidence >= minConfidence {
		if enc := GetEncodingByName(result.Charset); enc != nil {
			decoded, err := enc.NewDecoder().Bytes(data)
			if err == nil && utf8.Valid(decoded) {
				return string(decoded)
			}
		}
	}

	// Try common encodings in order of likelihood for email content.
	// Single-byte encodings first (Windows-1252/Latin-1 are most common in Western emails),
	// then multi-byte Asian encodings.
	encodings := []encoding.Encoding{
		charmap.Windows1252,     // Smart quotes, dashes common in Windows emails
		charmap.ISO8859_1,       // Latin-1 (Western European)
		charmap.ISO8859_15,      // Latin-9 (Western European with Euro)
		japanese.ShiftJIS,       // Japanese
		japanese.EUCJP,          // Japanese
		korean.EUCKR,            // Korean
		simplifiedchinese.GBK,   // Simplified Chinese
		traditionalchinese.Big5, // Traditional Chinese
	}

	for _, enc := range encodings {
		decoded, err := enc.NewDecoder().Bytes(data)
		if err == nil && utf8.Valid(decoded) {
			return string(decoded)
		}
	}

	// Last resort: replace invalid bytes
	return SanitizeUTF8(s)
}

// SanitizeUTF8 replaces invalid UTF-8 bytes with replacement character.
func SanitizeUTF8(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			sb.WriteRune('\ufffd')
			i++
		} else {
			sb.WriteRune(r)
			i += size
		}
	}
	return sb.String()
}

// GetEncodingByName returns an encoding for the given IANA charset name.
func GetEncodingByName(name string) encoding.Encoding {
	switch name {
	case "windows-1252", "CP1252", "cp1252":
		return charmap.Windows1252
	case "ISO-8859-1", "iso-8859-1", "latin1", "latin-1":
		return charmap.ISO8859_1
	case "ISO-8859-15", "iso-8859-15", "latin9":
		return charmap.ISO8859_15
	case "ISO-8859-2", "iso-8859-2", "latin2":
		return charmap.ISO8859_2
	case "Shift_JIS", "shift_jis", "shift-jis", "sjis":
		return japanese.ShiftJIS
	case "EUC-JP", "euc-jp", "eucjp":
		return japanese.EUCJP
	case "ISO-2022-JP", "iso-2022-jp":
		return japanese.ISO2022JP
	case "EUC-KR", "euc-kr", "euckr":
		return korean.EUCKR
	case "GB2312", "gb2312", "GBK", "gbk":
		return simplifiedchinese.GBK
	case "GB18030", "gb18030":
		return simplifiedchinese.GB18030
	case "Big5", "big5", "big-5":
		return traditionalchinese.Big5
	case "KOI8-R", "koi8-r":
		return charmap.KOI8R
	case "KOI8-U", "koi8-u":
		return charmap.KOI8U
	default:
		return nil
	}
}

// TruncateRunes truncates a string to maxRunes runes (not bytes), adding "..." if truncated.
// This is UTF-8 safe and won't split multi-byte characters.
func TruncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	if maxRunes <= 3 {
		return string(runes[:maxRunes])
	}
	return string(runes[:maxRunes-3]) + "..."
}

// FirstLine returns the first line of a string, capped at 200 runes.
// Useful for extracting clean error messages from multi-line outputs
// where the first line itself may be excessively long (e.g. enmime
// includes malformed MIME content in its error messages).
func FirstLine(s string) string {
	s = strings.TrimLeft(s, "\r\n")
	if idx := strings.Index(s, "\n"); idx >= 0 {
		s = s[:idx]
	}
	return TruncateRunes(s, 200)
}

// SanitizeTerminal strips ANSI escape sequences and C0/C1 control characters
// from a string, preventing terminal injection via untrusted data (e.g.,
// WhatsApp chat names, message snippets). Preserves printable characters
// and tabs. Replaces \r and \n with spaces to prevent line overwrite/break
// attacks in single-line contexts (TUI rows, progress output).
//
// C1 control characters (U+0080–U+009F) are checked on the decoded rune, not
// the raw leading byte, so that UTF-8 encoded C1 chars (e.g., U+009B CSI
// encoded as 0xC2 0x9B) are correctly stripped.
func SanitizeTerminal(s string) string {
	return sanitizeTerminal(s, false)
}

// SanitizeTerminalMultiline strips ANSI escape sequences and C0/C1 control
// characters while preserving line feeds. Carriage returns are discarded so
// untrusted text cannot overwrite terminal output, and CRLF is normalized to
// LF. Use SanitizeTerminal for single-line terminal output.
func SanitizeTerminalMultiline(s string) string {
	return sanitizeTerminal(s, true)
}

func sanitizeTerminal(s string, multiline bool) string {
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		c := s[i]
		// Strip ESC-initiated sequences (CSI, OSC, etc.).
		if c == 0x1b && i+1 < len(s) {
			next := s[i+1]
			switch next {
			case '[': // CSI sequence: ESC [ ... <final byte 0x40-0x7E>
				i += 2
				for i < len(s) && (s[i] < 0x40 || s[i] > 0x7E) {
					i++
				}
				if i < len(s) {
					i++ // skip final byte
				}
				continue
			case ']': // OSC sequence: ESC ] ... (ST or BEL)
				i += 2
				for i < len(s) {
					if s[i] == 0x07 { // BEL terminates OSC
						i++
						break
					}
					if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' { // ST terminates OSC
						i += 2
						break
					}
					i++
				}
				continue
			default: // Other ESC sequences (2-byte): skip both
				i += 2
				continue
			}
		}

		// Decode the full rune so we can check C1 control characters that
		// span multiple bytes in UTF-8 (e.g., U+009B is 0xC2 0x9B).
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			// Invalid UTF-8 byte — skip it.
			i++
			continue
		}

		// Allow tab. Multiline output keeps LF but always discards CR so an
		// untrusted value cannot return the cursor to the start of a line.
		if r == '\t' {
			b.WriteRune(r)
			i += size
			continue
		}
		if r == '\n' {
			if multiline {
				b.WriteByte('\n')
			} else {
				b.WriteByte(' ')
			}
			i += size
			continue
		}
		if r == '\r' {
			if !multiline {
				// Replace with space to preserve word boundaries.
				b.WriteByte(' ')
			}
			i += size
			continue
		}
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			i += size
			continue
		}

		b.WriteString(s[i : i+size])
		i += size
	}
	return b.String()
}
