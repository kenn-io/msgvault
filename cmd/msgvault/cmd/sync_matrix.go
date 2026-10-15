package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	matrixsource "go.kenn.io/msgvault/internal/matrix"
	"go.kenn.io/msgvault/internal/store"
)

var (
	syncMatrixFull    bool
	syncMatrixAccount string
)

func newSyncMatrixCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sync-matrix",
		Short: "Sync joined rooms from Matrix",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			state := invocationFromCommand(cmd)
			if state == nil || state.cfg == nil {
				return errors.New("configuration is unavailable")
			}
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}
			s, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, stop := withInterruptCancel(cmd, "\nInterrupted. Saving Matrix checkpoint...")
			defer stop()
			err = runMatrixSync(ctx, s, state.cfg, syncMatrixAccount, syncMatrixFull,
				func(line string) { _, _ = fmt.Fprintln(cmd.OutOrStdout(), "  "+line) }, cmd.OutOrStdout())
			cacheErr := rebuildCacheAfterManualSync(state.cfg.DatabaseDSN(), state)
			return errors.Join(err, cacheErr)
		},
	}
	cmd.Flags().BoolVar(&syncMatrixFull, "full", false, "ignore stored cursors and re-fetch complete room history")
	cmd.Flags().StringVar(&syncMatrixAccount, "account", "", "sync only this Matrix user ID")
	return addManualSyncCacheFlags(cmd)
}

func runMatrixSync(ctx context.Context, s *store.Store, cfg *config.Config, account string, full bool, progress func(string), out io.Writer) error {
	sources, err := s.ListSources(sourceTypeMatrix)
	if err != nil {
		return fmt.Errorf("list Matrix sources: %w", err)
	}
	if len(sources) == 0 {
		return errors.New("no Matrix accounts registered (run 'add-matrix' first)")
	}
	var failures []string
	backupHintShown := false
	for _, source := range sources {
		if account != "" && source.Identifier != account {
			continue
		}
		var summary *matrixsource.ImportSummary
		var needsBackupKey bool
		var homeserver string
		syncErr := matrixsource.WithCredentialLifecycleLock(cfg.TokensDir(), func() error {
			current, lookupErr := s.GetSourceByID(source.ID)
			if lookupErr != nil {
				return fmt.Errorf("reload Matrix source before sync: %w", lookupErr)
			}
			if current.SourceType != sourceTypeMatrix || current.Identifier != source.Identifier {
				return fmt.Errorf("matrix source %d changed before sync", source.ID)
			}
			creds, loadErr := matrixsource.LoadCredentials(cfg.TokensDir(), current.Identifier)
			if loadErr != nil {
				return loadErr
			}
			homeserver = creds.Homeserver
			runtime, openErr := matrixsource.Open(creds)
			if openErr != nil {
				return openErr
			}
			imp := matrixsource.NewImporter(s, runtime)
			var preferences matrixSyncPreferences
			if current.SyncConfig.Valid {
				if err := json.Unmarshal([]byte(current.SyncConfig.String), &preferences); err != nil {
					slog.Warn("Matrix key backup preference unreadable; treating it as unset", "error", err)
					preferences.SkipKeyBackup = false
				}
			}
			skipKeyBackup := preferences.SkipKeyBackup && creds.BackupKey == ""
			var importErr error
			summary, importErr = imp.Import(ctx, matrixsource.ImportOptions{
				UserID: current.Identifier, Full: full, SkipKeyBackup: skipKeyBackup,
				Rooms: cfg.Matrix.Rooms, ExcludeRooms: cfg.Matrix.ExcludeRooms,
				Progress: progress,
			})
			if importErr == nil && creds.BackupKey == "" {
				needsBackupKey = summary.PendingEncrypted > 0 && !skipKeyBackup
			}
			return importErr
		})
		if syncErr != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", source.Identifier, syncErr))
			continue
		}
		_, _ = fmt.Fprintf(out, "Matrix %s: %d rooms, %d messages, %d encrypted placeholders, %d message rows recovered, %d unsupported events skipped\n",
			source.Identifier, summary.RoomsProcessed, summary.MessagesProcessed, summary.PendingEncrypted, summary.Recovered, summary.EventsSkipped)
		if needsBackupKey && !backupHintShown {
			backupHintShown = true
			_, _ = fmt.Fprintf(out, "To recover encrypted history, run msgvault add-matrix --homeserver %q --user-id %q --recovery-file <path>. This renews the login and logs out the old Matrix device.\n", homeserver, source.Identifier)
		}
		if summary.BackupFailure != nil {
			failure := fmt.Sprintf("%s: key backup unavailable, %d encrypted events pending: %v", source.Identifier, summary.PendingEncrypted, summary.BackupFailure)
			failures = append(failures, failure)
		}
	}
	if account != "" {
		found := false
		for _, source := range sources {
			found = found || source.Identifier == account
		}
		if !found {
			return fmt.Errorf("matrix account %q is not registered", account)
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("matrix sync failed: %s", strings.Join(failures, "; "))
	}
	return nil
}

func runConfiguredMatrixSync(ctx context.Context, s *store.Store) error {
	state := invocationFromContext(ctx)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	syncErr := runMatrixSync(ctx, s, state.cfg, "", false, nil, io.Discard)
	// A failed attempt can still have committed room pages, so refresh after
	// every scheduled run and do not let cancellation hide durable changes.
	cacheErr := rebuildMatrixCacheAfterScheduledSync(context.WithoutCancel(ctx), "matrix")
	return errors.Join(syncErr, cacheErr)
}

var rebuildMatrixCacheAfterScheduledSync = rebuildCacheAfterScheduledSync

func init() { rootCmd.AddCommand(newSyncMatrixCmd()) }
