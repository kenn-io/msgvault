package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/wesm/msgvault/internal/query"
	"github.com/wesm/msgvault/internal/store"
)

var listSendersCmd = &cobra.Command{
	Use:   "list-senders",
	Short: "List top senders by message count",
	Long: `List email senders ranked by message count, size, or attachment size.

Use this command to see who sends you the most email. Results can be filtered
by date range and output as JSON for programmatic use.

Examples:
  msgvault list-senders --limit 20
  msgvault list-senders --after 2024-01-01 --before 2024-06-01
  msgvault list-senders --json`,
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
		results, err := engine.Aggregate(cmd.Context(), query.ViewSenders, opts)
		if err != nil {
			return query.HintRepairEncoding(fmt.Errorf("aggregate by sender: %w", err))
		}

		if len(results) == 0 {
			fmt.Println("No senders found.")
			return nil
		}

		if aggJSON {
			return outputAggregateJSON(results)
		}
		outputAggregateTable(results, "Sender")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(listSendersCmd)
	addCommonAggregateFlags(listSendersCmd)
}
