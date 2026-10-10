package cmd

import (
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/query"
)

var listSendersCmd = &cobra.Command{
	Use:   "list-senders",
	Short: "List top senders by message count",
	Long: `List email senders ranked by message count, size, or attachment size.

Use this command to see who sends you the most email. Results can be filtered
by date range and output as JSON for programmatic use.

JSON rows have key, count, total_size, and attachment_size.`,
	Example: `  msgvault list-senders -n 20 --after 2026-01-01 --json
  msgvault list-senders --after 2024-01-01 --before 2024-06-01`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runAggregateListCommand(cmd, query.ViewSenders, "No senders found.", "Sender", "sender")
	},
}

func init() {
	rootCmd.AddCommand(listSendersCmd)
	addCommonAggregateFlags(listSendersCmd)
}
