package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/twenty"
)

var (
	syncTwentyLimit                      int
	syncTwentyAfter                      string
	syncTwentyFull                       bool
	syncTwentyProbe                      bool
	newTwentyClient                      = func(baseURL, key string) (twenty.Source, error) { return twenty.NewClient(baseURL, key) }
	rebuildTwentyCacheAfterWrite         = rebuildCacheAfterManualSync
	rebuildTwentyCacheAfterScheduledSync = rebuildCacheAfterScheduledSync
)

const twentyConfigHint = `Add to your daemon's config.toml:

  [[twenty]]
  identifier = "work"
  account_email = "you@example.com"
  base_url = "https://api.twenty.com" # Cloud API; self-hosted: instance origin
  api_key = "YOUR_READ_ONLY_API_KEY"
  enabled = true
  # schedule = "15 */6 * * *"`

func resolveTwentySources(args []string, single bool, cfg *config.Config) ([]config.TwentySource, error) {
	if cfg == nil {
		return nil, errors.New("configuration is unavailable")
	}
	if len(cfg.Twenty) == 0 {
		return nil, errors.New("no [[twenty]] sources configured\n\n" + twentyConfigHint)
	}
	if len(args) > 0 {
		source := cfg.GetTwentySource(args[0])
		if source != nil {
			return []config.TwentySource{*source}, nil
		}
		var identifiers []string
		for _, s := range cfg.Twenty {
			identifiers = append(identifiers, s.Identifier)
		}
		return nil, fmt.Errorf("no [[twenty]] entry with identifier %q (configured: %s)", args[0], strings.Join(identifiers, ", "))
	}
	if single && len(cfg.Twenty) > 1 {
		return nil, errors.New("multiple [[twenty]] sources configured; pass an identifier")
	}
	return cfg.Twenty, nil
}

func runTwentyProbe(ctx context.Context, out io.Writer, client twenty.Source) error {
	if err := client.Probe(ctx); err != nil {
		return fmt.Errorf("probe Twenty access: %w", err)
	}
	_, _ = fmt.Fprintln(out, "Twenty probe succeeded.\n  Recording, calendar and participant access: available")
	return nil
}

var addTwentyCmd = &cobra.Command{
	Use: "add-twenty [identifier]", Short: "Register and validate a Twenty Call Recorder source", Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		state := invocationFromCommand(cmd)
		if state == nil || state.cfg == nil {
			return errors.New("configuration is unavailable")
		}
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		sources, err := resolveTwentySources(args, true, state.cfg)
		if err != nil {
			return err
		}
		source := sources[0]
		email, err := source.EffectiveAccountEmail()
		if err != nil {
			return err
		}
		client, err := newTwentyClient(source.BaseURL, source.APIKey)
		if err != nil {
			return err
		}
		if err := runTwentyProbe(cmd.Context(), cmd.OutOrStdout(), client); err != nil {
			return err
		}
		st, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
		if err != nil {
			return err
		}
		defer cleanup()
		if _, err := registerMeetingSource(cmd.OutOrStdout(), st, sourceTypeTwenty, source.Identifier, email); err != nil {
			return err
		}
		if err := runPostSourceCreateMigrationsForInvocation(st, state); err != nil {
			return fmt.Errorf("post-source-create migrations: %w", err)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "\nTwenty source %s registered.\nRun: msgvault sync-twenty %s\n", source.Identifier, source.Identifier)
		return nil
	},
}

