package cmd

import (
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/query"
)

var listDomainsCmd = &cobra.Command{
	Use:   "list-domains",
	Short: "List top sender domains by message count",
	Long: `List email sender domains ranked by message count, size, or attachment size.

Use this command to see which domains send you the most email. This is useful
for identifying newsletter subscriptions, mailing lists, or high-volume senders.

Examples:
  msgvault list-domains --limit 20
  msgvault list-domains --after 2024-01-01
  msgvault list-domains --json`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runAggregateListCommand(cmd, query.ViewDomains, "No domains found.", "Domain", "domain")
	},
}

func init() {
	rootCmd.AddCommand(listDomainsCmd)
	addCommonAggregateFlags(listDomainsCmd)
}
