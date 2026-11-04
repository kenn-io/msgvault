package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/hex"
	"go.kenn.io/msgvault/internal/store"
)

var (
	syncHexLimit int
	syncHexAfter string
	syncHexFull  bool
	syncHexWatch bool
)

var (
	rebuildHexCacheAfterWrite         = rebuildCacheAfterManualSync
	rebuildHexCacheAfterScheduledSync = rebuildCacheAfterScheduledSync
)

const hexConfigHint = `Add to your config.toml:

  [[hex]]
  identifier = "recorder"             # stable label for this Hex installation
  account_email = "you@example.com"   # you, the person who records the meetings
  enabled = true
  # meetings_dir = "/path/to/hex/meetings"  # uses Hex's platform default when omitted
  # schedule = "*/30 * * * *"         # daemon schedule, or remote client --watch`

func hexSources(cfg *config.Config) meetingSources[config.HexSource] {
	sources := meetingSources[config.HexSource]{
		table: "hex", hint: hexConfigHint,
	}
	if cfg != nil {
		sources.configured, sources.lookup = cfg.Hex, cfg.GetHexSource
		sources.identifier = func(s config.HexSource) string { return s.Identifier }
	}
	return sources
}

// probeHexMeetings proves the configured meeting folder opens read-only.
func probeHexMeetings(ctx context.Context, path string) error {
	reader, err := hex.Open(ctx, path)
	if err != nil {
		return fmt.Errorf("%w\n\nCheck meetings_dir in the [[hex]] entry. msgvault reads the folder on the "+
			"recorder's host in remote mode, or daemon's host in same-host mode; check that the reader can access the folder", err)
	}
	return reader.Close()
}

var addHexCmd = &cobra.Command{
	Use:   "add-hex [identifier]",
	Short: "Register a local Hex meeting folder",
	Long: `Register a configured Hex meeting folder as a msgvault meeting source.

Reads meetings_dir from the matching [[hex]] entry in config.toml, or Hex's
platform default when omitted, and checks that the folder opens read-only. In remote mode the client reads local files
and securely registers the source with the configured daemon. In same-host
mode the daemon reads the files.

Examples:
  msgvault add-hex
  msgvault add-hex mac`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		state := invocationFromCommand(cmd)
		if state == nil || state.cfg == nil {
			return errors.New("configuration is unavailable")
		}
		cfg := state.cfg
		if !isDaemonCLISubprocess() {
			if isRemoteModeFor(state) {
				return runHexClientAdd(cmd, args)
			}
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		source, err := hexSources(cfg).one(args)
		if err != nil {
			return err
		}
		accountEmail, err := source.EffectiveAccountEmail()
		if err != nil {
			return err
		}
		if err := probeHexMeetings(cmd.Context(), source.EffectiveMeetingsDir()); err != nil {
			return err
		}
		st, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
		if err != nil {
			return err
		}
		defer cleanup()
		if _, err := registerMeetingSource(cmd.OutOrStdout(), st, sourceTypeHex,
			source.Identifier, accountEmail); err != nil {
			return err
		}
		if err := runPostSourceCreateMigrationsForInvocation(st, state); err != nil {
			return fmt.Errorf("post-source-create migrations: %w", err)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "\nHex source %s registered.\n", source.Identifier)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Run: msgvault sync-hex %s\n", source.Identifier)
		return nil
	},
}

var syncHexCmd = &cobra.Command{
	Use:   "sync-hex [identifier]",
	Short: "Sync meetings from a local Hex folder",
	Long: `Archive stopped Hex meetings: stored notes, transcript, calendar evidence and
explicit participant emails. With no identifier, every configured [[hex]] source is
synced.

Every run reads the whole folder read-only and updates meetings that
changed in place. Meetings still recording or processing wait for a later run.
Meetings deleted in Hex stay archived.

Examples:
  msgvault sync-hex
  msgvault sync-hex mac --limit 5
  msgvault sync-hex --after 2026-01-01   # UTC date
  msgvault sync-hex --full`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		state := invocationFromCommand(cmd)
		if state == nil || state.cfg == nil {
			return errors.New("configuration is unavailable")
		}
		cfg := state.cfg
		if !isDaemonCLISubprocess() {
			if syncHexWatch && !isRemoteModeFor(state) {
				return errors.New("--watch requires a configured remote archive; same-host sync uses the daemon schedule")
			}
			if isRemoteModeFor(state) {
				return runHexClientSync(cmd, args)
			}
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		sources, err := hexSources(cfg).selected(args)
		if err != nil {
			return err
		}
		var after time.Time
		if syncHexAfter != "" {
			parsed, err := time.Parse(time.DateOnly, syncHexAfter)
			if err != nil {
				return usageErr(cmd, fmt.Errorf("invalid --after %q (expected YYYY-MM-DD): %w", syncHexAfter, err))
			}
			after = parsed.UTC()
		}
		for _, source := range sources {
			if _, err := source.EffectiveAccountEmail(); err != nil {
				return err
			}
		}

		st, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
		if err != nil {
			return err
		}
		defer cleanup()
		dbPath := cfg.DatabaseDSN()

		ctx, stop := withInterruptCancel(cmd, "\nInterrupted. Finishing current meeting...")
		defer stop()

		var pendingWrites int64
		for _, source := range sources {
			accountEmail, _ := source.EffectiveAccountEmail()
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Syncing Hex for %s\n\n", source.Identifier)
			options := hexImportOptions(source)
			options.AttachmentsDir = cfg.AttachmentsDir()
			options.AccountEmail = accountEmail
			options.Full, options.Limit, options.StartedAfter = syncHexFull, syncHexLimit, after
			options.Progress = func(line string) { _, _ = fmt.Fprintln(cmd.OutOrStdout(), "  "+line) }
			summary, importErr := hex.NewImporter(st).Import(ctx, options)
			if summary != nil {
				pendingWrites += summary.MeetingsAdded + summary.MeetingsUpdated
			}
			run := meetingSyncRun{
				provider: "hex", identifier: source.Identifier,
				writes: pendingWrites, err: importErr,
			}
			refresh := func() error { return rebuildHexCacheAfterWrite(dbPath, state) }
			if err := run.finish(refresh); err != nil {
				return err
			}
			writeHexSummary(cmd.OutOrStdout(), summary)
		}
		if pendingWrites > 0 {
			return rebuildHexCacheAfterWrite(dbPath, state)
		}
		return nil
	},
}

