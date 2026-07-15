package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/beeper"
	"go.kenn.io/msgvault/internal/store"
)

var (
	syncBeeperLimit    int
	syncBeeperFull     bool
	syncBeeperAccounts []string
	syncBeeperNoMedia  bool
)

func newSyncBeeperCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sync-beeper",
		Short: "Sync chats from Beeper Desktop (all bridged networks)",
		Long: `Sync chats from Beeper Desktop for every registered Beeper account.

The first run backfills each chat's full locally-available history; later
runs are incremental, fetching only new activity. Backfills are resumable:
re-run after an interruption and the sync continues where it stopped.

Requires Beeper Desktop to be running on this machine (run 'add-beeper'
first). Use --full to ignore stored cursors and re-fetch every message;
re-fetched messages are upserted in place, so this repairs existing rows
without creating duplicates.

Examples:
  msgvault sync-beeper
  msgvault sync-beeper --account signal --account telegram
  msgvault sync-beeper --limit 500
  msgvault sync-beeper --full`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}

			imp, accountIDs, dbPath, cleanup, err := openBeeperImporter(syncBeeperAccounts)
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, stop := withInterruptCancel(cmd, "\nInterrupted. Saving checkpoint...")
			defer stop()

			var syncErrors []string
			for _, accountID := range accountIDs {
				if ctx.Err() != nil {
					break
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Syncing Beeper account %s\n", accountID)
				opts := beeperImportOptions(accountID)
				opts.Limit = syncBeeperLimit
				opts.Full = syncBeeperFull
				opts.NoMedia = opts.NoMedia || syncBeeperNoMedia
				opts.Progress = func(s string) { _, _ = fmt.Fprintln(cmd.OutOrStdout(), "  "+s) }
				sum, err := imp.Import(ctx, opts)
				if ctx.Err() != nil {
					break
				}
				// One broken network must not block the others; collect and
				// keep syncing (same convention as the multi-account sync
				// commands and the beeper scheduler path).
				if err != nil {
					syncErrors = append(syncErrors, fmt.Sprintf("%s: %v", accountID, err))
					continue
				}
				printBeeperSummary(cmd, accountID, sum)
			}

			// Successful accounts' messages must reach the analytics cache
			// regardless of interruptions or per-account failures.
			cacheErr := rebuildCacheAfterWrite(dbPath)
			if ctx.Err() != nil {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "\nInterrupted — re-run sync-beeper to resume.")
				return cacheErr
			}
			if len(syncErrors) > 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "\nErrors:")
				for _, e := range syncErrors {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", e)
				}
				return errors.Join(
					fmt.Errorf("%d account(s) failed to sync: %s", len(syncErrors), strings.Join(syncErrors, "; ")),
					cacheErr,
				)
			}
			return cacheErr
		},
	}
	cmd.Flags().IntVar(&syncBeeperLimit, "limit", 0, "max messages per chat this run (0 = no limit; limited backfills resume on the next run)")
	cmd.Flags().BoolVar(&syncBeeperFull, "full", false, "ignore stored cursors and re-fetch every message (repairs/backfills existing rows in place)")
	cmd.Flags().StringArrayVar(&syncBeeperAccounts, "account", nil, "Beeper accountID to sync (repeatable; default: all registered accounts)")
	cmd.Flags().BoolVar(&syncBeeperNoMedia, "no-media", false, "skip attachment downloads for this run")
	return cmd
}

func printBeeperSummary(cmd *cobra.Command, accountID string, sum *beeper.ImportSummary) {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  %s done in %s: %d chats, %d messages (%d added)",
		accountID, sum.Duration.Round(time.Second), sum.ChatsProcessed, sum.MessagesProcessed, sum.MessagesAdded)
	if sum.ReactionsRefreshed > 0 {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), ", %d reactions refreshed", sum.ReactionsRefreshed)
	}
	if sum.AttachmentsDownloaded > 0 {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), ", %d attachments", sum.AttachmentsDownloaded)
	}
	if sum.AttachmentsPending > 0 {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), ", %d media pending (see backfill-beeper-media)", sum.AttachmentsPending)
	}
	if sum.Errors > 0 {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), ", %d errors", sum.Errors)
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout())
}

