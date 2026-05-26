package testutil

// MakeSet builds a map[T]bool from the given items.
// Useful for constructing selection sets in tests.
func MakeSet[T comparable](items ...T) map[T]bool {
	m := make(map[T]bool, len(items))
	for _, item := range items {
		m[item] = true
	}
	return m
}
