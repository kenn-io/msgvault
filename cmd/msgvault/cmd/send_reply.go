package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/daemonclient"
)

func init() { rootCmd.AddCommand(newSendReplyCommand()) }
func sendReplyCLIArgs(cmd *cobra.Command, args []string) []string {
	out := append([]string{"send-reply"}, args...)
	for _, name := range []string{"draft", "revision", "body", "adopt-post", "json"} {
		if f := cmd.Flags().Lookup(name); f.Changed {
			out = append(out, "--"+name+"="+f.Value.String())
		}
	}
	return out
}
func newSendReplyCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use: "send-reply [archived-message-id]", Short: "Send a reviewed reply",
		Long: `Send an owner-authorized reply through the selected daemon. The archived
parent or managed draft determines the source. Only X replies are supported.
Use --draft with its exact --revision, or an archived message ID with --body.
Direct mode first reports a durable draft handle; retain it for recovery.
A new X send requires [[x.sends]] opt-in, tweet.write and a reservation of at
least USD 0.200000, including URLs. X enforces text limits and reply eligibility.
remote_unknown means X may have accepted the reply. Do not create another draft
to retry. Inspect that same draft/revision or explicitly --adopt-post its known
remote ID; adoption performs a budgeted read and never posts. Accepted local
failures can be repaired by repeating the same managed draft/revision.`,
		Args: func(cmd *cobra.Command, args []string) error {
			_, err := api.ParseCLIRunSendReply(api.CLIRunRequest{Args: sendReplyCLIArgs(cmd, args)})
			return err
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			state := invocationFromContext(cmd.Context())
			if state == nil || state.cfg == nil {
				return errors.New("reply sending requires configuration")
			}
			if isAgentMode(state) {
				return errors.New("reply sending requires owner authority")
			}
			client, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			return client.RunCLICommand(cmd.Context(), daemonclient.CLIRunRequest{Args: sendReplyCLIArgs(cmd, args)}, func(stream, data string) error {
				switch stream {
				case cliStreamStdout:
					_, err := fmt.Fprint(cmd.OutOrStdout(), data)
					return err
				case cliStreamStderr:
					_, err := fmt.Fprint(cmd.ErrOrStderr(), data)
					return err
				}
				return nil
			})
		},
	}
	cmd.Flags().String("draft", "", "Managed chat draft ID")
	cmd.Flags().String("revision", "", "Exact reviewed draft revision")
	cmd.Flags().String("body", "", "Reply body for direct mode")
	cmd.Flags().String("adopt-post", "", "Explicit remote ID to associate with an uncertain attempt; sends nothing")
	cmd.Flags().Bool("json", false, "Print exact status and durable recovery handles as JSON")
	return cmd
}
