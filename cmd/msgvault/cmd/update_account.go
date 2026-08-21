package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
)

var (
	updateDisplayName     string
	updateAccountSourceID int64
)

var updateAccountCmd = newUpdateAccountCmd()

func newUpdateAccountCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update-account [account]",
		Short: "Update account settings",
		Long: `Update settings for an existing account.

Currently supports updating the display name for an account.

Examples:
  msgvault update-account you@gmail.com --display-name "Work"
  msgvault update-account --source-id 42 --display-name "Personal Email"`,
		Args: cobra.MaximumNArgs(1),
		RunE: runUpdateAccount,
	}
	cmd.Flags().StringVar(&updateDisplayName, "display-name", "", "Set the display name for the account")
	cmd.Flags().Int64Var(&updateAccountSourceID, "source-id", 0, "Exact source ID to update")
	return cmd
}

func runUpdateAccount(cmd *cobra.Command, args []string) error {
	if updateDisplayName == "" {
		return usageErr(cmd, errors.New("nothing to update: use --display-name to set a display name"))
	}
	account := ""
	if len(args) == 1 {
		account = args[0]
	}
	sourceIDSet := cmd.Flags().Changed("source-id")
	switch {
	case sourceIDSet && updateAccountSourceID <= 0:
		return usageErr(cmd, errors.New("source ID must be positive"))
	case sourceIDSet && account != "":
		return usageErr(cmd, errors.New("account and source ID are mutually exclusive"))
	case !sourceIDSet && account == "":
		return usageErr(cmd, errors.New("account or source ID is required"))
	}

	st, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	result, err := st.UpdateCLIAccount(cmd.Context(), daemonclient.CLIAccountUpdateRequest{
		Email:       account,
		SourceID:    updateAccountSourceID,
		SourceIDSet: sourceIDSet,
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
}
