package cmd

import "github.com/spf13/cobra"

func init() {
	rootCmd.AddCommand(newDraftForwardCommand())
}

func newDraftForwardCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "draft-forward <message-id>",
		Short: "Create an IMAP draft forwarding an archived message",
		Args:  cobra.ExactArgs(1),
		RunE:  runDaemonCLICommandHTTPFromCobra,
	}
	command.Flags().String(draftFromFlag, "", "confirmed destination source identity for the draft")
	command.Flags().StringArray("to", nil, "recipient address, repeatable")
	command.Flags().StringArray("cc", nil, "Cc recipient address, repeatable")
	command.Flags().StringArray("bcc", nil, "Bcc recipient address, repeatable")
	command.Flags().String("account", "", "destination source account or display name")
	command.Flags().Int64("source-id", 0, "exact destination source ID")
	command.Flags().String("body", "", "forwarding note")
	command.Flags().Bool("json", false, "emit one JSON result")
	return command
}
