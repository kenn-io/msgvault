package cmd

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/store"
)

var (
	statsAccount    string
	statsCollection string
)

var statsCmd = &cobra.Command{
	Use:   "stats",
	Short: "Show database statistics",
	Long: `Show statistics about the email archive.

Uses configured remote server or the local daemon by default.
Use --local to use the local daemon even when a remote is configured.`,
	RunE: runStats,
}

func runStats(cmd *cobra.Command, _ []string) error {
	out := cmd.OutOrStdout()
	scoped := statsAccount != "" || statsCollection != ""

	if scoped {
		return runHTTPScopedStats(cmd, out)
	}

	s, info, err := OpenHTTPStore(cmd.Context())
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
		tableMessages, dbStats.MessageCount,
		"threads", dbStats.ThreadCount,
		tableAttachments, dbStats.AttachmentCount,
		tableLabels, dbStats.LabelCount,
		"accounts", dbStats.SourceCount,
		"db_bytes", dbStats.DatabaseSize,
	)

	if info.Kind == HTTPStoreConfiguredRemote {
		_, _ = fmt.Fprintf(out, "Remote: %s\n", info.URL)
	} else {
		_, _ = fmt.Fprintf(out, "Database: %s\n", cfg.DatabaseDSN())
	}

	printStats(out, dbStats)
	return nil
}

func runHTTPScopedStats(cmd *cobra.Command, out io.Writer) error {
	s, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = s.Close() }()

	resp, err := s.GetCLIStats(cmd.Context(), statsAccount, statsCollection)
	if err != nil {
		logger.Warn("stats failed", "error", err.Error())
		return fmt.Errorf("get stats: %w", err)
	}
	logger.Info("stats",
		tableMessages, resp.Stats.MessageCount,
		"threads", resp.Stats.ThreadCount,
		tableAttachments, resp.Stats.AttachmentCount,
		tableLabels, resp.Stats.LabelCount,
		"accounts", resp.Stats.SourceCount,
		"db_bytes", resp.Stats.DatabaseSize,
	)

	label := resp.ScopeLabel
	if label == "" {
		if statsAccount != "" {
			label = statsAccount
		} else {
			label = statsCollection
		}
	}
	printScopedStats(out, resp.Stats, statsAccount != "", label, resp.ScopeSourceCount)
	return nil
}

func printScopedStats(
	w io.Writer,
	s *store.Stats,
	accountScope bool,
	label string,
	sourceCount int,
) {
	if accountScope {
		_, _ = fmt.Fprintf(w, "Stats for account %q:\n", label)
	} else {
		suffix := "s"
		if sourceCount == 1 {
			suffix = ""
		}
		_, _ = fmt.Fprintf(w, "Stats for collection %q (%d account%s):\n",
			label, sourceCount, suffix)
	}
	printStats(w, s)
	_, _ = fmt.Fprintln(w, "\nNote: Size is global (not scoped).")
}

func printStats(w io.Writer, s *store.Stats) {
	_, _ = fmt.Fprintf(w, "  Messages:    %d\n", s.MessageCount)
	_, _ = fmt.Fprintf(w, "  Threads:     %d\n", s.ThreadCount)
	_, _ = fmt.Fprintf(w, "  Attachments: %d\n", s.AttachmentCount)
	_, _ = fmt.Fprintf(w, "  Labels:      %d\n", s.LabelCount)
	_, _ = fmt.Fprintf(w, "  Accounts:    %d\n", s.SourceCount)
	_, _ = fmt.Fprintf(w, "  Size:        %.2f MB\n", float64(s.DatabaseSize)/(1024*1024))
}

func init() {
	rootCmd.AddCommand(statsCmd)
	statsCmd.Flags().StringVar(&statsAccount, "account", "", "Show stats for a specific account")
	statsCmd.Flags().StringVar(&statsCollection, "collection", "",
		"Show stats for all member accounts of one collection")
	statsCmd.MarkFlagsMutuallyExclusive("account", "collection")
}
