// Package sourceops resolves user-facing source selectors with explicit
// cardinality. Callers choose whether a selector must name exactly one source,
// an account plus its related sources, or every matching source.
package sourceops

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/opserr"
	"go.kenn.io/msgvault/internal/store"
)

// Store is the source catalog used by the resolvers.
type Store interface {
	GetSourceByID(sourceID int64) (*store.Source, error)
	GetSourcesByIdentifierOrDisplayName(identifier string) ([]*store.Source, error)
	GetSourcesByTypeAndAccount(sourceType, account string) ([]*store.Source, error)
}

// Selector identifies a source either by its exact database ID or by a
// user-facing account token. SourceType is a compatibility filter for callers
// that already expose one; it never makes a non-unique token exact.
type Selector struct {
	Account     string `json:"account,omitempty"`
	SourceID    int64  `json:"source_id,omitempty"`
	SourceIDSet bool   `json:"-"`
	SourceType  string `json:"-"`
}

// Selection reports the normalized input, optional primary account source,
// and every source selected by the chosen cardinality policy.
type Selection struct {
	Input   string
	Primary *store.Source
	Sources []*store.Source
}

// ResolveExactOne resolves exactly one source and rejects ambiguous tokens.
func ResolveExactOne(st Store, selector Selector) (*store.Source, error) {
	input, source, err := resolveSelector(st, selector)
	if err != nil || source != nil {
		return source, err
	}

	sources, err := lookupToken(st, input, selector.SourceType)
	if err != nil {
		return nil, err
	}
	switch len(sources) {
	case 0:
		return nil, sourceNotFound(input)
	case 1:
		return sources[0], nil
	default:
		return nil, ambiguous(input, sources)
	}
}

// ResolveAccountFamily resolves one primary account source and its related
// Google Calendar sources. An explicit source ID always remains exact.
func ResolveAccountFamily(st Store, selector Selector) (Selection, error) {
	input, source, err := resolveSelector(st, selector)
	if err != nil {
		return Selection{}, err
	}
	if source != nil {
		return Selection{Input: strconv.FormatInt(source.ID, 10), Primary: source, Sources: []*store.Source{source}}, nil
	}

	direct, err := lookupToken(st, input, selector.SourceType)
	if err != nil {
		return Selection{}, err
	}
	if len(direct) > 1 {
		return Selection{}, ambiguous(input, direct)
	}

	calendarAccount := input
	var primary *store.Source
	if len(direct) == 1 {
		primary = direct[0]
		if primary.SourceType != gcal.SourceType && !strings.EqualFold(primary.Identifier, input) {
			calendarAccount = primary.Identifier
		}
	}
	calendars, err := st.GetSourcesByTypeAndAccount(gcal.SourceType, calendarAccount)
	if err != nil {
		return Selection{}, opserr.Internal(fmt.Errorf("resolve calendar sources: %w", err))
	}

	sources := make([]*store.Source, 0, len(calendars)+1)
	seen := make(map[int64]struct{}, len(calendars)+1)
	if primary != nil {
		sources = append(sources, primary)
		seen[primary.ID] = struct{}{}
	}
	for _, calendar := range calendars {
		if _, ok := seen[calendar.ID]; ok {
			continue
		}
		sources = append(sources, calendar)
		seen[calendar.ID] = struct{}{}
	}
	if len(sources) == 0 {
		return Selection{}, sourceNotFound(input)
	}
	return Selection{Input: input, Primary: primary, Sources: sources}, nil
}

// ResolveAllMatches resolves every source matching an account token. An
// explicit source ID always remains exact.
func ResolveAllMatches(st Store, selector Selector) (Selection, error) {
	input, source, err := resolveSelector(st, selector)
	if err != nil {
		return Selection{}, err
	}
	if source != nil {
		return Selection{Input: strconv.FormatInt(source.ID, 10), Primary: source, Sources: []*store.Source{source}}, nil
	}

	sources, err := lookupToken(st, input, selector.SourceType)
	if err != nil {
		return Selection{}, err
	}
	if len(sources) == 0 {
		return Selection{}, sourceNotFound(input)
	}
	primary := sources[0]
	return Selection{Input: input, Primary: primary, Sources: sources}, nil
}

func resolveSelector(st Store, selector Selector) (string, *store.Source, error) {
	input := strings.TrimSpace(selector.Account)
	sourceType := strings.TrimSpace(selector.SourceType)
	idSet := selector.SourceIDSet || selector.SourceID != 0
	switch {
	case idSet && selector.SourceID <= 0:
		return "", nil, opserr.Invalid(errors.New("source ID must be positive"))
	case idSet && input != "":
		return "", nil, opserr.Invalid(errors.New("account and source ID are mutually exclusive"))
	case idSet && sourceType != "":
		return "", nil, opserr.Invalid(errors.New("source type and source ID are mutually exclusive"))
	case idSet:
		source, err := st.GetSourceByID(selector.SourceID)
		if errors.Is(err, store.ErrSourceNotFound) {
			return "", nil, opserr.NotFound(err)
		}
		if err != nil {
			return "", nil, opserr.Internal(fmt.Errorf("resolve source ID: %w", err))
		}
		return "", source, nil
	case input == "":
		return "", nil, opserr.Invalid(errors.New("account or source ID is required"))
	default:
		return input, nil, nil
	}
}

func lookupToken(st Store, input, sourceType string) ([]*store.Source, error) {
	sources, err := st.GetSourcesByIdentifierOrDisplayName(input)
	if err != nil {
		return nil, opserr.Internal(fmt.Errorf("resolve source token: %w", err))
	}
	if sourceType == "" {
		return uniqueSources(sources), nil
	}
	filtered := sources[:0]
	for _, source := range sources {
		if source.SourceType == sourceType {
			filtered = append(filtered, source)
		}
	}
	return uniqueSources(filtered), nil
}

func uniqueSources(sources []*store.Source) []*store.Source {
	unique := make([]*store.Source, 0, len(sources))
	seen := make(map[int64]struct{}, len(sources))
	for _, source := range sources {
		if source == nil {
			continue
		}
		if _, ok := seen[source.ID]; ok {
			continue
		}
		seen[source.ID] = struct{}{}
		unique = append(unique, source)
	}
	return unique
}

func ambiguous(input string, sources []*store.Source) error {
	details := make([]string, 0, len(sources))
	for _, source := range sources {
		details = append(details, fmt.Sprintf("%s (%s, id=%d)", source.Identifier, source.SourceType, source.ID))
	}
	return opserr.Invalid(fmt.Errorf(
		"ambiguous source selector %q matches multiple sources: %s",
		input,
		strings.Join(details, ", "),
	))
}

func sourceNotFound(input string) error {
	return opserr.NotFound(fmt.Errorf("no account found for %q", input))
}