// resolveBeeperSyncAccounts returns the Beeper accountIDs to sync: the
// explicit flag values, or every registered beeper source that passes the
// config include/exclude filters.
func resolveBeeperSyncAccounts(s *store.Store, flagAccounts []string) ([]string, error) {
	sources, err := s.ListSources(sourceTypeBeeper)
	if err != nil {
		return nil, fmt.Errorf("list beeper sources: %w", err)
	}
	if len(flagAccounts) > 0 {
		registered := make(map[string]struct{}, len(sources))
		for _, src := range sources {
			registered[src.Identifier] = struct{}{}
		}
		seen := make(map[string]struct{}, len(flagAccounts))
		out := make([]string, 0, len(flagAccounts))
		for _, accountID := range flagAccounts {
			if _, ok := registered[accountID]; !ok {
				return nil, fmt.Errorf("beeper account %q is not registered (run 'add-beeper' first)", accountID)
			}
			if _, duplicate := seen[accountID]; duplicate {
				continue
			}
			seen[accountID] = struct{}{}
			out = append(out, accountID)
		}
		return out, nil
	}
	var out []string
	for _, src := range sources {
		if cfg.Beeper.AccountIncluded(src.Identifier) {
			out = append(out, src.Identifier)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no Beeper accounts registered (run 'add-beeper' first)")
	}
	return out, nil
}

// beeperImportOptions builds the config-derived import options shared by the
// CLI and scheduler paths (flag overlays are applied by the CLI caller).
func beeperImportOptions(accountID string) beeper.ImportOptions {
	return beeper.ImportOptions{
		AccountID:      accountID,
		AttachmentsDir: cfg.AttachmentsDir(),
		NoMedia:        !cfg.Beeper.MediaEnabled(),
		MaxMediaBytes:  cfg.Beeper.MaxMediaBytes(),
	}
}

// openBeeperImporter performs the shared beeper-command prologue: open the
// store, load the token, resolve the target accounts, and build the importer.
// The returned cleanup closes the store.
func openBeeperImporter(flagAccounts []string) (imp *beeper.Importer, accountIDs []string, dbPath string, cleanup func(), err error) {
	s, cleanup, err := openWritableStoreAndInitForIngest()
	if err != nil {
		return nil, nil, "", nil, err
	}
	token, err := beeper.LoadToken(cfg.TokensDir())
	if err != nil {
		cleanup()
		return nil, nil, "", nil, err
	}
	accountIDs, err = resolveBeeperSyncAccounts(s, flagAccounts)
	if err != nil {
		cleanup()
		return nil, nil, "", nil, err
	}
	return beeper.NewImporter(s, beeperClient(token)), accountIDs, cfg.DatabaseDSN(), cleanup, nil
}

// withInterruptCancel derives a context canceled on SIGINT/SIGTERM, printing
// note to stderr on the first signal. The returned stop must be deferred.
func withInterruptCancel(cmd *cobra.Command, note string) (context.Context, func()) {
	ctx, cancel := context.WithCancel(cmd.Context())
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case <-sigChan:
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), note)
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, func() { signal.Stop(sigChan); cancel() }
}

// runConfiguredBeeperSync is the daemon scheduler entrypoint: an incremental
// sync of every registered Beeper account. Per-account failures are collected
// so one broken account does not starve the others, and the analytics cache is
// rebuilt after any attempt so partial writes become visible too.
func runConfiguredBeeperSync(ctx context.Context, s *store.Store) error {
	token, err := beeper.LoadToken(cfg.TokensDir())
	if err != nil {
		return err
	}
	accountIDs, err := resolveBeeperSyncAccounts(s, nil)
	if err != nil {
		return err
	}
	imp := beeper.NewImporter(s, beeperClient(token))
	return runScheduledBeeperAttempts(ctx, accountIDs,
		func(accountID string) error {
			_, err := imp.Import(ctx, beeperImportOptions(accountID))
			return err
		},
		func() error { return rebuildCacheAfterScheduledSync(context.WithoutCancel(ctx), "beeper") },
	)
}

// runScheduledBeeperAttempts keeps per-account failures isolated while
// rebuilding analytics after any import attempt, since even a failed or
// canceled attempt may have committed messages from healthy chats.
func runScheduledBeeperAttempts(ctx context.Context, accountIDs []string, attempt func(string) error, rebuild func() error) error {
	var errs []error
	attempted := 0
	for _, accountID := range accountIDs {
		if ctx.Err() != nil {
			break
		}
		attempted++
		if err := attempt(accountID); err != nil {
			errs = append(errs, fmt.Errorf("beeper %s: %w", accountID, err))
		}
	}
	if attempted > 0 {
		if err := rebuild(); err != nil {
			errs = append(errs, err)
		}
	}
	if ctx.Err() != nil {
		errs = append(errs, ctx.Err())
	}
	return errors.Join(errs...)
}

func init() {
	rootCmd.AddCommand(newSyncBeeperCmd())
}
