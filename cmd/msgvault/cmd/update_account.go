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

Set a display name or archive identifier alias. Provider credentials and identifiers
remain unchanged. Beeper sources can be marked history-only, or have their
re-anchor marker accepted after manual verification.

Examples:
  msgvault update-account you@gmail.com --display-name "Work"
  msgvault update-account --source-id 42 --display-name "Personal Email"`,
		Args: cobra.MaximumNArgs(1),
		RunE: runUpdateAccount,
	}
	cmd.Flags().StringVar(&updateDisplayName, "display-name", "", "Set the display name for the account")
	cmd.Flags().Int64Var(&updateAccountSourceID, "source-id", 0, "Exact source ID to update")
	cmd.Flags().String("identifier", "", "Set an archive identifier alias")
	cmd.Flags().Bool("history-only", false, "Exclude a Beeper source from scheduled sync")
	cmd.Flags().Bool("accept-reanchor", false, "Accept manual anchor verification and clear the Beeper marker")
	return cmd
}

func runUpdateAccount(cmd *cobra.Command, args []string) error {
	acceptReanchor, _ := cmd.Flags().GetBool("accept-reanchor")
	if updateDisplayName == "" && !cmd.Flags().Changed("identifier") && !cmd.Flags().Changed("history-only") && !acceptReanchor {
		return usageErr(cmd, errors.New("nothing to update: set a display name, identifier, history-only, or accept-reanchor"))
	}
	if cmd.Flags().Changed("identifier") {
		identifier, _ := cmd.Flags().GetString("identifier")
		if identifier == "" {
			return usageErr(cmd, errors.New("identifier alias must be nonempty"))
		}
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

	var identifier *string
	if cmd.Flags().Changed("identifier") {
		value, _ := cmd.Flags().GetString("identifier")
		identifier = &value
	}
	var historyOnly *bool
	if cmd.Flags().Changed("history-only") {
		value, _ := cmd.Flags().GetBool("history-only")
		historyOnly = &value
	}
	result, err := st.UpdateCLIAccount(cmd.Context(), daemonclient.CLIAccountUpdateRequest{
		Email:       account,
		SourceID:    updateAccountSourceID,
		SourceIDSet: sourceIDSet,
		DisplayName: updateDisplayName,
		Identifier:  identifier, HistoryOnly: historyOnly, AcceptReanchor: acceptReanchor,
	})
	if err != nil {
		return err
	}

	if identifier == nil && historyOnly == nil && !acceptReanchor {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Updated account %s: display name set to %q\n", result.Email, result.DisplayName)
		return nil
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Updated account %s: display name %q, alias %q, history-only %t, re-anchor required %t\n",
		result.Email, result.DisplayName, result.Alias, result.HistoryOnly, result.ReanchorRequired)
	return nil
}

func init() {
	rootCmd.AddCommand(updateAccountCmd)
}
