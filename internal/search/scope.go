package search

import "slices"

// IntersectValues narrows criteria to a scope; an empty input leaves that side unrestricted.
func IntersectValues(criteria, scope []string) ([]string, bool) {
	if len(scope) == 0 {
		return slices.Clone(criteria), false
	}
	values := slices.Clone(scope)
	if len(criteria) > 0 {
		values = slices.DeleteFunc(values, func(value string) bool { return !slices.Contains(criteria, value) })
	}
	return values, len(values) == 0
}
