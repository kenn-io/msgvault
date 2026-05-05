package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/wesm/msgvault/internal/query"
	"github.com/wesm/msgvault/internal/store"
)

var listLabelsCmd = &cobra.Command{
	Use:   "list-labels",
	Short: "List all labels with message counts",
	Long: `List all Gmail labels in your archive with message counts and sizes.

Use this command to see how your email is organized by label. This includes
both system labels (INBOX, SENT, etc.) and custom labels.

Examples:
  msgvault list-labels
  msgvault list-labels --limit 50
  msgvault list-labels --json`,
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
		results, err := engine.Aggregate(cmd.Context(), query.ViewLabels, opts)
		if err != nil {
			return query.HintRepairEncoding(fmt.Errorf("aggregate by label: %w", err))
		}

		if len(results) == 0 {
			fmt.Println("No labels found.")
			return nil
		}

		if aggJSON {
			return outputAggregateJSON(results)
		}
		outputAggregateTable(results, "Label")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(listLabelsCmd)
	addCommonAggregateFlags(listLabelsCmd)
}
