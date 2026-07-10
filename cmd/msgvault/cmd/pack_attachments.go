package cmd

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/packer"
)

var packAttachmentsCmd = &cobra.Command{
	Use:   "pack-attachments",
	Short: "Pack loose attachment files into sealed pack files",
	Long: `Pack loose content-addressed attachment files into sealed pack files
under the attachments directory.

Packing means far fewer files on disk and faster backups. Reads work
transparently from packs; new attachments arrive as loose files and are
picked up by the next run, so this command is safe to re-run any time.

When the background daemon is running, the operation runs under the
daemon and is serialized against syncs and backups.

To go back to loose files (e.g., before downgrading msgvault), run
'msgvault unpack-attachments'.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if isDaemonCLISubprocess() {
			return runPackAttachmentsLocal(cmd)
		}
		return runDaemonCLICommandHTTPFromCobra(cmd, args)
	},
}

func runPackAttachmentsLocal(cmd *cobra.Command) error {
	s, cleanup, err := openWritableStoreAndInit()
	if err != nil {
		return err
	}
	defer cleanup()

	stats, err := packer.Run(cmd.Context(), s, cfg.AttachmentsDir(), packer.Options{})
	if err != nil {
		return err
	}

	writePackAttachmentsStats(cmd.OutOrStdout(), stats)
	return nil
}

func writePackAttachmentsStats(out io.Writer, stats packer.Stats) {
	_, _ = fmt.Fprintf(out, "Packed %d blob(s) (%s) into %d pack(s).\n",
		stats.BlobsPacked, formatSize(stats.BytesPacked), stats.PacksSealed)
	if stats.PacksAdopted > 0 {
		_, _ = fmt.Fprintf(out, "Adopted %d orphan pack(s).\n", stats.PacksAdopted)
	}
	if stats.PacksRemoved > 0 {
		_, _ = fmt.Fprintf(out, "Removed %d redundant orphan pack(s).\n", stats.PacksRemoved)
	}
	if stats.BlobsMissing > 0 {
		_, _ = fmt.Fprintf(out, "Skipped %d blob(s) with missing files.\n", stats.BlobsMissing)
	}
	if stats.BlobsCorrupt > 0 {
		_, _ = fmt.Fprintf(out, "Skipped %d corrupt blob file(s).\n", stats.BlobsCorrupt)
	}
	if stats.LooseSwept > 0 {
		_, _ = fmt.Fprintf(out, "Swept %d already-packed loose file(s).\n", stats.LooseSwept)
	}
	if stats.MappingsPruned > 0 {
		_, _ = fmt.Fprintf(out, "Pruned %d stale packed blob mapping(s).\n", stats.MappingsPruned)
	}
	if stats.LooseOrphansRemoved > 0 {
		_, _ = fmt.Fprintf(out, "Removed %d unreferenced loose file(s).\n", stats.LooseOrphansRemoved)
	}
	if stats.PacksQuarantined > 0 {
		_, _ = fmt.Fprintf(out, "Quarantined %d damaged orphan pack(s).\n", stats.PacksQuarantined)
	}
	if stats.PacksUnreadable > 0 {
		_, _ = fmt.Fprintf(out, "Found %d unreadable orphan pack(s).\n", stats.PacksUnreadable)
	}
	if stats.BlobsDeferredOversized > 0 {
		_, _ = fmt.Fprintf(out,
			"Left %d large canonical blob(s) loose because they exceed the 64 MiB maintenance limit.\n",
			stats.BlobsDeferredOversized)
	}
	if stats.PacksDeferredOversized > 0 {
		_, _ = fmt.Fprintf(out,
			"Deferred %d oversized orphan pack(s); they remain untouched for a future maintenance run.\n",
			stats.PacksDeferredOversized)
	}
}

func init() {
	rootCmd.AddCommand(packAttachmentsCmd)
}
