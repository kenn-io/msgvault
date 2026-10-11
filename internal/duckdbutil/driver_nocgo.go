//go:build !cgo

package duckdbutil

// Available reports whether this build includes the DuckDB driver.
const Available = false

// IsOutOfMemory is false because this build cannot produce DuckDB errors.
func IsOutOfMemory(error) bool { return false }
