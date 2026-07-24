// Package explorecatalog owns dependency-neutral analytical request catalogs
// shared by persistence and transport layers.
package explorecatalog

const (
	GroupSource      = "source"
	GroupParticipant = "participant"
	GroupDomain      = "domain"
	GroupMessageType = "message_type"
	GroupKind        = "kind"
	GroupYear        = "year"
	GroupMonth       = "month"
)

var groupingDimensions = [...]string{
	GroupSource,
	GroupParticipant,
	GroupDomain,
	GroupMessageType,
	GroupKind,
	GroupYear,
	GroupMonth,
}

// GroupingDimensions returns every grouping value accepted by the analytical
// server contract. The returned slice cannot mutate the catalog.
func GroupingDimensions() []string {
	return append([]string(nil), groupingDimensions[:]...)
}

// IsGroupingDimension reports whether value is a canonical server grouping.
func IsGroupingDimension(value string) bool {
	for _, dimension := range groupingDimensions {
		if value == dimension {
			return true
		}
	}
	return false
}
