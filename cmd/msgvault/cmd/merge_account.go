package cmd

import (
	"encoding/json/v2"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/accountops"
)

func newMergeAccountCmd() *cobra.Command {
	var request accountops.MergeRequest
	var jsonOutput bool
	var confirmed bool
	cmd := &cobra.Command{
		Use:   "merge-account --from <account> --into <account>",
		Short: "Merge archived accounts and retire the historical source",
		Long:  "Move archived messages into a compatible account, preserve original evidence, and hide only unambiguous duplicates. Use --dry-run to preview the checks and counts. A merge retires the historical source, so preview first, then repeat with --yes to apply it.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			request.DryRunSet = true
			request.FromSourceIDSet = cmd.Flags().Changed("from-source-id")
			request.IntoSourceIDSet = cmd.Flags().Changed("into-source-id")
			if !request.DryRun && !confirmed {
				return errors.New("merge retires the historical source; preview with --dry-run, then repeat with --yes to apply")
			}
			st, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			result, err := st.MergeCLIAccounts(cmd.Context(), request)
			if err != nil {
				return err
			}
			if jsonOutput {
				data, err := json.Marshal(result)
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
				if err != nil {
					return fmt.Errorf("write merge report: %w", err)
				}
				return nil
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Source %d into %d: %d messages moved, %d duplicates hidden, %d ambiguous matches preserved (dry-run: %t, already merged: %t)\n", result.FromSourceID, result.IntoSourceID, result.MessagesMoved, result.DuplicatesHidden, result.AmbiguousMatches, result.DryRun, result.AlreadyMerged)
			if err != nil {
				return fmt.Errorf("write merge report: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&request.From, "from", "", "Historical account identifier or alias")
	cmd.Flags().StringVar(&request.Into, "into", "", "Destination account identifier or alias")
	cmd.Flags().Int64Var(&request.FromSourceID, "from-source-id", 0, "Exact historical source ID")
	cmd.Flags().Int64Var(&request.IntoSourceID, "into-source-id", 0, "Exact destination source ID")
	cmd.Flags().BoolVar(&request.DryRun, "dry-run", false, "Preview without changing the archive")
	cmd.Flags().BoolVar(&confirmed, "yes", false, "Confirm retirement of the historical source")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output the merge report as JSON")
	return cmd
}

func init() { rootCmd.AddCommand(newMergeAccountCmd()) }