func writeHexSummary(out io.Writer, summary *hex.ImportSummary) {
	_, _ = fmt.Fprintln(out, "\nHex sync complete!")
	_, _ = fmt.Fprintf(out, "  Duration:           %s\n", summary.Duration.Round(time.Second))
	_, _ = fmt.Fprintf(out, "  Meetings processed: %d\n", summary.MeetingsProcessed)
	_, _ = fmt.Fprintf(out, "  Meetings added:     %d\n", summary.MeetingsAdded)
	_, _ = fmt.Fprintf(out, "  Meetings updated:   %d\n", summary.MeetingsUpdated)
	if summary.RecordingsStored > 0 || summary.RecordingsSkipped > 0 {
		_, _ = fmt.Fprintf(out, "  Recordings stored:  %d (deferred: %d)\n", summary.RecordingsStored, summary.RecordingsSkipped)
	}
	if summary.SkippedInProgress > 0 {
		_, _ = fmt.Fprintf(out, "  Still in progress:  %d (archived by a later sync)\n", summary.SkippedInProgress)
	}
	if summary.Warnings > 0 {
		_, _ = fmt.Fprintf(out, "  Source warnings: %d\n", summary.Warnings)
	}

}

// hexImportOptions carries a configured source's paths and recording
// settings into an import.
func hexImportOptions(source config.HexSource) hex.ImportOptions {
	return hex.ImportOptions{
		Recordings: source.Recordings, RecordingMaxBytes: source.EffectiveRecordingMaxBytes(),
		Identifier: source.Identifier, AccountEmail: source.AccountEmail,
		MeetingsDir: source.EffectiveMeetingsDir(),
	}
}

// runConfiguredHexSync is the daemon-scheduler entry point for one
// [[hex]] source.
func runConfiguredHexSync(ctx context.Context, st *store.Store, source config.HexSource, attachmentsDir string) error {
	notRegistered := fmt.Errorf(
		"hex source %q is not registered; run msgvault add-hex %s first",
		source.Identifier, source.Identifier)
	registered, err := requireRegisteredMeetingSource(st, hex.SourceType, source.Identifier, notRegistered)
	if err != nil {
		return recordSyncAttempt(ctx, hex.SourceType, source.Identifier, err)
	}
	if registered.MergedIntoSourceID != 0 {
		return nil
	}
	accountEmail, err := source.EffectiveAccountEmail()
	if err != nil {
		return recordSyncAttempt(ctx, hex.SourceType, source.Identifier, err)
	}
	options := hexImportOptions(source)
	options.AttachmentsDir = attachmentsDir
	options.AccountEmail = accountEmail
	summary, importErr := hex.NewImporter(st).Import(ctx, options)
	var writes int64
	if summary != nil {
		writes = summary.MeetingsAdded + summary.MeetingsUpdated
	}
	run := meetingSyncRun{
		provider: "hex", identifier: source.Identifier, writes: writes, err: importErr,
	}
	// Check the cache even when this scan wrote nothing: a hook or manual
	// import may have been throttled by min_rebuild_interval, and the
	// background refresher schedules the retry that import did not.
	_ = recordSyncAttempt(ctx, hex.SourceType, source.Identifier, importErr)
	return run.finishScheduled(ctx, "hex:"+source.Identifier,
		rebuildHexCacheAfterScheduledSync)
}

func init() {
	syncHexCmd.Flags().IntVar(&syncHexLimit, "limit", 0, "max meetings processed per run (0 = no limit)")
	syncHexCmd.Flags().StringVar(&syncHexAfter, "after", "", "only meetings that start on or after this UTC date (YYYY-MM-DD)")
	syncHexCmd.Flags().BoolVar(&syncHexFull, "full", false, "rewrite every archived meeting, even unchanged ones (refreshes attribution)")
	syncHexCmd.Flags().BoolVar(&syncHexWatch, "watch", false, "keep rescanning enabled remote recorder sources using their schedules")
	addManualSyncCacheFlags(syncHexCmd)
	rootCmd.AddCommand(addHexCmd)
	rootCmd.AddCommand(syncHexCmd)
}
