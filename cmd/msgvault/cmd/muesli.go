package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/muesli"
	"go.kenn.io/msgvault/internal/store"
)

var (
	syncMuesliLimit     int
	syncMuesliAfter     string
	syncMuesliFull      bool
	syncMuesliWatch     bool
	syncMuesliMeetingID int64
)

var (
	rebuildMuesliCacheAfterWrite         = rebuildCacheAfterManualSync
	rebuildMuesliCacheAfterScheduledSync = rebuildCacheAfterScheduledSync
)

const muesliConfigHint = `Add to your config.toml:

  [[muesli]]
  identifier = "mac"                  # stable label for this Mac's Muesli database
  account_email = "you@example.com"   # you, the person who records the meetings
  enabled = true
  # db_path = "~/Library/Application Support/Muesli/muesli.db"  # default shown
  # schedule = "*/30 * * * *"         # daemon schedule, or remote client --watch
  # phone_country_code = "1"          # convert national-format Contacts phones
  # contacts = false                  # skip Apple Contacts attendee lookup`

func muesliSources(cfg *config.Config) meetingSources[config.MuesliSource] {
	sources := meetingSources[config.MuesliSource]{
		table: "muesli", hint: muesliConfigHint,
	}
	if cfg != nil {
		sources.configured, sources.lookup = cfg.Muesli, cfg.GetMuesliSource
		sources.identifier = func(s config.MuesliSource) string { return s.Identifier }
	}
	return sources
}

// probeMuesliDatabase proves the configured database opens read-only and
// has Muesli's meetings table.
func probeMuesliDatabase(ctx context.Context, path string) error {
	reader, err := muesli.Open(ctx, path)
	if err != nil {
		return fmt.Errorf("%w\n\nCheck db_path in the [[muesli]] entry. msgvault reads the database on the "+
			"recorder's host in remote mode, or daemon's host in same-host mode; grant the reader Full Disk Access", err)
	}
	return reader.Close()
}

var addMuesliCmd = &cobra.Command{
	Use:   "add-muesli [identifier]",
	Short: "Register a local Muesli meeting database",
	Long: `Register a configured Muesli database as a msgvault meeting source.

Reads db_path from the matching [[muesli]] entry in config.toml (default:
~/Library/Application Support/Muesli/muesli.db) and checks that it opens
read-only as a Muesli database. In remote mode the client reads local files
and securely registers the source with the configured daemon. In same-host
mode the daemon reads the files.

Examples:
  msgvault add-muesli
  msgvault add-muesli mac`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		state := invocationFromCommand(cmd)
		if state == nil || state.cfg == nil {
			return errors.New("configuration is unavailable")
		}
		cfg := state.cfg
		if !isDaemonCLISubprocess() {
			if isRemoteModeFor(state) {
				return runMuesliClientAdd(cmd, args)
			}
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		source, err := muesliSources(cfg).one(args)
		if err != nil {
			return err
		}
		accountEmail, err := source.EffectiveAccountEmail()
		if err != nil {
			return err
		}
		if err := probeMuesliDatabase(cmd.Context(), source.EffectiveDBPath()); err != nil {
			return err
		}
		if source.ContactsEnabled() {
			contacts, err := muesli.OpenContacts(cmd.Context(), source.EffectiveContactsPath())
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Apple Contacts: %s\n", contacts.State())
			if contacts.State() != muesli.ContactsComplete {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(),
					"Grant the process reading Muesli Full Disk Access so attendees picked from Contacts link to people.")
			}
		}
		st, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
		if err != nil {
			return err
		}
		defer cleanup()
		if _, err := registerMeetingSource(cmd.OutOrStdout(), st, sourceTypeMuesli,
			source.Identifier, accountEmail); err != nil {
			return err
		}
		if err := runPostSourceCreateMigrationsForInvocation(st, state); err != nil {
			return fmt.Errorf("post-source-create migrations: %w", err)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "\nMuesli source %s registered.\n", source.Identifier)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Run: msgvault sync-muesli %s\n", source.Identifier)
		return nil
	},
}

