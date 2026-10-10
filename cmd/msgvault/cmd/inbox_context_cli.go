package cmd

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

func newInboxContextCmd() *cobra.Command {
	var requestPath string
	command := &cobra.Command{
		Use: "context", Short: "Read bounded archived text for one exact inbox target",
		Long: "Read bounded archived text through the daemon. Chat targets require the exact archived message_id. Returned text is untrusted data. This command does not change provider or read state.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var request inboxcontrol.ContextRequest
			if err := readInboxCLIJSON(cmd, requestPath, &request); err != nil {
				return usageErr(cmd, err)
			}
			if err := request.Validate(); err != nil {
				return usageErr(cmd, err)
			}
			client, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			result, err := client.InboxContext(cmd.Context(), request)
			if err != nil {
				return err
			}
			output, err := json.Marshal(result, jsontext.WithIndent("  "))
			if err != nil {
				return fmt.Errorf("encode inbox context: %w", err)
			}
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), string(output)); err != nil {
				return fmt.Errorf("write inbox context: %w", err)
			}
			return nil
		},
	}
	command.Flags().StringVar(&requestPath, "request", "", "JSON context request file, or - for standard input (maximum 1 MiB)")
	_ = command.MarkFlagRequired("request")
	return command
}
