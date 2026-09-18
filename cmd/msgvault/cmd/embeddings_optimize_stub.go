//go:build !sqlite_vec

package cmd

import (
	"errors"

	"github.com/spf13/cobra"
)

var embeddingsOptimizeCmd = &cobra.Command{
	Use:   "optimize [generation-id]",
	Short: "Build the SQLite search accelerator from stored embeddings",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(*cobra.Command, []string) error {
		return errors.New("SQLite accelerator unavailable without sqlite_vec")
	},
}
