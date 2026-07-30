package vcard

import (
	"errors"
	"fmt"
	"io"
	"mime/quotedprintable"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	compactDatePattern    = regexp.MustCompile(`^(\d{4})(\d{2})(\d{2})$`)
	dashedDatePattern     = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})$`)
	yearMonthPattern      = regexp.MustCompile(`^(\d{4})-(\d{2})$`)
	yearPattern           = regexp.MustCompile(`^\d{4}$`)
	truncatedMonthDay     = regexp.MustCompile(`^--(\d{2})-?(\d{2})$`)
	truncatedMonthPattern = regexp.MustCompile(`^--(\d{2})$`)
	truncatedDayPattern   = regexp.MustCompile(`^---(\d{2})$`)
)

// PartialDate represents a vCard date whose year, month, or day may be absent.
type PartialDate struct {
	Year  *int
	Month *int
	Day   *int
}

// EscapeText encodes a vCard TEXT value.
func EscapeText(value string) string {
	var escaped strings.Builder
	escaped.Grow(len(value))
	for _, r := range value {
		switch r {
		case '\\':
			escaped.WriteString(`\\`)
		case ',':
			escaped.WriteString(`\,`)
		case ';':
			escaped.WriteString(`\;`)
		case '\n':
			escaped.WriteString(`\n`)
		default:
			escaped.WriteRune(r)
		}
	}
	return escaped.String()
}

// UnescapeText decodes vCard TEXT escaping while preserving unknown escapes.
func UnescapeText(raw string) (string, error) {
	if strings.ContainsAny(raw, "\r\x00") {
		return "", errors.New("text value contains CR or NUL")
	}
	var decoded strings.Builder
	decoded.Grow(len(raw))
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' || i+1 >= len(raw) {
			decoded.WriteByte(raw[i])
			continue
		}
		switch raw[i+1] {
		case '\\', ',', ';':
			decoded.WriteByte(raw[i+1])
			i++
		case 'n', 'N':
			decoded.WriteByte('\n')
			i++
		default:
			decoded.WriteByte('\\')
		}
	}
	return decoded.String(), nil
}

// SplitTextList splits a comma-separated TEXT list before unescaping values.
func SplitTextList(raw string) ([]string, error) {
	return splitText(raw, ',')
}

// JoinTextList encodes and joins a comma-separated TEXT list.
func JoinTextList(values []string) string {
	return joinText(values, ",")
}

// SplitStructuredText splits a semicolon-separated structured TEXT value.
func SplitStructuredText(raw string) ([]string, error) {
	return splitText(raw, ';')
}

// JoinStructuredText encodes and joins a structured TEXT value.
func JoinStructuredText(values []string) string {
	return joinText(values, ";")
}

func splitText(raw string, delimiter byte) ([]string, error) {
	parts := make([]string, 0, strings.Count(raw, string(delimiter))+1)
	start := 0
	for i := 0; i < len(raw); i++ {
		if raw[i] == '\\' && i+1 < len(raw) {
			i++
			continue
		}
		if raw[i] == delimiter {
			parts = append(parts, raw[start:i])
			start = i + 1
		}
	}
	parts = append(parts, raw[start:])
	for i := range parts {
		decoded, err := UnescapeText(parts[i])
		if err != nil {
			return nil, fmt.Errorf("component %d: %w", i+1, err)
		}
		parts[i] = decoded
	}
	return parts, nil
}

func joinText(values []string, separator string) string {
	escaped := make([]string, len(values))
	for i, value := range values {
		escaped[i] = EscapeText(value)
	}
	return strings.Join(escaped, separator)
}

// DecodeParameterValue decodes RFC 6868 parameter-value escaping.
func DecodeParameterValue(raw string) (string, error) {
	if containsInjection(raw) {
		return "", errors.New("parameter value contains CR, LF, or NUL")
	}
	return decodeRFC6868(raw), nil
}

// EncodeParameterValue encodes RFC 6868 parameter-value escaping.
func EncodeParameterValue(value string) string {
	return encodeRFC6868(value)
}

// DecodeQuotedPrintable decodes a quoted-printable value.
func DecodeQuotedPrintable(raw string) (string, error) {
	decoded, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(raw)))
	if err != nil {
		return "", fmt.Errorf("decode quoted-printable: %w", err)
	}
	return string(decoded), nil
}

// ParsePartialDate parses complete and truncated vCard date values.
func ParsePartialDate(raw string) (PartialDate, error) {
	switch {
	case compactDatePattern.MatchString(raw):
		parts := compactDatePattern.FindStringSubmatch(raw)
		year, month, day := mustAtoi(parts[1]), mustAtoi(parts[2]), mustAtoi(parts[3])
		if err := validateCalendarDate(year, month, day); err != nil {
			return PartialDate{}, err
		}
		return newPartialDate(&year, &month, &day), nil
	case dashedDatePattern.MatchString(raw):
		parts := dashedDatePattern.FindStringSubmatch(raw)
		year, month, day := mustAtoi(parts[1]), mustAtoi(parts[2]), mustAtoi(parts[3])
		if err := validateCalendarDate(year, month, day); err != nil {
			return PartialDate{}, err
		}
		return newPartialDate(&year, &month, &day), nil
	case yearMonthPattern.MatchString(raw):
		parts := yearMonthPattern.FindStringSubmatch(raw)
		year, month := mustAtoi(parts[1]), mustAtoi(parts[2])
		if year == 0 || month < 1 || month > 12 {
			return PartialDate{}, fmt.Errorf("invalid partial date %q", raw)
		}
		return newPartialDate(&year, &month, nil), nil
	case yearPattern.MatchString(raw):
		year := mustAtoi(raw)
		if year == 0 {
			return PartialDate{}, fmt.Errorf("invalid partial date %q", raw)
		}
		return newPartialDate(&year, nil, nil), nil
	case truncatedMonthDay.MatchString(raw):
		parts := truncatedMonthDay.FindStringSubmatch(raw)
		month, day := mustAtoi(parts[1]), mustAtoi(parts[2])
		if err := validateCalendarDate(2000, month, day); err != nil {
			return PartialDate{}, fmt.Errorf("invalid partial date %q", raw)
		}
		return newPartialDate(nil, &month, &day), nil
	case truncatedMonthPattern.MatchString(raw):
		parts := truncatedMonthPattern.FindStringSubmatch(raw)
		month := mustAtoi(parts[1])
		if month < 1 || month > 12 {
			return PartialDate{}, fmt.Errorf("invalid partial date %q", raw)
		}
		return newPartialDate(nil, &month, nil), nil
	case truncatedDayPattern.MatchString(raw):
		parts := truncatedDayPattern.FindStringSubmatch(raw)
		day := mustAtoi(parts[1])
		if day < 1 || day > 31 {
			return PartialDate{}, fmt.Errorf("invalid partial date %q", raw)
		}
		return newPartialDate(nil, nil, &day), nil
	default:
		return PartialDate{}, fmt.Errorf("invalid partial date %q", raw)
	}
}

func newPartialDate(year, month, day *int) PartialDate {
	return PartialDate{Year: year, Month: month, Day: day}
}

func mustAtoi(raw string) int {
	value, _ := strconv.Atoi(raw)
	return value
}

func validateCalendarDate(year, month, day int) error {
	if year == 0 || month < 1 || month > 12 || day < 1 || day > 31 {
		return errors.New("invalid calendar date")
	}
	got := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
	if got.Year() != year || int(got.Month()) != month || got.Day() != day {
		return errors.New("invalid calendar date")
	}
	return nil
}

// String renders the partial date with deterministic dashed precision.
func (d PartialDate) String() string {
	switch {
	case d.Year != nil && d.Month != nil && d.Day != nil:
		return fmt.Sprintf("%04d-%02d-%02d", *d.Year, *d.Month, *d.Day)
	case d.Year != nil && d.Month != nil && d.Day == nil:
		return fmt.Sprintf("%04d-%02d", *d.Year, *d.Month)
	case d.Year != nil && d.Month == nil && d.Day == nil:
		return fmt.Sprintf("%04d", *d.Year)
	case d.Year == nil && d.Month != nil && d.Day != nil:
		return fmt.Sprintf("--%02d-%02d", *d.Month, *d.Day)
	case d.Year == nil && d.Month != nil && d.Day == nil:
		return fmt.Sprintf("--%02d", *d.Month)
	case d.Year == nil && d.Month == nil && d.Day != nil:
		return fmt.Sprintf("---%02d", *d.Day)
	default:
		return ""
	}
}
