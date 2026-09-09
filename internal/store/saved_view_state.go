package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/explorecatalog"
	"go.kenn.io/msgvault/internal/jsonexact"
)

// Version-1 Saved View vocabulary. Every value a definition may carry must be
// executable through the Explore contract, so the store rejects anything the
// daemon could not run rather than persisting a view no surface can open.
var (
	// SavedViewFilterAliases map legacy v1 filter field names onto the
	// Explore dimensions they execute against. Definitions keep the alias;
	// SavedViewFilterDimension resolves it at execution time.
	SavedViewFilterAliases = map[string]string{
		"source_id":      explorecatalog.FilterSource,
		"participant_id": explorecatalog.FilterParticipant,
	}
	SavedViewFilterOperators = []string{"eq", "in"}
	// SavedViewColumns are the Web UI's selectable result columns. They shape
	// rendering only and never reach an Explore request.
	SavedViewColumns = []string{"kind", "people", "title", "excerpt", "time", "attachments", "size"}
)

// savedViewStateFields are the canonical state's top-level members. An
// explicit null for any of them is rejected because a typed decoder would
// otherwise treat it like an absent field and store a quietly different
// definition.
var savedViewStateFields = []string{
	"query", "search_mode", "filters", "grouping", "presentation", "sort", "columns", "inspector_pinned",
}

// SavedViewFilterFields returns every filter field a v1 definition may name:
// the Explore dimensions followed by the legacy aliases.
func SavedViewFilterFields() []string {
	return append(explorecatalog.FilterDimensions(), slices.Sorted(maps.Keys(SavedViewFilterAliases))...)
}

// SavedViewFilterDimension maps a v1 filter field, including legacy aliases,
// onto the Explore dimension it executes against.
func SavedViewFilterDimension(field string) string {
	if dimension, ok := SavedViewFilterAliases[field]; ok {
		return dimension
	}
	return field
}

// ValidateSavedViewStateJSON checks raw canonical state for the shape rules a
// typed decoder cannot see: exact key spellings, one JSON object, and no
// explicit nulls. Clients that decode the state into a typed request must run
// this first; the store runs it before persisting.
func ValidateSavedViewStateJSON(raw []byte) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || isJSONNull(trimmed) {
		return fmt.Errorf("%w: canonical state must be a JSON object", ErrSavedViewInvalidState)
	}
	if err := jsonexact.Validate(trimmed, SavedViewStateEnvelope{}); err != nil {
		return fmt.Errorf("%w: %w", ErrSavedViewInvalidState, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil || fields == nil {
		return fmt.Errorf("%w: canonical state must be a JSON object", ErrSavedViewInvalidState)
	}
	for _, name := range savedViewStateFields {
		if value, present := fields[name]; present && isJSONNull(value) {
			return fmt.Errorf("%w: %s must not be null", ErrSavedViewInvalidState, name)
		}
	}
	rawFilters, present := fields["filters"]
	if !present {
		return nil
	}
	// A filters value of the wrong shape decodes to nothing here and is
	// reported by the typed decoder that runs next; this pass only looks for
	// nulls that decoder would erase.
	var filters []struct {
		Values []json.RawMessage `json:"values"`
	}
	_ = json.Unmarshal(rawFilters, &filters)
	for i, filter := range filters {
		for j, value := range filter.Values {
			if isJSONNull(value) {
				return fmt.Errorf("%w: filters[%d].values[%d] must not be null", ErrSavedViewInvalidState, i, j)
			}
		}
	}
	return nil
}

// ValidateSavedViewState checks a decoded v1 definition against the
// executable vocabulary. Errors wrap ErrSavedViewInvalidState.
func ValidateSavedViewState(state SavedViewStateEnvelope) error {
	if err := validateSavedViewState(state); err != nil {
		return fmt.Errorf("%w: %w", ErrSavedViewInvalidState, err)
	}
	return nil
}

func validateSavedViewState(state SavedViewStateEnvelope) error {
	for i, filter := range state.Filters {
		if err := validateSavedViewFilter(filter); err != nil {
			return fmt.Errorf("filters[%d]%w", i, err)
		}
	}
	if state.SearchMode != "" && !explorecatalog.IsSearchMode(state.SearchMode) {
		return fmt.Errorf("search_mode %q is not supported", state.SearchMode)
	}
	for i, grouping := range state.Grouping {
		if !explorecatalog.IsGroupingDimension(grouping) {
			return fmt.Errorf("grouping[%d] %q is not a supported analytical dimension", i, grouping)
		}
	}
	if state.Presentation != "" && !explorecatalog.IsPresentation(state.Presentation) {
		return fmt.Errorf("presentation %q is not supported", state.Presentation)
	}
	for i, sort := range state.Sort {
		if sort.Field != explorecatalog.EntrySortField || sort.Direction != explorecatalog.EntrySortDirection {
			return fmt.Errorf("sort[%d] must be %s %s", i, explorecatalog.EntrySortField, explorecatalog.EntrySortDirection)
		}
	}
	for i, column := range state.Columns {
		if !slices.Contains(SavedViewColumns, column) {
			return fmt.Errorf("columns[%d] %q is not a supported column", i, column)
		}
	}
	return nil
}

func validateSavedViewFilter(filter SavedViewFilter) error {
	if !explorecatalog.IsFilterDimension(SavedViewFilterDimension(filter.Field)) {
		return fmt.Errorf(".field %q is not a supported filter dimension", filter.Field)
	}
	if !slices.Contains(SavedViewFilterOperators, filter.Operator) {
		return fmt.Errorf(".operator must be %s", strings.Join(SavedViewFilterOperators, " or "))
	}
	if len(filter.Values) == 0 {
		return errors.New(".values must not be empty")
	}
	for j, value := range filter.Values {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf(".values[%d] must not be empty", j)
		}
	}
	return nil
}

func isJSONNull(value []byte) bool {
	return bytes.Equal(bytes.TrimSpace(value), []byte("null"))
}
