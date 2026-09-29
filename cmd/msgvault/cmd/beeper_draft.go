package cmd

import "github.com/spf13/cobra"

func init() {
	rootCmd.AddCommand(newBeeperDraftCommand())
}

func newBeeperDraftCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "draft-beeper",
		Short: "Manage native Beeper chat drafts",
		Long:  "Manage text drafts in existing Beeper chats. Edit and clear compare the observed draft first; an edit made after that observation can still race an unconditional provider clear.",
	}
	command.AddCommand(newBeeperDraftCreateCommand())
	command.AddCommand(newBeeperDraftGetCommand())
	command.AddCommand(newBeeperDraftEditCommand())
	command.AddCommand(newBeeperDraftClearCommand())
	return command
}

func newBeeperDraftCreateCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "create",
		Short: "Create a text draft in an existing Beeper chat",
		Args:  cobra.NoArgs,
		RunE:  runDaemonCLICommandHTTPFromCobra,
	}
	command.Flags().Int64("source-id", 0, "Beeper source ID")
	command.Flags().String("chat-id", "", "canonical Beeper chat ID")
	command.Flags().String("body", "", "draft text")
	command.Flags().Bool("json", false, "emit one JSON result")
	_ = command.MarkFlagRequired("source-id")
	_ = command.MarkFlagRequired("chat-id")
	_ = command.MarkFlagRequired("body")
	return command
}

func newBeeperDraftGetCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "get <draft-id>",
		Short: "Read a managed Beeper draft",
		Args:  cobra.ExactArgs(1),
		RunE:  runDaemonCLICommandHTTPFromCobra,
	}
	command.Flags().Bool("json", false, "emit one JSON result")
	return command
}

func newBeeperDraftEditCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "edit <draft-id>",
		Short: "Replace a managed Beeper draft",
		Args:  cobra.ExactArgs(1),
		RunE:  runDaemonCLICommandHTTPFromCobra,
	}
	command.Flags().Int64("revision", 0, "current draft revision")
	command.Flags().String("body", "", "replacement draft text")
	command.Flags().Bool("json", false, "emit one JSON result")
	_ = command.MarkFlagRequired("revision")
	_ = command.MarkFlagRequired("body")
	return command
}

func newBeeperDraftClearCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "clear <draft-id>",
		Short: "Clear a managed Beeper draft",
		Args:  cobra.ExactArgs(1),
		RunE:  runDaemonCLICommandHTTPFromCobra,
	}
	command.Flags().Int64("revision", 0, "current draft revision")
	command.Flags().Bool("json", false, "emit one JSON result")
	_ = command.MarkFlagRequired("revision")
	return command
}
