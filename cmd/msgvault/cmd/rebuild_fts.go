package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
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
(a few percent of the SQLite database). By default this command runs through
the configured remote or local daemon over HTTP so the daemon keeps ownership
of SQLite while progress streams back to the CLI. Use --local to use the local
daemon even when a remote is configured.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runHTTPRebuildFTS(cmd)
	},
}

func runHTTPRebuildFTS(cmd *cobra.Command) error {
	st, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	errOut := cmd.ErrOrStderr()
	_, _ = fmt.Fprintln(errOut, "Rebuilding full-text search index...")
	n, err := st.RebuildCLIFTS(cmd.Context(), func(done, total int64) {
		writeCLIProgressPercent(errOut, done, total)
	})
	if err != nil {
		_, _ = fmt.Fprintln(errOut)
		return fmt.Errorf("rebuild FTS: %w", err)
	}
	writeCLIIndexedProgressComplete(errOut, n)
	return nil
}

func init() {
	rootCmd.AddCommand(rebuildFTSCmd)
}
