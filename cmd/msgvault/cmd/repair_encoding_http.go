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

	stderr := &childStderr{w: cmd.ErrOrStderr()}
	err = st.RunCLIRepairEncoding(cmd.Context(), func(stream, data string) error {
		switch stream {
		case cliStreamStdout:
			if _, err := fmt.Fprint(cmd.OutOrStdout(), data); err != nil {
				return fmt.Errorf("write repair-encoding stdout: %w", err)
			}
		case cliStreamStderr:
			if _, err := fmt.Fprint(stderr, data); err != nil {
				return fmt.Errorf("write repair-encoding stderr: %w", err)
			}
		}
		return nil
	})
	if proxy, ok := proxiedChildFailure(stderr, err); ok {
		return proxy
	}
	return err
}
