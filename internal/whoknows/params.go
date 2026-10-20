package whoknows

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

var (
	ErrMissingFreeText  = errors.New("topic must contain free text")
	ErrInvalidParameter = errors.New("invalid parameter")
)

const (
	DefaultLimit    = 10
	MaxLimit        = 50
	DefaultEvidence = 3
	MaxEvidence     = 10
	DefaultMinScore = 0.5
)

// Params is a validated Request with its defaults applied.
type Params struct {
	Topic    string
	Limit    int
	Evidence int
	MinScore float64
	After    *time.Time
	Before   *time.Time
}

// Resolve applies defaults and validates the request.
func (r Request) Resolve() (Params, error) {
	p := Params{
		Topic:    strings.TrimSpace(r.Topic),
		Limit:    DefaultLimit,
		Evidence: DefaultEvidence,
		MinScore: DefaultMinScore,
	}
	if p.Topic == "" {
		return p, ErrMissingFreeText
	}
	if r.Limit != nil {
		p.Limit = *r.Limit
	}
	if r.Evidence != nil {
		p.Evidence = *r.Evidence
	}
	if r.MinScore != nil {
		p.MinScore = *r.MinScore
	}
	if p.Limit < 1 || p.Limit > MaxLimit {
		return p, fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidParameter, MaxLimit)
	}
	if p.Evidence < 1 || p.Evidence > MaxEvidence {
		return p, fmt.Errorf("%w: evidence must be between 1 and %d", ErrInvalidParameter, MaxEvidence)
	}
	if math.IsNaN(p.MinScore) || p.MinScore <= 0 || p.MinScore > 1 {
		return p, fmt.Errorf("%w: min_score must be greater than 0 and at most 1", ErrInvalidParameter)
	}
	var err error
	if p.After, err = ParseDate(r.After); err != nil {
		return p, fmt.Errorf("%w: after: %w", ErrInvalidParameter, err)
	}
	if p.Before, err = ParseDate(r.Before); err != nil {
		return p, fmt.Errorf("%w: before: %w", ErrInvalidParameter, err)
	}
	if p.After != nil && p.Before != nil && !p.After.Before(*p.Before) {
		return p, fmt.Errorf("%w: after must precede before", ErrInvalidParameter)
	}
	return p, nil
}

// ParseDate reads YYYY-MM-DD or RFC3339. An empty string means no bound.
// Like the search operators, a date-only value is midnight UTC, so a window
// includes its after instant and excludes its before instant.
func ParseDate(raw string) (*time.Time, error) {
	if raw == "" {
		return nil, nil //nolint:nilnil // An omitted date leaves the filter unbounded.
	}
	layout := time.RFC3339
	if len(raw) == len(time.DateOnly) {
		layout = time.DateOnly
	}
	v, err := time.Parse(layout, raw)
	if err != nil {
		return nil, fmt.Errorf("date must be YYYY-MM-DD or RFC3339: %w", err)
	}
	return &v, nil
}
