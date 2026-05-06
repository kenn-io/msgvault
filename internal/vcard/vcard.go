// Package vcard parses vCard 2.1 and 3.0 contact files. Used to backfill
// participant display names for messaging sources whose underlying data
// (chat.db, WhatsApp msgstore.db, etc.) only stores phone/email handles.
package vcard

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Contact is a single parsed vCard entry.
type Contact struct {
	FullName string
	Phones   []string // normalized to E.164
	Emails   []string // lowercased
}

// ParseFile reads a .vcf file and returns parsed contacts. Handles vCard
// 2.1 and 3.0 formats, including RFC 2425 line folding and
// QUOTED-PRINTABLE encoded values.
func ParseFile(path string) ([]Contact, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var rawLines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
			if len(rawLines) > 0 {
				previous := rawLines[len(rawLines)-1]
				continuation := strings.TrimLeft(line, " \t")
				if strings.HasSuffix(previous, "=") && isQuotedPrintable(previous) {
					rawLines[len(rawLines)-1] = previous[:len(previous)-1] + continuation
				} else {
					rawLines[len(rawLines)-1] += continuation
				}
				continue
			}
		}
		rawLines = append(rawLines, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan vcard: %w", err)
	}

	// Handle QUOTED-PRINTABLE soft line breaks: trailing '=' continues on
	// the next line (vCard 2.1 convention). Scanner has already consumed
	// the newline, so we rejoin here. Gated on isQuotedPrintable: base64
	// PHOTO blobs commonly end with '=' padding and would otherwise
	// swallow the following END:VCARD line, dropping the contact.
	var qpJoined []string
	for i := 0; i < len(rawLines); i++ {
		line := rawLines[i]
		for strings.HasSuffix(line, "=") && isQuotedPrintable(line) && i+1 < len(rawLines) {
			line = line[:len(line)-1] + rawLines[i+1]
			i++
		}
		qpJoined = append(qpJoined, line)
	}
	rawLines = qpJoined

	var contacts []Contact
	var current *Contact

	for _, line := range rawLines {
		line = strings.TrimSpace(line)
		upper := strings.ToUpper(line)
		key := normalizedPropertyKey(line)

		switch {
		case upper == "BEGIN:VCARD":
			current = &Contact{}

		case upper == "END:VCARD":
			if current != nil && (current.FullName != "" || len(current.Phones) > 0 || len(current.Emails) > 0) {
				contacts = append(contacts, *current)
			}
			current = nil

		case current == nil:
			continue

		case key == "FN":
			name := extractValue(line)
			if isQuotedPrintable(line) {
				name = decodeQuotedPrintable(name)
			}
			if name != "" {
				current.FullName = name
			}

		case key == "TEL":
			phone := normalizePhone(extractValue(line))
			if phone != "" {
				current.Phones = append(current.Phones, phone)
			}

		case key == "EMAIL":
			email := strings.ToLower(strings.TrimSpace(extractValue(line)))
			if email != "" && strings.Contains(email, "@") {
				current.Emails = append(current.Emails, email)
			}
		}
	}

	return contacts, nil
}

// extractValue extracts the value part from a vCard line, handling both
// "KEY:value" and "KEY;params:value" formats.
func extractValue(line string) string {
	idx := strings.Index(line, ":")
	if idx < 0 {
		return ""
	}
	return strings.TrimSpace(line[idx+1:])
}

func normalizedPropertyKey(line string) string {
	end := strings.IndexAny(line, ":;")
	if end < 0 {
		return ""
	}
	key := line[:end]
	if dot := strings.LastIndex(key, "."); dot >= 0 {
		key = key[dot+1:]
	}
	return strings.ToUpper(key)
}

func isQuotedPrintable(line string) bool {
	upper := strings.ToUpper(line)
	return strings.Contains(upper, "ENCODING=QUOTED-PRINTABLE") ||
		strings.Contains(upper, ";QUOTED-PRINTABLE")
}

// decodeQuotedPrintable decodes =XX hex sequences (e.g., =C3=A9 → é).
func decodeQuotedPrintable(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '=' && i+2 < len(s) {
			hi := unhex(s[i+1])
			lo := unhex(s[i+2])
			if hi >= 0 && lo >= 0 {
				b.WriteByte(byte(hi<<4 | lo))
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func unhex(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'A' && c <= 'F':
		return int(c - 'A' + 10)
	case c >= 'a' && c <= 'f':
		return int(c - 'a' + 10)
	default:
		return -1
	}
}

var nonDigitRe = regexp.MustCompile(`[^\d]`)

// normalizePhone normalizes a vCard phone number to E.164. Handles formats
// like "+447...", "003-362-...", "+44 (0)7700 ...". Returns "" for
// country-ambiguous numbers (local "0…" or bare digits) rather than
// guessing — a wrong country prefix would match the wrong participant.
func normalizePhone(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	hasPlus := strings.HasPrefix(raw, "+")
	// Trunk prefix (0) common in UK/EU: "+44 (0)7700" means "+447700".
	raw = strings.ReplaceAll(raw, "(0)", "")
	digits := nonDigitRe.ReplaceAllString(raw, "")
	if digits == "" {
		return ""
	}
	if hasPlus {
		return "+" + digits
	}
	if strings.HasPrefix(digits, "00") && len(digits) > 4 {
		return "+" + digits[2:]
	}
	return ""
}
