//go:build !sqlite_vec && !pgvector

package cmd

import (
	"errors"

	"github.com/spf13/cobra"
)

// runEmbed is a stub for builds that lack any vector backend build tag.
// A vector backend is required for embedding generation: sqlite_vec for the
// SQLite/vectors.db backend, or pgvector for the PostgreSQL backend. Binaries
// produced by `make build` (which sets `-tags "fts5 sqlite_vec"`) use the
// real implementation in embed_vector.go.
func runEmbed(_ *cobra.Command) error {
	return errors.New("msgvault embeddings build requires a vector backend; rebuild with `go build -tags sqlite_vec` (SQLite vectors) or `-tags pgvector` (PostgreSQL vectors)")
}
