package cmd

import (
	"errors"

	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(newChatDraftCreateCommand())
	rootCmd.AddCommand(newChatDraftGetCommand())
	rootCmd.AddCommand(newChatDraftListCommand())
	rootCmd.AddCommand(newChatDraftEditCommand())
	rootCmd.AddCommand(newChatDraftDeleteCommand())
}

func newChatDraftCreateCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "chat-draft-create <conversation-id>",
		Short: "Create a local draft for an archived chat conversation",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("source") == cmd.Flags().Changed("source-id") {
				return usageErr(cmd, errors.New("choose one of --source or --source-id"))
			}
			if !cmd.Flags().Changed("body") {
				return usageErr(cmd, errors.New("--body is required"))
			}
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		},
	}
	command.Flags().String("source", "", "source identifier or display name")
	command.Flags().Int64("source-id", 0, "exact source ID")
	command.Flags().String("body", "", "local draft body")
	command.Flags().Int64("reply-to", 0, "archived message ID to use as the reply target")
	command.Flags().Bool("json", false, "emit one JSON result")
	return command
}

func newChatDraftGetCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "chat-draft-get <draft-id>",
		Short: "Read a local chat draft",
		Args:  cobra.ExactArgs(1),
		RunE:  runDaemonCLICommandHTTPFromCobra,
	}
	command.Flags().Bool("json", false, "emit one JSON result")
	return command
}

func newChatDraftListCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "chat-draft-list <conversation-id>",
		Short: "List local drafts for an archived chat conversation",
		Args:  cobra.ExactArgs(1),
		RunE:  runDaemonCLICommandHTTPFromCobra,
	}
	command.Flags().Bool("json", false, "emit one JSON array")
	return command
}

func newChatDraftEditCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "chat-draft-edit <draft-id>",
		Short: "Replace the body of a local chat draft",
		Args:  cobra.ExactArgs(1),
		RunE:  runDaemonCLICommandHTTPFromCobra,
	}
	command.Flags().Int64("revision", 0, "current draft revision")
	command.Flags().String("body", "", "replacement local draft body")
	command.Flags().Bool("json", false, "emit one JSON result")
	_ = command.MarkFlagRequired("revision")
	_ = command.MarkFlagRequired("body")
	return command
}

func newChatDraftDeleteCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "chat-draft-delete <draft-id>",
		Short: "Discard a local chat draft",
		Args:  cobra.ExactArgs(1),
		RunE:  runDaemonCLICommandHTTPFromCobra,
	}
	command.Flags().Int64("revision", 0, "current draft revision")
	command.Flags().Bool("json", false, "emit one JSON result")
	_ = command.MarkFlagRequired("revision")
	return command
}
