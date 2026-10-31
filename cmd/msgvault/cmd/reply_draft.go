package cmd

import "github.com/spf13/cobra"

func init() {
	rootCmd.AddCommand(newDraftReplyCommand())
}

const draftFromFlag = "from"

func newDraftReplyCommand() *cobra.Command {
	command := &cobra.Command{
		Use:     "draft-reply <message-id>",
		Short:   "Create an IMAP or Gmail reply draft from an archived message",
		Long:    "Create a reply to an internal message ID returned by search. Select a configured draft source with --account or --source-id and supply the reply using --body. This creates a draft for review; it does not send the reply.",
		Example: "  msgvault draft reply 1234 --body \"Thanks; I will review it.\" --json",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("body") {
				return usageErr(cmd, errDraftReplyBodyRequired)
			}
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		},
	}
	command.Flags().String(draftFromFlag, "", "confirmed source identity for the draft")
	command.Flags().String("body", "", "reply body")
	command.Flags().Bool("all", false, "reply to the parent sender and visible recipients")
	command.Flags().String("account", "", "destination source account or display name")
	command.Flags().Int64("source-id", 0, "exact destination source ID")
	command.Flags().Bool("json", false, "emit one JSON result")
	return command
}
