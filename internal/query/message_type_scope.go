package query

import "go.kenn.io/msgvault/internal/search"

// ScopedMessageTypes intersects parsed query message types with a single
// drill-down filter type. The returned bool is true when the intersection is
// empty and callers should force the overall query to match no messages.
func ScopedMessageTypes(queryTypes []string, filterType string) ([]string, bool) {
	if filterType == "" {
		return search.IntersectValues(queryTypes, nil)
	}
	values, noMatches := search.IntersectValues(queryTypes, []string{filterType})
	if noMatches {
		return []string{filterType}, true
	}
	return values, false
}
