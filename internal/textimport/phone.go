package textimport

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// NormalizePhone normalizes a phone number to E.164 format.
// Returns an error for inputs that are not phone numbers (emails,
// short codes, system identifiers).
func NormalizePhone(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("empty input")
	}
	// Reject email addresses
	if strings.Contains(raw, "@") {
		return "", fmt.Errorf("not a phone number: %q", raw)
	}

	// Strip trunk prefix (0) before collecting digits, e.g. "+44 (0)7700" → "+44 7700"
	cleaned := strings.ReplaceAll(raw, "(0)", "")

	// Collect digits and any leading '+'; reject embedded '+' (e.g. "1+555...")
	var b strings.Builder
	leadingPlus := false
	for i, r := range cleaned {
		switch {
		case r == '+' && i == 0:
			leadingPlus = true
		case r == '+':
			return "", fmt.Errorf("embedded '+' in phone number: %q", raw)
		case unicode.IsDigit(r):
			b.WriteRune(r)
		}
	}
	justDigits := b.String()

	if justDigits == "" {
		return "", fmt.Errorf("no digits in input: %q", raw)
	}

	var digits string
	switch {
	case leadingPlus:
		digits = "+" + justDigits
	case strings.HasPrefix(justDigits, "00"):
		// International 00-prefix → replace with +
		digits = "+" + justDigits[2:]
	case len(justDigits) == 10:
		// Assume US country code
		digits = "+1" + justDigits
	default:
		// Everything else (incl. 11-digit US "1NNN..." and fully-qualified
		// international without "+") is treated as already country-coded.
		digits = "+" + justDigits
	}

	// Validate length against the final normalized digit string.
	finalDigits := digits[1:] // strip leading '+'
	if len(finalDigits) < 7 {
		return "", fmt.Errorf("too short for phone number: %q", raw)
	}
	// E.164 max is 15 digits (country code + subscriber)
	if len(finalDigits) > 15 {
		return "", fmt.Errorf("too long for E.164 (max 15 digits): %q", raw)
	}

	return digits, nil
}

type AddressKind string

const (
	AddressPhone AddressKind = "phone"
	AddressRaw   AddressKind = "raw"
)

type AddressNormalization struct {
	Kind  AddressKind
	Value string
}

func NormalizeAddress(raw string) AddressNormalization {
	phone, err := NormalizePhone(raw)
	if err == nil && phone != "" {
		return AddressNormalization{Kind: AddressPhone, Value: phone}
	}
	return AddressNormalization{Kind: AddressRaw, Value: strings.TrimSpace(raw)}
}
