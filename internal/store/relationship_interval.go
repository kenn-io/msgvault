package store

import (
	"errors"
	"fmt"
	"strings"
)

// ErrPersonRelationshipInterval reports a world-time interval that cannot be
// true, such as an end that precedes its start.
var ErrPersonRelationshipInterval = errors.New("invalid relationship interval")

// ParseRelationshipDate parses one bound of a relationship's world-time
// interval.
//
// It narrows the shared PartialDate contract: a relationship bound MUST carry
// a year. The shared parser also accepts RFC 6350's truncated "--04-12" and
// "---12" forms, which are meaningful for a recurring birthday but not for an
// interval bound — a relationship cannot begin on "some April 12th" — and
// which no ordering could place. Rejecting them here keeps every stored bound
// orderable instead of admitting a value the interval check would have to
// ignore.
func ParseRelationshipDate(raw string) (PartialDate, error) {
	parsed, err := ParsePartialDate(strings.TrimSpace(raw))
	if err != nil {
		return PartialDate{}, err
	}
	if parsed.Year == nil {
		return PartialDate{}, fmt.Errorf(
			"%w: a relationship date requires a year, got %q", ErrInvalidPartialDate, raw)
	}
	return parsed, nil
}

// validateRelationshipInterval rejects an end that precedes its start. A nil
// bound is open-ended and always valid.
//
// Comparison uses CompareAtSharedPrecision, so only the components both bounds
// specify participate. That matters: "2019-06" to "2019" is not a
// contradiction — it says the relationship began in June 2019 and ended
// sometime that year — whereas a naive string or full-date comparison would
// reject it.
func validateRelationshipInterval(from, until *PartialDate) error {
	if err := validateRelationshipBound(from); err != nil {
		return err
	}
	if err := validateRelationshipBound(until); err != nil {
		return err
	}
	if from == nil || until == nil {
		return nil
	}
	if CompareAtSharedPrecision(*from, *until) > 0 {
		return fmt.Errorf("%w: start_date %q is after end_date %q",
			ErrPersonRelationshipInterval, from.String(), until.String())
	}
	return nil
}

// validateRelationshipBound validates values constructed directly by callers,
// not just values returned by ParseRelationshipDate. Relationship writers take
// PartialDate values, so treating the parser as the only entry point would let
// a yearless or calendar-invalid bound reach SQL.
func validateRelationshipBound(bound *PartialDate) error {
	if bound == nil {
		return nil
	}
	if err := bound.Validate(); err != nil {
		return fmt.Errorf("%w: relationship date %q", ErrInvalidPartialDate, bound.String())
	}
	if bound.Year == nil {
		return fmt.Errorf("%w: a relationship date requires a year", ErrInvalidPartialDate)
	}
	return nil
}