var syncMuesliCmd = &cobra.Command{
	Use:   "sync-muesli [identifier]",
	Short: "Sync meetings from a local Muesli database",
	Long: `Archive completed Muesli meetings: AI notes, typed notes, transcript, and
participant emails. With no identifier, every configured [[muesli]] source is
synced.

Every run reads the whole database read-only and updates meetings that
changed in place. Meetings still recording or processing wait for a later run.
Meetings deleted in Muesli stay archived.

Examples:
  msgvault sync-muesli
  msgvault sync-muesli mac --limit 5
  msgvault sync-muesli --after 2026-01-01   # UTC date
  msgvault sync-muesli --full`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		state := invocationFromCommand(cmd)
		if state == nil || state.cfg == nil {
			return errors.New("configuration is unavailable")
		}
		cfg := state.cfg
		if syncMuesliMeetingID < 0 {
			return errors.New("--meeting-id must be positive")
		}
		if !isDaemonCLISubprocess() {
			if syncMuesliWatch && !isRemoteModeFor(state) {
				return errors.New("--watch requires a configured remote archive; same-host sync uses the daemon schedule")
			}
			if isRemoteModeFor(state) {
				return runMuesliClientSync(cmd, args, syncMuesliMeetingID)
			}
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		return syncRecorderSources(cmd, args, muesliSources(cfg), "Muesli", "muesli", syncMuesliAfter, func(s config.MuesliSource) string { return s.Identifier },
			func(ctx context.Context, st *store.Store, source config.MuesliSource, after time.Time) (*muesli.ImportSummary, error) {
				options := muesliImportOptions(source)
				options.AttachmentsDir = cfg.AttachmentsDir()
				options.AccountEmail, _ = source.EffectiveAccountEmail()
				options.Full, options.Limit, options.StartedAfter = syncMuesliFull, syncMuesliLimit, after
				options.Progress = func(line string) { _, _ = fmt.Fprintln(cmd.OutOrStdout(), "  "+line) }
				options.MeetingID = syncMuesliMeetingID
				return muesli.NewImporter(st).Import(ctx, options)
			}, func(s *muesli.ImportSummary) int64 { return s.MeetingsAdded + s.MeetingsUpdated }, writeMuesliSummary, rebuildMuesliCacheAfterWrite)
	},
}

func writeMuesliSummary(out io.Writer, summary *muesli.ImportSummary) {
	writeRecorderSummary(out, "Muesli", summary.Duration, summary.MeetingsProcessed, summary.MeetingsAdded, summary.MeetingsUpdated, summary.RecordingsStored, summary.RecordingsSkipped, summary.SkippedInProgress)
	if summary.SkippedDeleted > 0 {
		_, _ = fmt.Fprintf(out, "  Deleted in Muesli:  %d (kept archived)\n", summary.SkippedDeleted)
	}
	if summary.SkippedEmpty > 0 {
		_, _ = fmt.Fprintf(out, "  Empty:              %d (no notes or transcript)\n", summary.SkippedEmpty)
	}
	if summary.ContactsState != "" {
		_, _ = fmt.Fprintf(out, "  Contacts:           %s\n", summary.ContactsState)
	}
	if summary.ContactsState == muesli.ContactsUnavailable || summary.ContactsState == muesli.ContactsPartial {
		_, _ = fmt.Fprintln(out, "  Grant the process reading Muesli Full Disk Access so attendees picked from Contacts link to people.")
	}
}

// muesliImportOptions carries a configured source's paths and Contacts
// settings into an import.
func muesliImportOptions(source config.MuesliSource) muesli.ImportOptions {
	return muesli.ImportOptions{
		Recordings: source.Recordings, RecordingMaxBytes: source.EffectiveRecordingMaxBytes(),
		Identifier: source.Identifier, AccountEmail: source.AccountEmail,
		DBPath:          source.EffectiveDBPath(),
		ContactsEnabled: source.ContactsEnabled(), ContactsPath: source.EffectiveContactsPath(),
		PhoneCountryCode: source.PhoneCountryCode,
	}
}

// runConfiguredMuesliSync is the daemon-scheduler entry point for one
// [[muesli]] source.
func runConfiguredMuesliSync(ctx context.Context, st *store.Store, source config.MuesliSource, attachmentsDir string) error {
	return syncConfiguredRecorder(ctx, st, source, "muesli", source.Identifier, func() (*muesli.ImportSummary, error) {
		options := muesliImportOptions(source)
		options.AttachmentsDir = attachmentsDir
		options.AccountEmail, _ = source.EffectiveAccountEmail()
		return muesli.NewImporter(st).Import(ctx, options)
	}, func(s *muesli.ImportSummary) int64 { return s.MeetingsAdded + s.MeetingsUpdated }, rebuildMuesliCacheAfterScheduledSync)
}

func init() {
	syncMuesliCmd.Flags().IntVar(&syncMuesliLimit, "limit", 0, "max meetings processed per run (0 = no limit)")
	syncMuesliCmd.Flags().StringVar(&syncMuesliAfter, "after", "", "only meetings that start on or after this UTC date (YYYY-MM-DD)")
	syncMuesliCmd.Flags().BoolVar(&syncMuesliFull, "full", false, "rewrite every archived meeting, even unchanged ones (refreshes attribution)")
	syncMuesliCmd.Flags().BoolVar(&syncMuesliWatch, "watch", false, "keep rescanning enabled remote recorder sources using their schedules")
	syncMuesliCmd.Flags().Int64Var(&syncMuesliMeetingID, "meeting-id", 0, "only sync the completion-hook meeting")
	_ = syncMuesliCmd.Flags().MarkHidden("meeting-id")
	addManualSyncCacheFlags(syncMuesliCmd)
	rootCmd.AddCommand(addMuesliCmd)
	rootCmd.AddCommand(syncMuesliCmd)
}
