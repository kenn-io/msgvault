package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/muesli"
	"go.kenn.io/msgvault/internal/store"
)

var (
	syncMuesliLimit int
	syncMuesliAfter string
	syncMuesliFull  bool
)

var (
	rebuildMuesliCacheAfterWrite         = rebuildCacheAfterWrite
	rebuildMuesliCacheAfterScheduledSync = rebuildCacheAfterScheduledSync
)

const muesliConfigHint = `Add to your config.toml:

  [[muesli]]
  identifier = "mac"                  # stable label for this Mac's Muesli database
  account_email = "you@example.com"   # you, the person who records the meetings
  enabled = true
  # db_path = "~/Library/Application Support/Muesli/muesli.db"  # default shown
  # schedule = "*/30 * * * *"         # optional daemon schedule
  # phone_country_code = "1"          # convert national-format Contacts phones
  # contacts = false                  # skip Apple Contacts attendee lookup`

// resolveMuesliSources picks [[muesli]] entries: an explicit identifier must
// match one entry; with no argument every configured entry is returned.
func resolveMuesliSources(args []string) ([]config.MuesliSource, error) {
	if len(cfg.Muesli) == 0 {
		return nil, errors.New("no [[muesli]] sources configured\n\n" + muesliConfigHint)
	}
	if len(args) == 0 {
		return cfg.Muesli, nil
	}
	source := cfg.GetMuesliSource(args[0])
	if source == nil {
		identifiers := make([]string, 0, len(cfg.Muesli))
		for _, candidate := range cfg.Muesli {
			identifiers = append(identifiers, candidate.Identifier)
		}
		return nil, fmt.Errorf("no [[muesli]] entry with identifier %q (configured: %s)",
			args[0], strings.Join(identifiers, ", "))
	}
	return []config.MuesliSource{*source}, nil
}

// probeMuesliDatabase proves the configured database opens read-only and
// has Muesli's meetings table.
func probeMuesliDatabase(ctx context.Context, path string) error {
	reader, err := muesli.Open(ctx, path)
	if err != nil {
		return fmt.Errorf("%w\n\nCheck db_path in the [[muesli]] entry. msgvault reads the database on the "+
			"daemon's host; if macOS blocks the read, grant the daemon Full Disk Access", err)
	}
	return reader.Close()
}

var addMuesliCmd = &cobra.Command{
	Use:   "add-muesli [identifier]",
	Short: "Register a local Muesli meeting database",
	Long: `Register a configured Muesli database as a msgvault meeting source.

Reads db_path from the matching [[muesli]] entry in config.toml (default:
~/Library/Application Support/Muesli/muesli.db) and checks that it opens
read-only as a Muesli database. The daemon reads the file on its own host,
so msgvault must run on the Mac where Muesli records.

Examples:
  msgvault add-muesli
  msgvault add-muesli mac`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		sources, err := resolveMuesliSources(args)
		if err != nil {
			return err
		}
		if len(sources) > 1 {
			return errors.New("multiple [[muesli]] sources configured; pass an identifier")
		}
		source := sources[0]
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
					"Grant the msgvault daemon Full Disk Access so attendees picked from Contacts link to people.")
			}
		}
		st, cleanup, err := openWritableStoreAndInitForIngest()
		if err != nil {
			return err
		}
		defer cleanup()
		if _, err := registerMeetingSource(cmd.OutOrStdout(), st, sourceTypeMuesli,
			source.Identifier, accountEmail); err != nil {
			return err
		}
		if err := runPostSourceCreateMigrations(st); err != nil {
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
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		sources, err := resolveMuesliSources(args)
		if err != nil {
			return err
		}
		var after time.Time
		if syncMuesliAfter != "" {
			parsed, err := time.Parse(time.DateOnly, syncMuesliAfter)
			if err != nil {
				return usageErr(cmd, fmt.Errorf("invalid --after %q (expected YYYY-MM-DD): %w", syncMuesliAfter, err))
			}
			after = parsed.UTC()
		}
		for _, source := range sources {
			if _, err := source.EffectiveAccountEmail(); err != nil {
				return err
			}
		}

		st, cleanup, err := openWritableStoreAndInitForIngest()
		if err != nil {
			return err
		}
		defer cleanup()
		dbPath := cfg.DatabaseDSN()

		ctx, cancel := context.WithCancel(cmd.Context())
		defer cancel()
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(sigChan)
		go func() {
			select {
			case <-sigChan:
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "\nInterrupted. Finishing current meeting...")
				cancel()
			case <-ctx.Done():
			}
		}()

		pendingWrites := &muesli.ImportSummary{}
		for _, source := range sources {
			accountEmail, _ := source.EffectiveAccountEmail()
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Syncing Muesli for %s\n\n", source.Identifier)
			options := muesliImportOptions(source)
			options.AccountEmail = accountEmail
			options.Full, options.Limit, options.StartedAfter = syncMuesliFull, syncMuesliLimit, after
			options.Progress = func(line string) { _, _ = fmt.Fprintln(cmd.OutOrStdout(), "  "+line) }
			summary, importErr := muesli.NewImporter(st).Import(ctx, options)
			if summary != nil {
				pendingWrites.MeetingsAdded += summary.MeetingsAdded
				pendingWrites.MeetingsUpdated += summary.MeetingsUpdated
			}
			if err := finishMuesliImport(source.Identifier, pendingWrites, importErr,
				func() error { return rebuildMuesliCacheAfterWrite(dbPath) }); err != nil {
				return err
			}
			writeMuesliSummary(cmd.OutOrStdout(), summary)
		}
		return rebuildMuesliCacheAfterWrite(dbPath)
	},
}

