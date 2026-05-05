package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/wesm/msgvault/internal/store"
)

var (
	statsAccount    string
	statsCollection string
)

var statsCmd = &cobra.Command{
	Use:   "stats",
	Short: "Show database statistics",
	Long: `Show statistics about the email archive.

Uses remote server if [remote].url is configured, otherwise uses local database.
Use --local to force local database.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		scoped := statsAccount != "" || statsCollection != ""

		if IsRemoteMode() {
			if statsAccount != "" {
				return fmt.Errorf("--account is not supported in remote mode")
			}
			if statsCollection != "" {
				return fmt.Errorf("--collection is not supported in remote mode")
			}
		}

		// Scoped stats require a local store for scope resolution and GetStatsForScope.
		if scoped {
			st, err := openLocalStoreAndInit()
			if err != nil {
				return fmt.Errorf("open store: %w", err)
			}
			defer func() { _ = st.Close() }()

			var scope Scope
			if statsAccount != "" {
				scope, err = ResolveAccountFlag(st, statsAccount)
				if err != nil {
					return err
				}
				if scope.IsEmpty() {
					return fmt.Errorf("--account %q resolved to zero sources", statsAccount)
				}
			} else {
				scope, err = ResolveCollectionFlag(st, statsCollection)
				if err != nil {
					return err
				}
				if scope.IsEmpty() {
					return fmt.Errorf("--collection %q has no member accounts", statsCollection)
				}
			}

			sourceIDs := scope.SourceIDs()
			// A collection with zero member sources resolves to a non-nil
			// Scope (Collection set, Source nil) so IsEmpty above is false,
			// but SourceIDs is empty. GetStatsForScope treats an empty
			// slice as "unscoped" and would silently return archive-wide
			// counts. Reject explicitly with the same shape as the
			// IsEmpty branch above.
			if len(sourceIDs) == 0 {
				return fmt.Errorf("--collection %q has no member accounts", statsCollection)
			}
			dbStats, err := st.GetStatsForScope(sourceIDs)
			if err != nil {
				logger.Warn("stats failed", "error", err.Error())
				return fmt.Errorf("get stats: %w", err)
			}
			logger.Info("stats",
				"messages", dbStats.MessageCount,
				"threads", dbStats.ThreadCount,
				"attachments", dbStats.AttachmentCount,
				"labels", dbStats.LabelCount,
				"accounts", dbStats.SourceCount,
				"db_bytes", dbStats.DatabaseSize,
			)

			if statsAccount != "" {
				fmt.Printf("Stats for account %q:\n", scope.DisplayName())
			} else {
				n := len(sourceIDs)
				suffix := "s"
				if n == 1 {
					suffix = ""
				}
				fmt.Printf("Stats for collection %q (%d account%s):\n",
					scope.DisplayName(), n, suffix)
			}

			printStats(dbStats)
			fmt.Printf("\nNote: Size is global (not scoped).\n")
			return nil
		}

		// Unscoped: route remote to OpenStore (HTTP path), local to
		// openLocalStoreAndInit so InitSchema and runStartupMigrations
		// run consistently with every other command.
		var (
			s   MessageStore
			err error
		)
		if IsRemoteMode() {
			s, err = OpenStore()
		} else {
			s, err = openLocalStoreAndInit()
		}
		if err != nil {
			return fmt.Errorf("open store: %w", err)
		}
		defer func() { _ = s.Close() }()

		dbStats, err := s.GetStats()
		if err != nil {
			logger.Warn("stats failed", "error", err.Error())
			return fmt.Errorf("get stats: %w", err)
		}
		logger.Info("stats",
			"messages", dbStats.MessageCount,
			"threads", dbStats.ThreadCount,
			"attachments", dbStats.AttachmentCount,
			"labels", dbStats.LabelCount,
			"accounts", dbStats.SourceCount,
			"db_bytes", dbStats.DatabaseSize,
		)

		if IsRemoteMode() {
			fmt.Printf("Remote: %s\n", cfg.Remote.URL)
		} else {
			fmt.Printf("Database: %s\n", cfg.DatabaseDSN())
		}

		printStats(dbStats)
		return nil
	},
}

func printStats(s *store.Stats) {
	fmt.Printf("  Messages:    %d\n", s.MessageCount)
	fmt.Printf("  Threads:     %d\n", s.ThreadCount)
	fmt.Printf("  Attachments: %d\n", s.AttachmentCount)
	fmt.Printf("  Labels:      %d\n", s.LabelCount)
	fmt.Printf("  Accounts:    %d\n", s.SourceCount)
	fmt.Printf("  Size:        %.2f MB\n", float64(s.DatabaseSize)/(1024*1024))
}

func init() {
	rootCmd.AddCommand(statsCmd)
	statsCmd.Flags().StringVar(&statsAccount, "account", "", "Show stats for a specific account")
	statsCmd.Flags().StringVar(&statsCollection, "collection", "",
		"Show stats for all member accounts of one collection")
	statsCmd.MarkFlagsMutuallyExclusive("account", "collection")
}
