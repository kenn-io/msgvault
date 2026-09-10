package cmd

import "github.com/spf13/cobra"

func init() {
	rootCmd.AddCommand(newDraftReplyCommand())
}

func newDraftReplyCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "draft-reply <message-id>",
		Short: "Create an IMAP reply draft from an archived message",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("from") {
				return usageErr(cmd, errDraftReplyFromRequired)
			}
			if !cmd.Flags().Changed("body") {
				return usageErr(cmd, errDraftReplyBodyRequired)
			}
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		},
	}
	command.Flags().String("from", "", "confirmed source identity for the draft")
	command.Flags().String("body", "", "reply body")
	command.Flags().Bool("json", false, "emit one JSON result")
	return command
}
