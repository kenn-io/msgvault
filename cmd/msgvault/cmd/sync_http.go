package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
)

func runSyncIncrementalHTTP(cmd *cobra.Command, args []string) error {
	req := daemonclient.CLISyncRequest{}
	if len(args) == 1 {
		req.Email = args[0]
	}
	return runSyncHTTP(cmd, req)
}

func runSyncFullHTTP(cmd *cobra.Command, args []string) error {
	req := daemonclient.CLISyncRequest{
		Full:     true,
		Query:    syncQuery,
		NoResume: syncNoResume,
		Before:   syncBefore,
		After:    syncAfter,
		Limit:    syncLimit,
	}
	if len(args) == 1 {
		req.Email = args[0]
	}
	return runSyncHTTP(cmd, req)
}

func runSyncHTTP(cmd *cobra.Command, req daemonclient.CLISyncRequest) error {
	st, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	return st.RunCLISync(cmd.Context(), req, func(stream, data string) error {
		switch stream {
		case cliStreamStdout:
			if _, err := fmt.Fprint(cmd.OutOrStdout(), data); err != nil {
				return fmt.Errorf("write sync stdout: %w", err)
			}
		case cliStreamStderr:
			if _, err := fmt.Fprint(cmd.ErrOrStderr(), data); err != nil {
				return fmt.Errorf("write sync stderr: %w", err)
			}
		}
		return nil
	})
}
