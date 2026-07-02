package cmd

import (
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/query"
)

var listLabelsCmd = &cobra.Command{
	Use:   "list-labels",
	Short: "List all labels with message counts",
	Long: `List all Gmail labels in your archive with message counts and sizes.

Use this command to see how your email is organized by label. This includes
both system labels (INBOX, SENT, etc.) and custom labels.

Counts come from the analytics cache and include messages deleted from their
source account (the archive retains them), so per-label totals may exceed the
active message count shown by 'stats'.

Examples:
  msgvault list-labels
  msgvault list-labels --limit 50
  msgvault list-labels --json`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runAggregateListCommand(cmd, query.ViewLabels, "No labels found.", "Label", "label")
	},
}

func init() {
	rootCmd.AddCommand(listLabelsCmd)
	addCommonAggregateFlags(listLabelsCmd)
}
