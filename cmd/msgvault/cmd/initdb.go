package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/wesm/msgvault/internal/store"
)

var initDBCmd = &cobra.Command{
	Use:   "init-db",
	Short: "Initialize the database schema",
	Long: `Initialize the msgvault database with the required schema.

This command creates all necessary tables for storing emails, attachments,
labels, and sync state. It is safe to run multiple times - tables are only
created if they don't already exist.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath := cfg.DatabaseDSN()
		logger.Info("initializing database", "path", dbPath)

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

		logger.Info("database initialized successfully")

		// Print stats
		stats, err := s.GetStats()
		if err != nil {
			return fmt.Errorf("get stats: %w", err)
		}

		fmt.Printf("Database: %s\n", dbPath)
		fmt.Printf("  Messages:    %d\n", stats.MessageCount)
		fmt.Printf("  Threads:     %d\n", stats.ThreadCount)
		fmt.Printf("  Attachments: %d\n", stats.AttachmentCount)
		fmt.Printf("  Labels:      %d\n", stats.LabelCount)
		fmt.Printf("  Sources:     %d\n", stats.SourceCount)
		fmt.Printf("  Size:        %.2f MB\n", float64(stats.DatabaseSize)/(1024*1024))

		return nil
	},
}

func init() {
	rootCmd.AddCommand(initDBCmd)
}
