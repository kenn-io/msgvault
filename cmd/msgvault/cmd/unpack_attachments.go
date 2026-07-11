package cmd

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"go.kenn.io/kit/packstore"
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

// unpackAttachmentsAfterDaemonLock is a narrow command test barrier.
// Production leaves it nil.
var unpackAttachmentsAfterDaemonLock func()

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

func runUnpackAttachmentsLocal(cmd *cobra.Command) (runErr error) {
	if IsRemoteMode() {
		return errors.New(
			"unpack-attachments is local-only; run it on the archive host, " +
				"or pass --local to select this machine's local archive intentionally")
	}
	daemonLock, err := tryAcquireDaemonOwnerLock(cfg.Data.DataDir)
	if err != nil {
		return fmt.Errorf("unpack-attachments: %w", err)
	}
	defer func() {
		if err := daemonLock.Close(); err != nil {
			runErr = errors.Join(runErr, err)
		}
	}()
	if unpackAttachmentsAfterDaemonLock != nil {
		unpackAttachmentsAfterDaemonLock()
	}
	if err := refuseUnpackWithLiveDaemon(cfg.Data.DataDir); err != nil {
		return err
	}
	s, cleanup, err := openWritableStoreAndInit()
	if err != nil {
		return err
	}
	defer cleanup()

	maintenance, err := newAttachmentMaintenance(s, cfg.AttachmentsDir(), nil)
	if err != nil {
		return err
	}
	defer func() { _ = maintenance.close() }()
	stats, err := maintenance.unpack(cmd.Context())
	if err != nil {
		return err
	}

	writeUnpackAttachmentsStats(cmd.OutOrStdout(), stats)
	return nil
}

func writeUnpackAttachmentsStats(out io.Writer, stats packstore.UnpackStats) {
	_, _ = fmt.Fprintf(out,
		"Restored %d blob(s) (%s) from %d pack(s) to loose files.\n",
		stats.BlobsRestored, formatSize(stats.BytesRestored), stats.PacksUnpacked)
	if stats.MappingsPruned > 0 {
		_, _ = fmt.Fprintf(out, "Pruned %d stale packed blob mapping(s).\n", stats.MappingsPruned)
	}
}

func init() {
	rootCmd.AddCommand(unpackAttachmentsCmd)
}
