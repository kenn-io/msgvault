package cmd

import (
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/importer"
)

func newImportMaildirCommand() *cobra.Command {
	return newImportRawDirectoryCommand("maildir", importer.ImportMaildir)
}

func init() { rootCmd.AddCommand(newImportMaildirCommand()) }
