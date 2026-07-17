package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

func runRepairEncodingHTTP(cmd *cobra.Command) error {
	st, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	return st.RunCLIRepairEncoding(cmd.Context(), func(stream, data string) error {
		switch stream {
		case cliStreamStdout:
			if _, err := fmt.Fprint(cmd.OutOrStdout(), data); err != nil {
				return fmt.Errorf("write repair-encoding stdout: %w", err)
			}
		case cliStreamStderr:
			if _, err := fmt.Fprint(cmd.ErrOrStderr(), data); err != nil {
				return fmt.Errorf("write repair-encoding stderr: %w", err)
			}
		}
		return nil
	})
}
