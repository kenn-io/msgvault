package cmd

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/store"
)

var rebuildFTSCmd = &cobra.Command{
	Use:   "rebuild-fts",
	Short: "Rebuild the full-text search index from scratch",
	Long: `Drop and recreate the messages_fts virtual table, then repopulate it
from messages / message_bodies / message_recipients / participants.

Use this to recover from FTS5 shadow-table corruption that surfaces as
"malformed inverted index for FTS5 table main.messages_fts" in
'msgvault verify' output. SQLite's own 'rebuild' pragma reads from the
same corrupt shadow tables and cannot clear this state.

This command only fixes the derived search index. Core-table corruption
(e.g., "Rowid out of order" in messages / message_bodies B-trees) requires
a different recovery path — see 'msgvault verify' output.

Peak extra disk usage is roughly the size of the FTS5 shadow tables
(a few percent of the SQLite database). Stop 'msgvault serve' and any
MCP clients before running this command — it needs an exclusive write lock.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath := cfg.DatabaseDSN()
		s, err := store.Open(dbPath)
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer func() { _ = s.Close() }()

		if err := s.InitSchema(); err != nil {
			return fmt.Errorf("init schema: %w", err)
		}

		fmt.Fprintln(os.Stderr, "Rebuilding full-text search index...")
		n, err := s.RebuildFTS(func(done, total int64) {
			if total <= 0 {
				return
			}
			if done > total {
				done = total
			}
			pct := int(done * 100 / total)
			barWidth := 30
			filled := barWidth * pct / 100
			bar := strings.Repeat("=", filled) +
				strings.Repeat(" ", barWidth-filled)
			fmt.Fprintf(os.Stderr, "\r  [%s] %3d%%", bar, pct)
		})
		if err != nil {
			fmt.Fprintln(os.Stderr)
			if s.IsBusyError(err) {
				return errors.New("database is busy — stop 'msgvault serve' and any MCP " +
					"clients, then retry",
				)
			}
			return fmt.Errorf("rebuild FTS: %w", err)
		}
		fmt.Fprintf(os.Stderr,
			"\r  [%s] 100%%  %d messages indexed.\n",
			strings.Repeat("=", 30), n)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(rebuildFTSCmd)
}
