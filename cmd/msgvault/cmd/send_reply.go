package cmd

import (
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/api"
)

func init() { rootCmd.AddCommand(newSendReplyCommand()) }

func newSendReplyCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "send-reply [archived-message-id]",
		Short: "Send a reply to an archived message or reviewed draft",
		Long:  "Send a reply using the source of the archived message or reviewed local draft. The source owner must enable sending.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			argv, err := daemonCLIArgsFromCobra(cmd, args)
			if err != nil {
				return err
			}
			if _, err := api.ParseCLIRunSendReply(api.CLIRunRequest{Args: argv}); err != nil {
				return usageErr(cmd, err)
			}
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		},
	}
	command.Flags().String("draft", "", "Reviewed local chat draft ID")
	command.Flags().Int64("revision", 0, "Reviewed draft revision")
	command.Flags().String("body", "", "Reply text for a direct send")
	command.Flags().String("adopt-media-id", "", "Explicit own-reply ID for unknown-send recovery")
	command.Flags().Bool("quota", false, "Inspect reply quota for the archived message's source")
	command.Flags().Bool("json", false, "Emit one structured result")
	return command
}
