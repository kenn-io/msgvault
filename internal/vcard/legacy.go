// Package vcard parses and renders vCard documents and projects them into the
// legacy contact shape used by message importers.
package vcard

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/textimport"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/ianaindex"
)

// Contact is a single parsed vCard entry.
type Contact struct {
	FullName string
	Phones   []string // normalized to E.164
	Emails   []string // lowercased
}

// ParseFileOptions controls optional vCard projection behavior.
// NormalizePhone receives the decoded phone candidate after tel URI and
// phone-context assembly. A nil callback uses the package normalizer.
type ParseFileOptions struct {
	NormalizePhone func(string) string
}

// ParseFile reads a vCard file and projects its contact identity fields.
func ParseFile(path string) ([]Contact, error) {
	return ParseFileWithOptions(path, ParseFileOptions{})
}

// ParseFileWithOptions reads a vCard file and projects its contact identity
// fields with optional phone normalization.
func ParseFileWithOptions(path string, options ParseFileOptions) ([]Contact, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open vCard %q: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	document, err := Decode(file)
	if err != nil {
		return nil, fmt.Errorf("decode vCard %q: %w", path, err)
	}

	normalize := options.NormalizePhone
	if normalize == nil {
		normalize = normalizePhone
	}

	contacts := make([]Contact, 0, len(document.Cards))
	for _, card := range document.Cards {
		var contact Contact
		for _, property := range card.Properties {
			switch {
			case strings.EqualFold(property.Name, "FN"):
				name, decodeErr := decodeLegacyText(property)
				if decodeErr != nil {
					return nil, fmt.Errorf("decode FN in %q: %w", path, decodeErr)
				}
				name = strings.TrimSpace(name)
				if name != "" {
					contact.FullName = name
				}
			case strings.EqualFold(property.Name, "TEL"):
				phone, decodeErr := decodeLegacyText(property)
				if decodeErr != nil {
					return nil, fmt.Errorf("decode TEL in %q: %w", path, decodeErr)
				}
				if normalized := normalizeLegacyPhone(strings.TrimSpace(phone), normalize); normalized != "" {
					contact.Phones = append(contact.Phones, normalized)
				}
			case strings.EqualFold(property.Name, "EMAIL"):
				email, decodeErr := decodeLegacyText(property)
				if decodeErr != nil {
					return nil, fmt.Errorf("decode EMAIL in %q: %w", path, decodeErr)
				}
				email = strings.ToLower(strings.TrimSpace(email))
				if email != "" && strings.Contains(email, "@") {
					contact.Emails = append(contact.Emails, email)
				}
			}
		}
		if contact.FullName != "" || len(contact.Phones) > 0 || len(contact.Emails) > 0 {
			contacts = append(contacts, contact)
		}
	}
	return contacts, nil
}

func decodeLegacyText(property Property) (string, error) {
	raw, err := decodeLegacyRawValue(property)
	if err != nil {
		return "", err
	}
	return UnescapeText(raw)
}

// decodeLegacyRawValue performs the legacy transfer and character-set
// decoding while preserving vCard syntax escapes and structured delimiters.
// Callers that need a TEXT value can apply UnescapeText afterwards; canonical
// v4 rendering must keep those delimiters in values such as N and ADR.
func decodeLegacyRawValue(property Property) (string, error) {
	raw, quotedPrintable, err := decodeLegacyTransfer(property)
	if err != nil {
		return "", err
	}
	if quotedPrintable {
		raw = escapeDecodedLineBreaks(raw)
	}
	return raw, nil
}

