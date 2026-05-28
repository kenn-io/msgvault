//go:build !sqlite_vec

package cmd

import (
	"errors"

	"github.com/spf13/cobra"
)

// runEmbed is a stub for builds that lack the sqlite_vec build tag.
// The sqlite-vec extension is required for embedding generation; binaries
// produced by `make build` (which sets `-tags "fts5 sqlite_vec"`) use
// the real implementation in embed_vector.go.
func runEmbed(_ *cobra.Command) error {
	return errors.New("msgvault embeddings build requires sqlite-vec support; rebuild with `go build -tags \"fts5 sqlite_vec\"`")
}