var syncTwentyCmd = &cobra.Command{
	Use: "sync-twenty [identifier]", Short: "Sync Twenty Call Recorder meetings",
	Long: `Sync summaries and diarized transcripts from Twenty's read-only API.

Each run reads recordings updated since the last successful run. --full
rescans every recording, which also picks up later attendee edits. --after
filters meeting dates locally. --limit caps eligible meetings and reports
incomplete scans. --probe validates access without archive writes.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		state := invocationFromCommand(cmd)
		if state == nil || state.cfg == nil {
			return errors.New("configuration is unavailable")
		}
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		sources, err := resolveTwentySources(args, syncTwentyProbe, state.cfg)
		if err != nil {
			return err
		}
		if syncTwentyLimit < 0 {
			return usageErr(cmd, errors.New("--limit must be nonnegative"))
		}
		var after time.Time
		if syncTwentyAfter != "" {
			after, err = time.Parse(time.DateOnly, syncTwentyAfter)
			if err != nil {
				return usageErr(cmd, errors.New("invalid --after (expected YYYY-MM-DD)"))
			}
		}
		clients := make([]twenty.Source, len(sources))
		clientErrors := make([]error, len(sources))
		emails := make([]string, len(sources))
		for i, source := range sources {
			email, err := source.EffectiveAccountEmail()
			if err != nil {
				clientErrors[i] = fmt.Errorf("twenty source %q: %w", source.Identifier, err)
				continue
			}
			emails[i] = email
			client, err := newTwentyClient(source.BaseURL, source.APIKey)
			if err != nil {
				clientErrors[i] = fmt.Errorf("twenty source %q: %w", source.Identifier, err)
				continue
			}
			clients[i] = client
		}
		if syncTwentyProbe {
			if clientErrors[0] != nil {
				return clientErrors[0]
			}
			if err := runTwentyProbe(cmd.Context(), cmd.OutOrStdout(), clients[0]); err != nil {
				return fmt.Errorf("twenty source %q: %w", sources[0].Identifier, err)
			}
			return nil
		}
		ready := false
		var setupErrors []error
		for i, client := range clients {
			if client != nil {
				ready = true
			}
			if clientErrors[i] != nil {
				setupErrors = append(setupErrors, clientErrors[i])
			}
		}
		if !ready {
			return errors.Join(setupErrors...)
		}
		st, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
		if err != nil {
			return err
		}
		defer cleanup()
		pendingWrites := &twenty.ImportSummary{}
		refresh := func() error { return rebuildTwentyCacheAfterWrite(state.cfg.DatabaseDSN(), state) }
		var syncErrors []error
		importFailed := false
		for i, source := range sources {
			if err := cmd.Context().Err(); err != nil {
				syncErrors = append(syncErrors, err)
				break
			}
			if clientErrors[i] != nil {
				syncErrors = append(syncErrors, clientErrors[i])
				continue
			}
			email := emails[i]
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Syncing Twenty meetings for %s\n", source.Identifier)
			summary, importErr := twenty.NewImporter(st, clients[i]).Import(cmd.Context(), twenty.ImportOptions{Identifier: source.Identifier, AccountEmail: email, Full: syncTwentyFull, Limit: syncTwentyLimit, StartedAfter: after, Progress: func(line string) { _, _ = fmt.Fprintln(cmd.OutOrStdout(), "  "+line) }})
			accumulateTwentyWrites(pendingWrites, summary)
			if importErr != nil {
				importFailed = true
				syncErrors = append(syncErrors, finishTwentyImport(source.Identifier, summary, importErr, nil))
				if cmd.Context().Err() != nil {
					break
				}
				continue
			}
			writeTwentySummary(cmd.OutOrStdout(), summary)
		}
		if !importFailed || pendingWrites.MeetingsAdded+pendingWrites.MeetingsUpdated > 0 {
			if err := refresh(); err != nil {
				syncErrors = append(syncErrors, err)
			}
		}
		return errors.Join(syncErrors...)
	},
}

func accumulateTwentyWrites(total, current *twenty.ImportSummary) {
	if total == nil || current == nil {
		return
	}
	total.MeetingsAdded += current.MeetingsAdded
	total.MeetingsUpdated += current.MeetingsUpdated
}
func finishTwentyImport(identifier string, summary *twenty.ImportSummary, importErr error, refresh func() error) error {
	if importErr == nil {
		return nil
	}
	var refreshErr error
	if summary != nil && summary.MeetingsAdded+summary.MeetingsUpdated > 0 && refresh != nil {
		refreshErr = refresh()
	}
	return errors.Join(fmt.Errorf("twenty sync %s failed: %w", identifier, importErr), refreshErr)
}
func writeTwentySummary(out io.Writer, summary *twenty.ImportSummary) {
	_, _ = fmt.Fprintf(out, "\nTwenty sync complete!\n  Meetings processed: %d\n  Meetings added: %d\n  Meetings updated: %d\n  Empty: %d\n", summary.MeetingsProcessed, summary.MeetingsAdded, summary.MeetingsUpdated, summary.SkippedEmpty)
	if summary.SkippedInvalid > 0 {
		_, _ = fmt.Fprintf(out, "  Skipped (invalid evidence): %d\n", summary.SkippedInvalid)
	}
	if summary.PartialCoverage {
		_, _ = fmt.Fprintln(out, "  Coverage: partial (limit stopped catalog scan)")
	}
}
func runConfiguredTwentySync(ctx context.Context, st *store.Store, source config.TwentySource) error {
	if _, err := st.GetSourceByTypeAndIdentifier(twenty.SourceType, source.Identifier); err != nil {
		if errors.Is(err, store.ErrSourceNotFound) {
			return fmt.Errorf("twenty source %q is not registered; run msgvault add-twenty %s first", source.Identifier, source.Identifier)
		}
		return err
	}
	email, err := source.EffectiveAccountEmail()
	if err != nil {
		return err
	}
	client, err := newTwentyClient(source.BaseURL, source.APIKey)
	if err != nil {
		return err
	}
	summary, importErr := twenty.NewImporter(st, client).Import(ctx, twenty.ImportOptions{Identifier: source.Identifier, AccountEmail: email})
	refresh := func() error {
		return rebuildTwentyCacheAfterScheduledSync(context.WithoutCancel(ctx), "twenty:"+source.Identifier)
	}
	if err := finishTwentyImport(source.Identifier, summary, importErr, refresh); err != nil {
		return err
	}
	return refresh()
}

func init() {
	syncTwentyCmd.Flags().IntVar(&syncTwentyLimit, "limit", 0, "max eligible meetings processed (0 = unlimited)")
	syncTwentyCmd.Flags().StringVar(&syncTwentyAfter, "after", "", "local meeting-date lower bound (YYYY-MM-DD, UTC)")
	syncTwentyCmd.Flags().BoolVar(&syncTwentyFull, "full", false, "rescan every recording and refresh archive projections and attribution")
	syncTwentyCmd.Flags().BoolVar(&syncTwentyProbe, "probe", false, "validate read access without printing content or writing the archive")
	rootCmd.AddCommand(addTwentyCmd)
	rootCmd.AddCommand(addManualSyncCacheFlags(syncTwentyCmd))
}
