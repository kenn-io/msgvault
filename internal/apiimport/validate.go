package apiimport

import (
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrValidation wraps every field error. The text after its prefix names the
// field and the rule, never the value, so the API can return it.
var ErrValidation = errors.New("import validation failed")

func ValidationError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrValidation, fmt.Sprintf(format, args...))
}

// ValidationDetail returns the field and rule of a validation error.
func ValidationDetail(err error) (string, bool) {
	if !errors.Is(err, ErrValidation) {
		return "", false
	}
	return strings.CutPrefix(err.Error(), ErrValidation.Error()+": ")
}

// ValidateText rejects text the archive can't store on every backend:
// invalid UTF-8, or a NUL character, which PostgreSQL text and JSONB refuse.
func ValidateText(field, value string) error {
	if !utf8.ValidString(value) {
		return ValidationError("%s must be valid UTF-8", field)
	}
	if strings.ContainsRune(value, 0) {
		return ValidationError("%s must not contain NUL characters", field)
	}
	return nil
}

// ValidateMetadataText applies ValidateText to every key and string value
// in free-form metadata, which is stored as JSON.
func ValidateMetadataText(field string, value any) error {
	switch value := value.(type) {
	case map[string]any:
		for key, nested := range value {
			if err := ValidateText(field, key); err != nil {
				return err
			}
			if err := ValidateMetadataText(field, nested); err != nil {
				return err
			}
		}
	case []any:
		for _, nested := range value {
			if err := ValidateMetadataText(field, nested); err != nil {
				return err
			}
		}
	case string:
		return ValidateText(field, value)
	}
	return nil
}

func ValidateBoundedRequired(field, value string, maxChars int) error {
	if value == "" {
		return ValidationError("%s is required", field)
	}
	return ValidateBoundedOptional(field, value, maxChars)
}

func ValidateBoundedOptional(field, value string, maxChars int) error {
	if err := ValidateText(field, value); err != nil {
		return err
	}
	if utf8.RuneCountInString(value) > maxChars {
		return ValidationError("%s must be at most %d characters", field, maxChars)
	}
	return nil
}

// NormalizeEmail returns one lowercased address without a display name.
func NormalizeEmail(field, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", ValidationError("%s is required", field)
	}
	if err := ValidateText(field, value); err != nil {
		return "", err
	}
	parsed, err := mail.ParseAddress(value)
	if err != nil || parsed.Name != "" || !strings.EqualFold(parsed.Address, value) {
		return "", ValidationError("%s must be one email address without a display name", field)
	}
	return strings.ToLower(parsed.Address), nil
}

// NormalizePhone accepts international numbers only (a leading + or 00) and
// returns E.164. National numbers are rejected rather than guessed: a wrong
// country code could attach an item to someone else. Errors name the field,
// never the number.
func NormalizePhone(field, value string) (string, error) {
	// "+44 (0)20 …" marks a trunk zero that is dialed only nationally.
	value = strings.ReplaceAll(strings.TrimSpace(value), "(0)", "")
	var digits strings.Builder
	for i, r := range value {
		switch {
		case r >= '0' && r <= '9':
			digits.WriteRune(r)
		case r == '+' && i == 0:
		case r == ' ' || r == '-' || r == '.' || r == '(' || r == ')':
		default:
			return "", ValidationError("%s must contain only digits and phone formatting", field)
		}
	}
	number := digits.String()
	switch {
	case strings.HasPrefix(value, "+"):
	case strings.HasPrefix(number, "00"):
		number = number[2:]
	default:
		return "", ValidationError("%s must be an international number starting with + or 00", field)
	}
	if strings.HasPrefix(number, "0") {
		return "", ValidationError("%s must start with a country code", field)
	}
	if len(number) < 7 || len(number) > 15 {
		return "", ValidationError("%s must have 7 to 15 digits", field)
	}
	return "+" + number, nil
}

// ParseTimestamp parses a required RFC 3339 timestamp and returns it in UTC.
func ParseTimestamp(field, value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, ValidationError("%s is required", field)
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, ValidationError("%s must be an RFC3339 timestamp with an explicit offset", field)
	}
	return parsed.UTC(), nil
}
