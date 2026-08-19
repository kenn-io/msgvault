//go:build !sqlite_vec

package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// evalCmd is a stub for builds that lack the sqlite_vec build tag. The eval
// command exercises vector/hybrid retrieval, which needs the sqlite-vec
// extension; binaries from `make build` (which sets `-tags "fts5 sqlite_vec"`)
// use the real implementation in eval.go.
var evalCmd = &cobra.Command{
	Use:   "eval",
	Short: "Evaluate retrieval quality against relevance judgments (requires sqlite_vec build)",
	RunE: func(_ *cobra.Command, _ []string) error {
		return fmt.Errorf("eval requires sqlite-vec support; rebuild with `go build -tags \"fts5 sqlite_vec\"`")
	},
}

func init() {
	rootCmd.AddCommand(evalCmd)
}
