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
	"golang.org/x/text/encoding/ianaindex"
)

// Contact is a single parsed vCard entry.
type Contact struct {
	FullName string
	Phones   []string // normalized to E.164
	Emails   []string // lowercased
}

// ParseFile reads a vCard file and projects its contact identity fields.
func ParseFile(path string) ([]Contact, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open vCard %q: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	document, err := Decode(file)
	if err != nil {
		return nil, fmt.Errorf("decode vCard %q: %w", path, err)
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
				if normalized := normalizeLegacyPhone(strings.TrimSpace(phone)); normalized != "" {
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
	raw := property.RawValue
	if propertyIsQuotedPrintable(property) {
		decoded, err := DecodeQuotedPrintable(raw)
		if err != nil {
			return "", err
		}
		raw = decoded
	}
	charset := propertyCharset(property)
	if charset != "" && !strings.EqualFold(charset, "UTF-8") && !strings.EqualFold(charset, "UTF8") {
		encoding, err := ianaindex.MIME.Encoding(charset)
		if err != nil {
			return "", fmt.Errorf("resolve charset %q: %w", charset, err)
		}
		if encoding == nil {
			return "", fmt.Errorf("charset %q is not supported", charset)
		}
		decoded, err := encoding.NewDecoder().String(raw)
		if err != nil {
			return "", fmt.Errorf("decode charset %q: %w", charset, err)
		}
		raw = decoded
	}
	if !utf8.ValidString(raw) {
		return "", errors.New("decoded text is not valid UTF-8")
	}
	return UnescapeText(raw)
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

func normalizeLegacyPhone(raw string) string {
	if len(raw) < len("tel:") || !strings.EqualFold(raw[:len("tel:")], "tel:") {
		return normalizePhone(raw)
	}

	parts := strings.Split(raw[len("tel:"):], ";")
	subscriber, err := url.PathUnescape(parts[0])
	if err != nil {
		return ""
	}
	if strings.HasPrefix(subscriber, "+") {
		return normalizePhone(subscriber)
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
		return normalizePhone(context + subscriber)
	}
	return normalizePhone(subscriber)
}
