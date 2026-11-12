package cmd

import (
	"errors"

	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(newDraftGetCommand())
	rootCmd.AddCommand(newDraftEditCommand())
	rootCmd.AddCommand(newDraftDeleteCommand())
	rootCmd.AddCommand(newDraftRecoverCommand())
	rootCmd.AddCommand(newDraftSendAsCommand())
}

func newDraftGetCommand() *cobra.Command {
	command := &cobra.Command{
		Use:     "draft-get (<draft-id> | --conversation <conversation-id>)",
		Short:   "Read a managed draft or list a chat's local drafts",
		Long:    "Read one managed draft by its returned draft ID, or list local drafts for an archived chat using --conversation. The returned revision is required when editing or deleting that draft.",
		Example: "  msgvault draft get draft-example --json\n  msgvault draft get --conversation 42 --json",
		Args: func(cmd *cobra.Command, args []string) error {
			hasDraft, hasConversation := len(args) > 0, cmd.Flags().Changed("conversation")
			if hasDraft == hasConversation {
				return usageErr(cmd, errors.New("pass a draft ID or --conversation, not both"))
			}
			if cmd.Flags().Changed("conversation") {
				conversation, _ := cmd.Flags().GetInt64("conversation")
				if conversation <= 0 {
					return validationErr(cmd, errors.New("--conversation must be a positive archived conversation ID"))
				}
			}
			return cobra.MaximumNArgs(1)(cmd, args)
		},
		RunE: runDaemonCLICommandHTTPFromCobra,
	}
	command.Flags().Int64("conversation", 0, "list local drafts for this chat conversation")
	command.Flags().Bool("json", false, "emit one JSON result")
	return command
}

func newDraftEditCommand() *cobra.Command {
	command := &cobra.Command{
		Use:     "draft-edit <draft-id>",
		Short:   "Replace the body of a managed draft",
		Long:    "Replace a draft body using the current revision returned by draft get. A stale revision prevents overwriting a newer edit. Read the draft again before retrying a revision conflict.",
		Example: "  msgvault draft edit draft-example --revision 1 --body \"Updated reply.\" --json",
		Args:    cobra.ExactArgs(1),
		RunE:    runDaemonCLICommandHTTPFromCobra,
	}
	command.Flags().Int64("revision", 0, "current draft revision")
	command.Flags().String("body", "", "replacement plain-text body")
	_ = command.MarkFlagRequired("revision")
	_ = command.MarkFlagRequired("body")
	command.Flags().Bool("json", false, "emit one JSON result")
	return command
}

func newDraftDeleteCommand() *cobra.Command {
	command := &cobra.Command{
		Use:     "draft-delete <draft-id>",
		Short:   "Delete a managed draft",
		Long:    "Delete the managed draft at its current revision. This removes the draft, rather than deleting the archived message used to create it. Read draft get first to obtain the revision.",
		Example: "  msgvault draft delete draft-example --revision 1 --json",
		Args:    cobra.ExactArgs(1),
		RunE:    runDaemonCLICommandHTTPFromCobra,
	}
	command.Flags().Int64("revision", 0, "current draft revision")
	_ = command.MarkFlagRequired("revision")
	command.Flags().Bool("json", false, "emit one JSON result")
	return command
}

func newDraftRecoverCommand() *cobra.Command {
	command := &cobra.Command{
		Use:     "draft-recover <draft-id>",
		Short:   "Recover an interrupted managed IMAP draft edit or delete",
		Long:    "Reconcile an IMAP draft whose edit or delete was interrupted. Use the draft ID and current revision from draft get; recovery checks provider state before completing the pending action.",
		Example: "  msgvault draft recover draft-example --revision 1 --json",
		Args:    cobra.ExactArgs(1),
		RunE:    runDaemonCLICommandHTTPFromCobra,
	}
	command.Flags().Int64("revision", 0, "current draft revision")
	_ = command.MarkFlagRequired("revision")
	command.Flags().Bool("json", false, "emit one JSON result")
	return command
}

func newDraftSendAsCommand() *cobra.Command {
	command := &cobra.Command{
		Use:     "draft-send-as <account>",
		Short:   "List Gmail send-as identities",
		Long:    "List the Gmail sender identities available for the account. Use a returned identity with --from when composing or replying, subject to the source draft grant.",
		Example: "  msgvault draft send-as you@example.com --json",
		Args:    cobra.ExactArgs(1),
		RunE:    runDaemonCLICommandHTTPFromCobra,
	}
	command.Flags().Bool("json", false, "emit one JSON result")
	return command
}
