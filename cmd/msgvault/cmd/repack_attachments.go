package cmd

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"go.kenn.io/msgvault/internal/repacker"
)

var repackAttachmentsCmd = &cobra.Command{
	Use:   "repack-attachments",
	Short: "Reclaim dead bytes from sparse attachment packs",
	Long: `Reclaim dead bytes from sparse immutable attachment pack files.

This command always runs in the msgvault daemon so it can atomically swap live
blob mappings and retire the daemon's shared pack readers before deleting old
files. It is safe to retry after interruption or a Windows file-sharing error.`,
	Args: cobra.NoArgs,
	RunE: runDaemonCLICommandHTTPFromCobra,
}

func writeRepackAttachmentsStats(out io.Writer, stats repacker.Stats) {
	_, _ = fmt.Fprintf(out,
		"Repacked %d blob(s) (%s) from %d pack(s) into %d pack(s); removed %d old pack(s).\n",
		stats.BlobsRepacked, formatSize(stats.BytesRepacked), stats.PacksRewritten,
		stats.PacksSealed, stats.PacksRemoved)
	if stats.MappingsPruned > 0 {
		_, _ = fmt.Fprintf(out, "Pruned %d stale packed blob mapping(s).\n", stats.MappingsPruned)
	}
	if stats.PacksDeferredOversized > 0 {
		_, _ = fmt.Fprintf(out,
			"Deferred %d oversized pack(s); they remain authoritative for a future maintenance run.\n",
			stats.PacksDeferredOversized)
	}
	if stats.BudgetExhausted {
		_, _ = fmt.Fprintln(out, "Repack byte budget reached; another run will continue with remaining sparse packs.")
	}
}

func init() {
	rootCmd.AddCommand(repackAttachmentsCmd)
}
