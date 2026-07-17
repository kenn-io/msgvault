package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
)

var updateDisplayName string

var updateAccountCmd = &cobra.Command{
	Use:   "update-account <email>",
	Short: "Update account settings",
	Long: `Update settings for an existing account.

Currently supports updating the display name for an account.

Examples:
  msgvault update-account you@gmail.com --display-name "Work"
  msgvault update-account you@gmail.com --display-name "Personal Email"`,
	Args: cobra.ExactArgs(1),
	RunE: runUpdateAccount,
}

func runUpdateAccount(cmd *cobra.Command, args []string) error {
	email := args[0]

	if updateDisplayName == "" {
		return usageErr(cmd, errors.New("nothing to update: use --display-name to set a display name"))
	}

	st, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	result, err := st.UpdateCLIAccount(cmd.Context(), daemonclient.CLIAccountUpdateRequest{
		Email:       email,
		DisplayName: updateDisplayName,
	})
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Updated account %s: display name set to %q\n",
		result.Email, result.DisplayName)
	return nil
}

func init() {
	rootCmd.AddCommand(updateAccountCmd)
	updateAccountCmd.Flags().StringVar(&updateDisplayName, "display-name", "", "Set the display name for the account")
}
