package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/packer"
)

var unpackAttachmentsCmd = &cobra.Command{
	Use:   "unpack-attachments",
	Short: "Restore packed attachments to loose files",
	Long: `Restore every packed attachment blob to a loose file and remove the
pack files.

This is the downgrade escape hatch: older msgvault binaries cannot read
pack files, so run this before downgrading. Each blob is hash-verified
as it is written back.

The daemon must be stopped first ('msgvault serve stop') — a running
daemon holds pack files open, and the write-lock error will say so.
Re-running 'msgvault pack-attachments' packs everything again.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runUnpackAttachmentsLocal(cmd)
	},
}

func runUnpackAttachmentsLocal(cmd *cobra.Command) error {
	s, cleanup, err := openWritableStoreAndInit()
	if err != nil {
		return err
	}
	defer cleanup()

	stats, err := packer.Unpack(cmd.Context(), s, cfg.AttachmentsDir())
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(),
		"Restored %d blob(s) (%s) from %d pack(s) to loose files.\n",
		stats.BlobsRestored, formatSize(stats.BytesRestored), stats.PacksUnpacked)
	return nil
}

func init() {
	rootCmd.AddCommand(unpackAttachmentsCmd)
}
