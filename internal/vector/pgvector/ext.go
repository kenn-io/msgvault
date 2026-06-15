//go:build pgvector

package pgvector

// Available reports whether this build includes the pgvector backend.
// Mirrors the sqlitevec.Available shape so call sites can branch on
// build-tag presence without duplicating logic.
func Available() bool { return true }
