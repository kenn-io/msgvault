package explorecatalog

import "slices"

// Explore filter dimensions accepted by the analytical server contract.
const (
	FilterSource      = "source"
	FilterIdentity    = "identity"
	FilterParticipant = "participant"
	FilterDomain      = "domain"
	FilterMailingList = "mailing_list"
	FilterMessageType = "message_type"
	FilterAfter       = "after"
	FilterBefore      = "before"
	FilterDeletion    = "deletion"
)

var filterDimensions = [...]string{
	FilterSource,
	FilterIdentity,
	FilterParticipant,
	FilterDomain,
	FilterMailingList,
	FilterMessageType,
	FilterAfter,
	FilterBefore,
	FilterDeletion,
}

// Explore search modes accepted by the analytical server contract.
const (
	SearchModeFullText = "full_text"
	SearchModeSemantic = "semantic"
	SearchModeHybrid   = "hybrid"
)

var searchModes = [...]string{SearchModeFullText, SearchModeSemantic, SearchModeHybrid}

// Explore presentations accepted by the analytical server contract.
const (
	PresentationTable    = "table"
	PresentationTimeline = "timeline"
	PresentationFiles    = "files"
)

var presentations = [...]string{PresentationTable, PresentationTimeline, PresentationFiles}

// Entry sort accepted by the analytical server contract. Entries sort by
// occurrence time, newest first, and nothing else.
const (
	EntrySortField     = "occurred_at"
	EntrySortDirection = "desc"
)

// FilterDimensions returns every filter dimension accepted by the analytical
// server contract. The returned slice cannot mutate the catalog.
func FilterDimensions() []string {
	return append([]string(nil), filterDimensions[:]...)
}

// IsFilterDimension reports whether value is a canonical server filter
// dimension.
func IsFilterDimension(value string) bool {
	return slices.Contains(filterDimensions[:], value)
}

// SearchModes returns every search mode accepted by the analytical server
// contract. The returned slice cannot mutate the catalog.
func SearchModes() []string {
	return append([]string(nil), searchModes[:]...)
}

// IsSearchMode reports whether value is a canonical server search mode.
func IsSearchMode(value string) bool {
	return slices.Contains(searchModes[:], value)
}

// Presentations returns every presentation accepted by the analytical server
// contract. The returned slice cannot mutate the catalog.
func Presentations() []string {
	return append([]string(nil), presentations[:]...)
}

// IsPresentation reports whether value is a canonical server presentation.
func IsPresentation(value string) bool {
	return slices.Contains(presentations[:], value)
}
