package cmd

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/store"
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

		s, info, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return fmt.Errorf("open store: %w", err)
		}
		defer func() { _ = s.Close() }()

		logger.Info("database initialized successfully")

		result, err := s.InitCLIArchive(cmd.Context())
		if err != nil {
			return fmt.Errorf("init archive: %w", err)
		}
		if result.Notice != "" {
			_, _ = fmt.Fprint(cmd.ErrOrStderr(), result.Notice)
		}

		printInitDBStats(cmd.OutOrStdout(), info, dbPath, result.Stats)

		return nil
	},
}

func printInitDBStats(w io.Writer, info HTTPStoreInfo, dbPath string, stats *store.Stats) {
	if info.Kind == HTTPStoreConfiguredRemote {
		_, _ = fmt.Fprintf(w, "Remote: %s\n", info.URL)
	} else {
		_, _ = fmt.Fprintf(w, "Database: %s\n", dbPath)
	}
	if stats == nil {
		stats = &store.Stats{}
	}
	_, _ = fmt.Fprintf(w, "  Messages:    %d\n", stats.MessageCount)
	_, _ = fmt.Fprintf(w, "  Threads:     %d\n", stats.ThreadCount)
	_, _ = fmt.Fprintf(w, "  Attachments: %d\n", stats.AttachmentCount)
	_, _ = fmt.Fprintf(w, "  Labels:      %d\n", stats.LabelCount)
	_, _ = fmt.Fprintf(w, "  Sources:     %d\n", stats.SourceCount)
	_, _ = fmt.Fprintf(w, "  Size:        %.2f MB\n", float64(stats.DatabaseSize)/(1024*1024))
}

func init() {
	rootCmd.AddCommand(initDBCmd)
}
