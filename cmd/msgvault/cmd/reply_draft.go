package cmd

import "github.com/spf13/cobra"

func init() {
	rootCmd.AddCommand(newDraftReplyCommand())
}

const draftFromFlag = "from"

func newDraftReplyCommand() *cobra.Command {
	command := &cobra.Command{
		Use:     "draft-reply <message-id>",
		Short:   "Create a reply draft from an archived message",
		Long:    "Create a managed reply draft from an archived message. The parent message determines the source; use --body for the text. This command prepares a draft and never sends it.",
		Example: "  msgvault draft reply 55102 --body 'Thanks for the details.' --json",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("body") {
				return xCommandUsageErr(cmd, errDraftReplyBodyRequired, "run 'msgvault draft reply <message-id> --body <text>' to create a managed reply draft")
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
