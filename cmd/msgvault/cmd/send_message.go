package cmd

import "github.com/spf13/cobra"

func init() { rootCmd.AddCommand(newSendMessageCommand()) }

func newSendMessageCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "send-message",
		Short: "Send to an archived Beeper chat",
		Long:  "Send text and/or one local file immediately. Sending requires [[beeper.sends]]. Requests are never retried; on remote_unknown, read the chat before sending again.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Flags().Changed("file") {
				return runDaemonCLICommandHTTPFromCobraWithLocalFiles(cmd, nil, nil)
			}
			return runDaemonCLICommandHTTPFromCobra(cmd, nil)
		},
	}
	command.Flags().String("account", "", "source account or display name")
	command.Flags().Int64("source-id", 0, "exact source ID")
	// Preserve repeated values so the daemon can reject ambiguous send inputs.
	command.Flags().StringArray("to", nil, "archived Beeper chat ID")
	command.Flags().String("body", "", "message text")
	command.Flags().StringArray("file", nil, "one local file (owner only)")
	command.Flags().Int64("reply-to", 0, "archived message in this chat")
	command.Flags().Bool("json", false, "emit one JSON result")
	return command
}