// finishMuesliImport reports a failed sync, first refreshing the cache when
// the run still archived meetings.
func finishMuesliImport(identifier string, summary *muesli.ImportSummary, importErr error, refresh func() error) error {
	if importErr == nil {
		return nil
	}
	var refreshErr error
	if summary != nil && summary.MeetingsAdded+summary.MeetingsUpdated > 0 && refresh != nil {
		refreshErr = refresh()
	}
	return errors.Join(fmt.Errorf("muesli sync %s failed: %w", identifier, importErr), refreshErr)
}

func writeMuesliSummary(out io.Writer, summary *muesli.ImportSummary) {
	_, _ = fmt.Fprintln(out, "\nMuesli sync complete!")
	_, _ = fmt.Fprintf(out, "  Duration:           %s\n", summary.Duration.Round(time.Second))
	_, _ = fmt.Fprintf(out, "  Meetings processed: %d\n", summary.MeetingsProcessed)
	_, _ = fmt.Fprintf(out, "  Meetings added:     %d\n", summary.MeetingsAdded)
	_, _ = fmt.Fprintf(out, "  Meetings updated:   %d\n", summary.MeetingsUpdated)
	if summary.SkippedInProgress > 0 {
		_, _ = fmt.Fprintf(out, "  Still in progress:  %d (archived by a later sync)\n", summary.SkippedInProgress)
	}
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
		_, _ = fmt.Fprintln(out, "  Grant the msgvault daemon Full Disk Access so attendees picked from Contacts link to people.")
	}
}

// muesliImportOptions carries a configured source's paths and Contacts
// settings into an import.
func muesliImportOptions(source config.MuesliSource) muesli.ImportOptions {
	return muesli.ImportOptions{
		Identifier: source.Identifier, AccountEmail: source.AccountEmail,
		DBPath:          source.EffectiveDBPath(),
		ContactsEnabled: source.ContactsEnabled(), ContactsPath: source.EffectiveContactsPath(),
		PhoneCountryCode: source.PhoneCountryCode,
	}
}

// runConfiguredMuesliSync is the daemon-scheduler entry point for one
// [[muesli]] source.
func runConfiguredMuesliSync(ctx context.Context, st *store.Store, source config.MuesliSource) error {
	if _, err := st.GetSourceByTypeAndIdentifier(muesli.SourceType, source.Identifier); err != nil {
		if errors.Is(err, store.ErrSourceNotFound) {
			return fmt.Errorf("muesli source %q is not registered; run msgvault add-muesli %s first",
				source.Identifier, source.Identifier)
		}
		return err
	}
	accountEmail, err := source.EffectiveAccountEmail()
	if err != nil {
		return err
	}
	options := muesliImportOptions(source)
	options.AccountEmail = accountEmail
	summary, importErr := muesli.NewImporter(st).Import(ctx, options)
	refreshCtx := context.WithoutCancel(ctx)
	refresh := func() error {
		return rebuildMuesliCacheAfterScheduledSync(refreshCtx, "muesli:"+source.Identifier)
	}
	if err := finishMuesliImport(source.Identifier, summary, importErr, refresh); err != nil {
		return err
	}
	return refresh()
}

func init() {
	syncMuesliCmd.Flags().IntVar(&syncMuesliLimit, "limit", 0, "max meetings processed per run (0 = no limit)")
	syncMuesliCmd.Flags().StringVar(&syncMuesliAfter, "after", "", "only meetings that start on or after this UTC date (YYYY-MM-DD)")
	syncMuesliCmd.Flags().BoolVar(&syncMuesliFull, "full", false, "rewrite every archived meeting, even unchanged ones (refreshes attribution)")
	rootCmd.AddCommand(addMuesliCmd)
	rootCmd.AddCommand(syncMuesliCmd)
}
