package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// ErrInvalidPartialDate reports a malformed or calendar-invalid partial date.
var ErrInvalidPartialDate = errors.New("invalid partial date")

// PartialDate is a calendar date with optional components in one of the six
// reduced or truncated shapes accepted by ParsePartialDate. Nil means
// unspecified, never zero. Compact vCard spellings parse to the same shape.
// Component storage is portable and indexable.
type PartialDate struct {
	Year  *int `json:"year,omitempty"`
	Month *int `json:"month,omitempty"`
	Day   *int `json:"day,omitempty"`
}

// ParsePartialDate accepts the reduced and truncated ISO forms used by vCard.
func ParsePartialDate(raw string) (PartialDate, error) {
	var date PartialDate
	var err error

	switch len(raw) {
	case 4:
		if raw[:2] == "--" {
			date.Month, err = parseDateComponent(raw[2:4], 2)
		} else {
			date.Year, err = parseDateComponent(raw, 4)
		}
	case 6:
		if raw[:2] != "--" {
			err = ErrInvalidPartialDate
			break
		}
		date.Month, err = parseDateComponent(raw[2:4], 2)
		if err == nil {
			date.Day, err = parseDateComponent(raw[4:6], 2)
		}
	case 7:
		if raw[:2] == "--" && raw[4] == '-' {
			date.Month, err = parseDateComponent(raw[2:4], 2)
			if err == nil {
				date.Day, err = parseDateComponent(raw[5:7], 2)
			}
		} else if raw[4] == '-' {
			date.Year, err = parseDateComponent(raw[:4], 4)
			if err == nil {
				date.Month, err = parseDateComponent(raw[5:7], 2)
			}
		} else {
			err = ErrInvalidPartialDate
		}
	case 8:
		date.Year, err = parseDateComponent(raw[:4], 4)
		if err == nil {
			date.Month, err = parseDateComponent(raw[4:6], 2)
		}
		if err == nil {
			date.Day, err = parseDateComponent(raw[6:8], 2)
		}
	case 10:
		if raw[4] != '-' || raw[7] != '-' {
			err = ErrInvalidPartialDate
			break
		}
		date.Year, err = parseDateComponent(raw[:4], 4)
		if err == nil {
			date.Month, err = parseDateComponent(raw[5:7], 2)
		}
		if err == nil {
			date.Day, err = parseDateComponent(raw[8:10], 2)
		}
	case 5:
		if raw[:3] != "---" {
			err = ErrInvalidPartialDate
			break
		}
		date.Day, err = parseDateComponent(raw[3:5], 2)
	default:
		err = ErrInvalidPartialDate
	}
	if err != nil {
		return PartialDate{}, fmt.Errorf("%w: %q", ErrInvalidPartialDate, raw)
	}
	if err := date.Validate(); err != nil {
		return PartialDate{}, fmt.Errorf("%w: %q", err, raw)
	}
	return date, nil
}

func parseDateComponent(raw string, width int) (*int, error) {
	if len(raw) != width {
		return nil, ErrInvalidPartialDate
	}
	for i := range len(raw) {
		if raw[i] < '0' || raw[i] > '9' {
			return nil, ErrInvalidPartialDate
		}
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return nil, ErrInvalidPartialDate
	}
	return &value, nil
}

// IsZero reports whether no date component is present.
func (d PartialDate) IsZero() bool {
	return d.Year == nil && d.Month == nil && d.Day == nil
}

// Validate checks component ranges and calendar validity.
func (d PartialDate) Validate() error {
	if d.IsZero() {
		return ErrInvalidPartialDate
	}
	if d.Year != nil && (*d.Year < 1 || *d.Year > 9999) {
		return ErrInvalidPartialDate
	}
	if d.Month != nil && (*d.Month < 1 || *d.Month > 12) {
		return ErrInvalidPartialDate
	}
	if d.Day != nil && (*d.Day < 1 || *d.Day > 31) {
		return ErrInvalidPartialDate
	}
	if d.Year != nil && d.Month == nil && d.Day != nil {
		return ErrInvalidPartialDate
	}
	if d.Day != nil && d.Month != nil {
		year := 2000
		if d.Year != nil {
			year = *d.Year
		}
		candidate := time.Date(year, time.Month(*d.Month), *d.Day, 0, 0, 0, 0, time.UTC)
		if candidate.Year() != year || int(candidate.Month()) != *d.Month || candidate.Day() != *d.Day {
			return ErrInvalidPartialDate
		}
	}
	return nil
}

// String renders the reduced or truncated ISO representation.
func (d PartialDate) String() string {
	switch {
	case d.Year != nil && d.Month != nil && d.Day != nil:
		return fmt.Sprintf("%04d-%02d-%02d", *d.Year, *d.Month, *d.Day)
	case d.Year != nil && d.Month != nil:
		return fmt.Sprintf("%04d-%02d", *d.Year, *d.Month)
	case d.Year != nil:
		return fmt.Sprintf("%04d", *d.Year)
	case d.Month != nil && d.Day != nil:
		return fmt.Sprintf("--%02d-%02d", *d.Month, *d.Day)
	case d.Month != nil:
		return fmt.Sprintf("--%02d", *d.Month)
	case d.Day != nil:
		return fmt.Sprintf("---%02d", *d.Day)
	default:
		return ""
	}
}

// CompareAtSharedPrecision compares only components both values specify.
func CompareAtSharedPrecision(a, b PartialDate) int {
	for _, pair := range [][2]*int{{a.Year, b.Year}, {a.Month, b.Month}, {a.Day, b.Day}} {
		if pair[0] == nil || pair[1] == nil {
			continue
		}
		if *pair[0] < *pair[1] {
			return -1
		}
		if *pair[0] > *pair[1] {
			return 1
		}
	}
	return 0
}

// PartialDateArgs returns nullable SQL bind values in year/month/day order.
func PartialDateArgs(d PartialDate) []any {
	return []any{intValue(d.Year), intValue(d.Month), intValue(d.Day)}
}

// ScanPartialDate converts nullable component columns to a PartialDate.
func ScanPartialDate(year, month, day sql.NullInt64) PartialDate {
	return PartialDate{
		Year:  nullIntPtr(year),
		Month: nullIntPtr(month),
		Day:   nullIntPtr(day),
	}
}

func intValue(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullIntPtr(value sql.NullInt64) *int {
	if !value.Valid {
		return nil
	}
	converted := int(value.Int64)
	return &converted
}
