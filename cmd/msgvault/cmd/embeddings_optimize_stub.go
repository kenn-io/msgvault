//go:build !sqlite_vec

package cmd

import (
	"errors"

	"github.com/spf13/cobra"
)

var embeddingsOptimizeCmd = &cobra.Command{
	Use:     "optimize [generation-id]",
	Short:   "Build or remove the SQLite search accelerator",
	Long:    "Build a local SQLite approximate-search accelerator from existing message vectors. This binary needs SQLite vector support to run the operation.",
	Example: "  msgvault embeddings optimize",
	Args:    cobra.MaximumNArgs(1),
	RunE: func(*cobra.Command, []string) error {
		return errors.New("SQLite accelerator unavailable without sqlite_vec")
	},
}

func runEmbeddingsOptimizeManaged(*cobra.Command, []string, func() error) error {
	return errors.New("SQLite accelerator unavailable without sqlite_vec")
}
