package cmd

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

const maxInboxCLIRequest = 1 << 20

func init() { rootCmd.AddCommand(newInboxCmd()) }

func newInboxCmd() *cobra.Command {
	parent := &cobra.Command{Use: "inbox", Short: "Observe and control exact native inbox items through the daemon", Long: "Read exact inbox metadata, preview native actions, and retrieve durable receipts. Mutation commands preview by default. Execute with --apply and the expected state, signed preview token, and idempotency key in the request. No command sends a message."}
	for _, op := range []inboxcontrol.Operation{inboxcontrol.OpGetCapabilities, inboxcontrol.OpGetState, inboxcontrol.OpListFolders, inboxcontrol.OpArchive, inboxcontrol.OpUnarchive, inboxcontrol.OpSetRead, inboxcontrol.OpSetUnread, inboxcontrol.OpMove, inboxcontrol.OpTags, inboxcontrol.OpCreateFolder, inboxcontrol.OpReceiptGet, inboxcontrol.OpReconcile} {
		var requestPath string
		var apply bool
		command := &cobra.Command{Use: string(op), Short: "Run one exact inbox " + string(op) + " request", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			if apply && !op.IsMutation() {
				return usageErr(cmd, fmt.Errorf("%w: --apply requires a mutation command", inboxcontrol.ErrInvalid))
			}
			request, err := readInboxCLIRequest(cmd, requestPath)
			if err != nil {
				return usageErr(cmd, err)
			}
			if request.Operation != "" && request.Operation != op {
				return usageErr(cmd, fmt.Errorf("%w: request operation differs from the command", inboxcontrol.ErrInvalid))
			}
			request.Operation = op
			if op.IsMutation() {
				request.DryRun = !apply
			}
			if err := request.Validate(); err != nil {
				return usageErr(cmd, err)
			}
			client, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			result, callErr := client.ControlInbox(cmd.Context(), request)
			if result != nil {
				output, err := json.Marshal(result, jsontext.WithIndent("  "))
				if err == nil {
					err = writeInboxCLIOutputLine(cmd, output)
				}
				if err != nil {
					if op.IsMutation() && apply {
						return fmt.Errorf("%w: could not output the daemon receipt", inboxcontrol.ErrOutcomeUnknown)
					}
					return fmt.Errorf("write inbox result: %w", err)
				}
			}
			return callErr
		}}
		command.Flags().StringVar(&requestPath, "request", "", "JSON request file, or - to read standard input (maximum 1 MiB)")
		command.Flags().BoolVar(&apply, "apply", false, "Execute a mutation using the signed preview and expected state in the request")
		_ = command.MarkFlagRequired("request")
		parent.AddCommand(command)
	}
	parent.AddCommand(newInboxCandidatesCmd())
	parent.AddCommand(newInboxContextCmd())
	parent.AddCommand(newInboxTriageCmd())
	return parent
}
func readInboxCLIRequest(cmd *cobra.Command, path string) (inboxcontrol.Request, error) {
	var request inboxcontrol.Request
	err := readInboxCLIJSON(cmd, path, &request)
	return request, err
}

func readInboxCLIJSON(cmd *cobra.Command, path string, request any) error {
	invalid := func() error {
		return fmt.Errorf("%w: a bounded JSON request object is required", inboxcontrol.ErrInvalid)
	}
	if path == "" {
		return invalid()
	}
	var reader = cmd.InOrStdin()
	if path != "-" {
		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open inbox request: %w", err)
		}
		defer func() { _ = file.Close() }()
		reader = file
	}
	data, err := io.ReadAll(io.LimitReader(reader, maxInboxCLIRequest+1))
	if err != nil {
		return fmt.Errorf("read inbox request: %w", err)
	}
	if len(data) > maxInboxCLIRequest {
		return invalid()
	}
	if json.Unmarshal(data, request, json.RejectUnknownMembers(true)) != nil {
		return invalid()
	}
	return nil
}

// A broken stdout pipe must return an error so callers can recover committed
// operations by their original keys instead of losing the acknowledgement to SIGPIPE.
func writeInboxCLIOutputLine(cmd *cobra.Command, data []byte) error {
	brokenPipe := make(chan os.Signal, 1)
	signal.Notify(brokenPipe, syscall.SIGPIPE)
	defer signal.Stop(brokenPipe)
	_, err := fmt.Fprintln(cmd.OutOrStdout(), string(data))
	if err != nil {
		return fmt.Errorf("write inbox output: %w", err)
	}
	return nil
}
