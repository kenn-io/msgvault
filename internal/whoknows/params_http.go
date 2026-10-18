package whoknows

import (
	"fmt"
	"go.kenn.io/msgvault/internal/search"
	"net/url"
	"strconv"
	"strings"
)

// ParseHTTPParams shares the strict API query contract with daemon clients.
func ParseHTTPParams(values url.Values) (string, Params, error) {
	p := DefaultParams()
	topic := strings.TrimSpace(values.Get("q"))
	for key, entries := range values {
		if len(entries) != 1 {
			return topic, p, fmt.Errorf("%w: %s must be supplied once", ErrInvalidParameter, key)
		}
	}
	if len(search.Parse(topic).TextTerms) == 0 {
		return topic, p, ErrMissingFreeText
	}
	for _, v := range []struct {
		name   string
		target *int
	}{{"limit", &p.Limit}, {"evidence", &p.Evidence}, {"candidates", &p.Candidates}, {"half_life_days", &p.HalfLifeDays}} {
		if values.Has(v.name) {
			n, err := strconv.Atoi(values.Get(v.name))
			if err != nil {
				return topic, p, fmt.Errorf("%w: %s must be an integer", ErrInvalidParameter, v.name)
			}
			*v.target = n
		}
	}
	for _, v := range []struct {
		name   string
		target *float64
	}{{"min_score", &p.MinScore}, {"min_profile_score", &p.MinProfileScore}} {
		if values.Has(v.name) {
			n, err := strconv.ParseFloat(values.Get(v.name), 64)
			if err != nil {
				return topic, p, fmt.Errorf("%w: %s must be numeric", ErrInvalidParameter, v.name)
			}
			*v.target = n
		}
	}
	for _, v := range []struct {
		name   string
		target *bool
	}{{"include_owner", &p.IncludeOwner}, {"include_unsaved", &p.IncludeUnsaved}, {"include_organizations", &p.IncludeOrganizations}, {"excerpts", &p.Excerpts}} {
		if values.Has(v.name) {
			b, err := strconv.ParseBool(values.Get(v.name))
			if err != nil {
				return topic, p, fmt.Errorf("%w: %s must be boolean", ErrInvalidParameter, v.name)
			}
			*v.target = b
		}
	}
	if values.Has("tiebreak") {
		p.Tiebreak = values.Get("tiebreak")
	}
	var err error
	p.After, err = ParseDate(values.Get("after"), false)
	if err != nil {
		return topic, p, fmt.Errorf("%w: after: %w", ErrInvalidParameter, err)
	}
	p.Before, err = ParseDate(values.Get("before"), true)
	if err != nil {
		return topic, p, fmt.Errorf("%w: before: %w", ErrInvalidParameter, err)
	}
	if err = p.Validate(); err != nil {
		return topic, p, fmt.Errorf("%w: %w", ErrInvalidParameter, err)
	}
	return topic, p, nil
}