// decodeLegacyTransfer undoes the quoted-printable transfer encoding and the
// declared charset of a legacy value and reports whether it was
// quoted-printable. Line breaks are left as decoded: a quoted-printable body
// in a multibyte charset (UTF-16) carries CR and LF bytes inside its code
// units, and rewriting them before the charset decode corrupts it, so callers
// escape them only once the value is UTF-8.
func decodeLegacyTransfer(property Property) (string, bool, error) {
	raw := property.RawValue
	quotedPrintable := propertyIsQuotedPrintable(property)
	if quotedPrintable {
		decoded, err := DecodeQuotedPrintable(raw)
		if err != nil {
			return "", false, err
		}
		raw = decoded
	}
	charset := propertyCharset(property)
	if charset != "" && !strings.EqualFold(charset, "UTF-8") && !strings.EqualFold(charset, "UTF8") {
		encoding, err := ianaindex.MIME.Encoding(charset)
		if err != nil {
			return "", false, fmt.Errorf("resolve charset %q: %w", charset, err)
		}
		if encoding == nil {
			return "", false, fmt.Errorf("charset %q is not supported", charset)
		}
		decoded, err := encoding.NewDecoder().String(raw)
		if err != nil {
			return "", false, fmt.Errorf("decode charset %q: %w", charset, err)
		}
		raw = decoded
	}
	if !utf8.ValidString(raw) {
		if charset != "" {
			return "", false, errors.New("decoded text is not valid UTF-8")
		}
		// vCard 2.1 leaves the charset of undeclared text to the producer, and
		// ISO-8859-1 is what those producers wrote in practice. Every byte
		// sequence decodes under it, so undeclared non-UTF-8 text is read as
		// Latin-1 rather than refused, and the card stays canonicalizable.
		decoded, err := charmap.ISO8859_1.NewDecoder().String(raw)
		if err != nil {
			return "", false, fmt.Errorf("decode undeclared legacy text as ISO-8859-1: %w", err)
		}
		raw = decoded
	}
	return raw, quotedPrintable, nil
}

// escapeDecodedLineBreaks turns the literal line breaks a quoted-printable
// value may carry (=0D=0A in vCard 2.1 NOTE and ADR values) into the vCard
// text escape, which is the only form a content line can hold them in. Only
// line breaks are touched: structured delimiters and existing escapes pass
// through untouched.
func escapeDecodedLineBreaks(value string) string {
	if !strings.ContainsAny(value, "\r\n") {
		return value
	}
	replacer := strings.NewReplacer("\r\n", "\\n", "\r", "\\n", "\n", "\\n")
	return replacer.Replace(value)
}

func propertyCharset(property Property) string {
	for _, parameter := range property.ParametersNamed("CHARSET") {
		if len(parameter.Values) > 0 {
			return strings.TrimSpace(parameter.Values[0].Decoded)
		}
	}
	return ""
}

func propertyIsQuotedPrintable(property Property) bool {
	for _, parameter := range property.Parameters {
		for _, value := range parameter.Values {
			if !strings.EqualFold(value.Decoded, "QUOTED-PRINTABLE") {
				continue
			}
			if strings.EqualFold(parameter.Name, "ENCODING") ||
				parameter.Bare {
				return true
			}
		}
	}
	return false
}

// normalizePhone normalizes a vCard phone number through the same path used by
// message imports, keeping the lookup keys symmetric across sources.
func normalizePhone(raw string) string {
	normalized, err := textimport.NormalizePhone(raw)
	if err != nil {
		return ""
	}
	return normalized
}

func normalizeLegacyPhone(raw string, normalize func(string) string) string {
	if len(raw) < len("tel:") || !strings.EqualFold(raw[:len("tel:")], "tel:") {
		return normalize(raw)
	}

	parts := strings.Split(raw[len("tel:"):], ";")
	subscriber, err := url.PathUnescape(parts[0])
	if err != nil {
		return ""
	}
	if strings.HasPrefix(subscriber, "+") {
		return normalize(subscriber)
	}

	for _, part := range parts[1:] {
		name, value, found := strings.Cut(part, "=")
		if !found || !strings.EqualFold(name, "phone-context") {
			continue
		}
		context, err := url.PathUnescape(value)
		if err != nil || !strings.HasPrefix(context, "+") {
			return ""
		}
		return normalize(context + subscriber)
	}
	return normalize(subscriber)
}
