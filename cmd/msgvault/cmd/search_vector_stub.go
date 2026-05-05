//go:build !sqlite_vec

package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// runHybridSearch is a stub for builds that lack the sqlite_vec build
// tag. The sqlite-vec extension is required for vector search; binaries
// produced by `make build` (which sets `-tags "fts5 sqlite_vec"`) use
// the real implementation in search_vector.go.
func runHybridSearch(_ *cobra.Command, _ string, mode string, _ bool, _ Scope) error {
	return fmt.Errorf(
		"--mode=%s requires sqlite-vec support; rebuild with `go build -tags \"fts5 sqlite_vec\"`",
		mode)
}
