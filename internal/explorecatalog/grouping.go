// Package explorecatalog owns dependency-neutral analytical request catalogs
// shared by persistence and transport layers.
package explorecatalog

const (
	GroupSource      = "source"
	GroupParticipant = "participant"
	GroupDomain      = "domain"
	GroupMessageType = "message_type"
	GroupMailingList = "mailing_list"
	GroupKind        = "kind"
	GroupYear        = "year"
	GroupMonth       = "month"
	GroupDay         = "day"
)

var groupingDimensions = [...]string{
	GroupSource,
	GroupParticipant,
	GroupDomain,
	GroupMessageType,
	GroupMailingList,
	GroupKind,
	GroupYear,
	GroupMonth,
}

// GroupingDimensions returns the saved-state grouping vocabulary. The returned slice cannot mutate the catalog.
func GroupingDimensions() []string {
	return append([]string(nil), groupingDimensions[:]...)
}

// IsGroupingDimension reports whether value is a supported saved-state grouping.
func IsGroupingDimension(value string) bool {
	for _, dimension := range groupingDimensions {
		if value == dimension {
			return true
		}
	}
	return false
}

// RequestGroupingDimensions includes daily message counts for activity requests.
func RequestGroupingDimensions() []string {
	return append(GroupingDimensions(), GroupDay)
}
