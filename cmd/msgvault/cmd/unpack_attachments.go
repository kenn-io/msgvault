package cmd

import (
	"errors"
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

The daemon must be stopped first ('msgvault serve stop'): a running
daemon holds pack files open, so this command refuses to run while one
is detected. Re-running 'msgvault pack-attachments' packs everything
again. When a remote server is configured, run this command on the archive
host or pass --local to select this machine's local archive intentionally.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runUnpackAttachmentsLocal(cmd)
	},
}

// refuseUnpackWithLiveDaemon rejects unpack while any responding daemon owns
// the archive, on every backend. The SQLite write lock (taken next by
// openWritableStoreAndInit) already guarantees exclusivity there, but
// PostgreSQL deployments skip that filesystem lock entirely, and a running
// daemon's blob store holds pack files open (which blocks their deletion on
// Windows) regardless of backend. Any responding daemon counts, compatible
// with this client or not — it holds pack readers all the same.
func refuseUnpackWithLiveDaemon(dataDir string) error {
	if findAnyDaemonRuntime(dataDir) != nil {
		return errors.New(
			"unpack-attachments: a msgvault daemon is running and holds pack files open; " +
				"stop it with `msgvault serve stop`, then retry")
	}
	return nil
}

func runUnpackAttachmentsLocal(cmd *cobra.Command) error {
	if IsRemoteMode() {
		return errors.New(
			"unpack-attachments is local-only; run it on the archive host, " +
				"or pass --local to select this machine's local archive intentionally")
	}
	if err := refuseUnpackWithLiveDaemon(cfg.Data.DataDir); err != nil {
		return err
	}
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
