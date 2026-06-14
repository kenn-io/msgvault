//go:build !sqlite_vec && !pgvector

package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// runHybridSearch is a stub for builds that lack any vector backend build
// tag. A vector backend is required for vector search: sqlite_vec for the
// SQLite/vectors.db backend, or pgvector for the PostgreSQL backend. Binaries
// produced by `make build` (which sets `-tags "fts5 sqlite_vec"`) use the
// real implementation in search_vector.go.
func runHybridSearch(_ *cobra.Command, _ string, mode string, _ bool, _ Scope) error {
	return fmt.Errorf(
		"--mode=%s requires a vector backend; rebuild with `go build -tags sqlite_vec` (SQLite vectors) or `-tags pgvector` (PostgreSQL vectors)",
		mode)
}
