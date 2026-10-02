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
		Use:   "draft-get (<draft-id> | --conversation <conversation-id>)",
		Short: "Read a managed draft, or list a chat conversation's local drafts",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 && !cmd.Flags().Changed("conversation") {
				return errors.New("requires a draft ID or --conversation")
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
		Use:   "draft-edit <draft-id>",
		Short: "Replace the body of a managed draft",
		Args:  cobra.ExactArgs(1),
		RunE:  runDaemonCLICommandHTTPFromCobra,
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
		Use:   "draft-delete <draft-id>",
		Short: "Delete a managed draft",
		Args:  cobra.ExactArgs(1),
		RunE:  runDaemonCLICommandHTTPFromCobra,
	}
	command.Flags().Int64("revision", 0, "current draft revision")
	_ = command.MarkFlagRequired("revision")
	command.Flags().Bool("json", false, "emit one JSON result")
	return command
}

func newDraftRecoverCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "draft-recover <draft-id>",
		Short: "Recover an interrupted managed IMAP draft edit or delete",
		Args:  cobra.ExactArgs(1),
		RunE:  runDaemonCLICommandHTTPFromCobra,
	}
	command.Flags().Int64("revision", 0, "current draft revision")
	_ = command.MarkFlagRequired("revision")
	command.Flags().Bool("json", false, "emit one JSON result")
	return command
}

func newDraftSendAsCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "draft-send-as <account>",
		Short: "List or confirm Gmail send-as identities",
		Args:  cobra.ExactArgs(1),
		RunE:  runDaemonCLICommandHTTPFromCobra,
	}
	command.Flags().StringArray("confirm", nil, "confirm a primary or accepted Gmail sender locally, repeatable")
	command.Flags().Bool("json", false, "emit one JSON result")
	return command
}
