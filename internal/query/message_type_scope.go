package query

import "slices"

func containsMessageType(types []string, want string) bool {
	return slices.Contains(types, want)
}

// ScopedMessageTypes intersects parsed query message types with a single
// drill-down filter type. The returned bool is true when the intersection is
// empty and callers should force the overall query to match no messages.
func ScopedMessageTypes(queryTypes []string, filterType string) ([]string, bool) {
	if filterType == "" {
		return append([]string(nil), queryTypes...), false
	}
	if len(queryTypes) == 0 {
		return []string{filterType}, false
	}
	if containsMessageType(queryTypes, filterType) {
		return []string{filterType}, false
	}
	return []string{filterType}, true
}
