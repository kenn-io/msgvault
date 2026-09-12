package cmd

import "github.com/spf13/cobra"

func init() {
	rootCmd.AddCommand(newDraftGetCommand())
}

func newDraftGetCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "draft-get <draft-id>",
		Short: "Inspect a locally tracked IMAP draft and its remote state",
		Args:  cobra.ExactArgs(1),
		RunE:  runDaemonCLICommandHTTPFromCobra,
	}
	command.Flags().Bool("json", false, "emit one JSON result")
	return command
}
