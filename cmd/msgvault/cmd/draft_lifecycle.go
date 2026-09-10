package cmd

import (
	"errors"

	"github.com/spf13/cobra"
)

var (
	errDraftLifecycleRevisionRequired = errors.New("--revision is required")
	errDraftLifecycleBodyRequired     = errors.New("--body is required")
)

func init() {
	rootCmd.AddCommand(newDraftGetCommand())
	rootCmd.AddCommand(newDraftEditCommand())
	rootCmd.AddCommand(newDraftDeleteCommand())
}

func newDraftGetCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "draft-get <draft-id>",
		Short: "Inspect a locally tracked IMAP draft and its remote state",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		},
	}
	command.Flags().Bool("json", false, "emit one JSON result")
	return command
}

func newDraftEditCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "draft-edit <draft-id>",
		Short: "Replace the body of an IMAP draft and advance its revision",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("resume") {
				if !cmd.Flags().Changed("revision") {
					return usageErr(cmd, errDraftLifecycleRevisionRequired)
				}
				if !cmd.Flags().Changed("body") {
					return usageErr(cmd, errDraftLifecycleBodyRequired)
				}
			}
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		},
	}
	command.Flags().String("revision", "", "expected current revision (required)")
	command.Flags().String("body", "", "new body text for the draft")
	command.Flags().Bool("resume", false, "resume a pending edit operation")
	command.Flags().Bool("json", false, "emit one JSON result")
	return command
}

func newDraftDeleteCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "draft-delete <draft-id>",
		Short: "Permanently delete an IMAP draft from the remote server",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("revision") && !cmd.Flags().Changed("resume") {
				return usageErr(cmd, errDraftLifecycleRevisionRequired)
			}
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		},
	}
	command.Flags().String("revision", "", "expected current revision (required unless --resume)")
	command.Flags().Bool("resume", false, "resume a pending delete operation")
	command.Flags().Bool("json", false, "emit one JSON result")
	return command
}
