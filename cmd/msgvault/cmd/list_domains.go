package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/wesm/msgvault/internal/query"
	"github.com/wesm/msgvault/internal/store"
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
	RunE: func(cmd *cobra.Command, args []string) error {
		opts, err := parseCommonFlags()
		if err != nil {
			return err
		}

		// Open database
		dbPath := cfg.DatabaseDSN()
		s, err := store.Open(dbPath)
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer func() { _ = s.Close() }()

		if err := s.InitSchema(); err != nil {
			return fmt.Errorf("init schema: %w", err)
		}
		if err := runStartupMigrations(s); err != nil {
			return fmt.Errorf("startup migrations: %w", err)
		}

		// Create query engine
		engine := query.NewSQLiteEngine(s.DB())

		// Execute aggregation
		results, err := engine.Aggregate(cmd.Context(), query.ViewDomains, opts)
		if err != nil {
			return query.HintRepairEncoding(fmt.Errorf("aggregate by domain: %w", err))
		}

		if len(results) == 0 {
			fmt.Println("No domains found.")
			return nil
		}

		if aggJSON {
			return outputAggregateJSON(results)
		}
		outputAggregateTable(results, "Domain")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(listDomainsCmd)
	addCommonAggregateFlags(listDomainsCmd)
}
