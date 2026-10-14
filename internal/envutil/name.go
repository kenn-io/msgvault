package envutil

import "regexp"

var namePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidName reports whether name is a nonempty environment variable name.
func ValidName(name string) bool {
	return namePattern.MatchString(name)
}
