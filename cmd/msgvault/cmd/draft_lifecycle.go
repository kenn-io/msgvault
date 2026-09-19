package cmd

import "github.com/spf13/cobra"

func init() {
	rootCmd.AddCommand(newDraftGetCommand())
	rootCmd.AddCommand(newDraftEditCommand())
	rootCmd.AddCommand(newDraftDeleteCommand())
}

func newDraftGetCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "draft-get <draft-id>",
		Short: "Read a managed IMAP draft from the archive",
		Args:  cobra.ExactArgs(1),
		RunE:  runDaemonCLICommandHTTPFromCobra,
	}
	command.Flags().Bool("json", false, "emit one JSON result")
	return command
}

func newDraftEditCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "draft-edit <draft-id>",
		Short: "Replace the body of a managed IMAP draft",
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
		Short: "Delete a managed IMAP draft",
		Args:  cobra.ExactArgs(1),
		RunE:  runDaemonCLICommandHTTPFromCobra,
	}
	command.Flags().Int64("revision", 0, "current draft revision")
	_ = command.MarkFlagRequired("revision")
	command.Flags().Bool("json", false, "emit one JSON result")
	return command
}
