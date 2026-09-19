package cmd

import "github.com/spf13/cobra"

func init() {
	rootCmd.AddCommand(newDraftComposeCommand())
}

func newDraftComposeCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "draft-compose",
		Short: "Create an IMAP draft from a selected account",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDaemonCLICommandHTTPFromCobra(cmd, nil)
		},
	}
	command.Flags().String("account", "", "source account or display name")
	command.Flags().Int64("source-id", 0, "exact source ID")
	command.Flags().String("from", "", "confirmed source identity for the draft")
	command.Flags().StringArray("to", nil, "recipient address, repeatable")
	command.Flags().StringArray("cc", nil, "Cc recipient address, repeatable")
	command.Flags().StringArray("bcc", nil, "Bcc recipient address, repeatable")
	command.Flags().String("subject", "", "draft subject")
	command.Flags().String("body", "", "draft body")
	command.Flags().Bool("json", false, "emit one JSON result")
	return command
}
