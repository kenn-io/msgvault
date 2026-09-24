//go:build !sqlite_vec

package cmd

import (
	"errors"

	"github.com/spf13/cobra"
)

var embeddingsOptimizeWorkerCmd = &cobra.Command{
	Use:    embeddingsOptimizeWorkerName,
	Hidden: true,
	RunE: func(*cobra.Command, []string) error {
		return errors.New("SQLite accelerator worker unavailable without sqlite_vec")
	},
}
