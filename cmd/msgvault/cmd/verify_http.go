package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
)

func runVerifyHTTP(cmd *cobra.Command, email string) error {
	req := daemonclient.CLIVerifyRequest{
		Email:       email,
		SampleSize:  verifySampleSize,
		SkipDBCheck: verifySkipDBCheck,
		JSON:        verifyJSON,
	}
	st, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	return st.RunCLIVerify(cmd.Context(), req, func(stream, data string) error {
		switch stream {
		case cliStreamStdout:
			if _, err := fmt.Fprint(cmd.OutOrStdout(), data); err != nil {
				return fmt.Errorf("write verify stdout: %w", err)
			}
		case cliStreamStderr:
			if _, err := fmt.Fprint(cmd.ErrOrStderr(), data); err != nil {
				return fmt.Errorf("write verify stderr: %w", err)
			}
		}
		return nil
	})
}
