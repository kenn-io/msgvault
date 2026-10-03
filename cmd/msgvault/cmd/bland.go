package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/bland"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

var (
	syncBlandLimit int
	syncBlandAfter string
	syncBlandFull  bool
	syncBlandProbe bool
)

var (
	newBlandClient = func(baseURL, token string) bland.Source {
		return bland.NewClient(baseURL, token)
	}
	rebuildBlandCacheAfterWrite         = rebuildCacheAfterManualSync
	rebuildBlandCacheAfterScheduledSync = rebuildCacheAfterScheduledSync
)

const blandConfigHint = `Add to config.toml:

  [[bland]]
  identifier = "bland-work"
  account_email = "you@example.com"
  api_key = "your-org-api-key"
  enabled = true
  # schedule = "0 */6 * * *"
  # max_media_mb = 250`

func resolveBlandSource(args []string, cfg *config.Config) (*config.BlandSource, error) {
	if cfg == nil {
		return nil, errors.New("configuration is unavailable")
	}
	if len(cfg.Bland) == 0 {
		return nil, errors.New("no [[bland]] sources configured\n\n" + blandConfigHint)
	}
	if len(args) > 0 {
		source := cfg.GetBlandSource(args[0])
		if source == nil {
			identifiers := make([]string, 0, len(cfg.Bland))
			for _, candidate := range cfg.Bland {
				identifiers = append(identifiers, candidate.Identifier)
			}
			return nil, fmt.Errorf("no [[bland]] entry with identifier %q (configured: %s)",
				args[0], strings.Join(identifiers, ", "))
		}
		return source, nil
	}
	if len(cfg.Bland) > 1 {
		return nil, errors.New("multiple [[bland]] sources configured; pass an identifier")
	}
	source := cfg.Bland[0]
	return &source, nil
}

func resolveBlandSources(args []string, probe bool, cfg *config.Config) ([]config.BlandSource, error) {
	if cfg == nil {
		return nil, errors.New("configuration is unavailable")
	}
	if probe || len(args) > 0 || len(cfg.Bland) == 1 {
		source, err := resolveBlandSource(args, cfg)
		if err != nil {
			return nil, err
		}
		return []config.BlandSource{*source}, nil
	}
	if len(cfg.Bland) == 0 {
		return nil, errors.New("no [[bland]] sources configured\n\n" + blandConfigHint)
	}
	return cfg.Bland, nil
}

func runBlandProbe(ctx context.Context, out io.Writer, client bland.Source) error {
	result, err := client.ListCalls(ctx, bland.ListOptions{Limit: 1})
	if err != nil {
		return fmt.Errorf("probe Bland call access: %w", err)
	}
	_, _ = fmt.Fprintf(out, "Bland existing-call access available. Sampled calls: %d\n", len(result.Calls))
	if len(result.Calls) > 0 {
		id := result.Calls[0].ID
		if _, err := client.GetCall(ctx, id); err != nil {
			return err
		}
		_, err := client.GetPostCall(ctx, id)
		if errors.Is(err, bland.ErrNotFound) {
			_, _ = fmt.Fprintln(out, "Retained postcall history unavailable for sampled call.")
		} else if err != nil {
			return err
		} else {
			_, _ = fmt.Fprintln(out, "Retained postcall history available.")
		}
	}
	_, _ = fmt.Fprintln(out, "Direct correction processing/billing semantics unverified; sync calls it only with fetch_corrected_transcript enabled.")
	return nil
}

func configuredBlandClient(source config.BlandSource) bland.Source {
	client := newBlandClient(bland.DefaultBaseURL, source.APIKey)
	if concrete, ok := client.(*bland.Client); ok {
		concrete.EncryptedKey = source.EncryptedKey
	}
	return client
}

var addBlandCmd = &cobra.Command{
	Use:   "add-bland [identifier]",
	Short: "Register and validate a Bland recorded calls source",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		state := invocationFromCommand(cmd)
		if state == nil || state.cfg == nil {
			return errors.New("configuration is unavailable")
		}
		cfg := state.cfg
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		source, err := resolveBlandSource(args, cfg)
		if err != nil {
			return err
		}
		accountEmail, err := source.EffectiveAccountEmail()
		if err != nil {
			return err
		}
		if strings.TrimSpace(source.APIKey) == "" {
			return fmt.Errorf("[[bland]] entry %q has no API key\n\n%s", source.Identifier, blandConfigHint)
		}
		client := configuredBlandClient(*source)
		if err := runBlandProbe(cmd.Context(), cmd.OutOrStdout(), client); err != nil {
			return err
		}
		st, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
		if err != nil {
			return err
		}
		defer cleanup()
		if _, err := registerMeetingSource(cmd.OutOrStdout(), st, bland.SourceType,
			source.Identifier, accountEmail); err != nil {
			return err
		}
		if err := runPostSourceCreateMigrationsForInvocation(st, state); err != nil {
			return fmt.Errorf("post-source-create migrations: %w", err)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "\nBland call source %s registered.\n", source.Identifier)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Run: msgvault sync-bland %s\n", source.Identifier)
		return nil
	},
}

var syncBlandCmd = &cobra.Command{
	Use:   "sync-bland [identifier]",
	Short: "Sync Bland recorded calls",
	Long: `Sync existing Bland call recordings, transcripts and retained postcall enrichment.

Repeated limited runs progress through a frozen discovery window. Due artifact
maintenance is additional. --after implies --full; --probe reports capabilities
without printing call content. Retained enrichment is read by default.
fetch_corrected_transcript explicitly opts into the direct correction GET; its
processing/billing semantics are unverified. No webhook setup is requested.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		state := invocationFromCommand(cmd)
		if state == nil || state.cfg == nil {
			return errors.New("configuration is unavailable")
		}
		cfg := state.cfg
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		sources, err := resolveBlandSources(args, syncBlandProbe, cfg)
		if err != nil {
			return err
		}

		var after time.Time
		if syncBlandAfter != "" {
			parsed, err := time.Parse(time.DateOnly, syncBlandAfter)
			if err != nil {
				return usageErr(cmd, fmt.Errorf("invalid --after %q (expected YYYY-MM-DD): %w", syncBlandAfter, err))
			}
			after = parsed.UTC()
		}
		if syncBlandLimit < 0 {
			return usageErr(cmd, errors.New("--limit must be zero or positive"))
		}
		for _, source := range sources {
			if strings.TrimSpace(source.APIKey) == "" {
				return fmt.Errorf("[[bland]] entry %q has no API key", source.Identifier)
			}
			if _, err := source.EffectiveAccountEmail(); err != nil {
				return err
			}
		}
		if syncBlandProbe {
			source := sources[0]
			return runBlandProbe(cmd.Context(), cmd.OutOrStdout(),
				configuredBlandClient(source))
		}

		st, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
		if err != nil {
			return err
		}
		defer cleanup()
		dbPath := cfg.DatabaseDSN()
		pendingWrites := &bland.ImportSummary{}
		for _, source := range sources {
			if err := requireBlandRegistered(st, source.Identifier); err != nil {
				return finishBlandImport(source.Identifier, pendingWrites, err,
					func() error { return rebuildBlandCacheAfterWrite(dbPath, state) })
			}
			accountEmail, _ := source.EffectiveAccountEmail()
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Syncing Bland calls for %s\n\n", source.Identifier)
			importer := bland.NewImporter(st,
				configuredBlandClient(source))
			summary, importErr := importer.Import(cmd.Context(), bland.ImportOptions{
				Identifier: source.Identifier, AccountEmail: accountEmail,
				Full: syncBlandFull || !after.IsZero(), Limit: syncBlandLimit,
				FetchCorrectedTranscript: source.FetchCorrectedTranscript, CreatedAfter: after, AttachmentsDir: cfg.AttachmentsDir(), MediaPolicy: source.MediaPolicy(),
				Progress: func(line string) { _, _ = fmt.Fprintln(cmd.OutOrStdout(), "  "+line) },
			})
			accumulateBlandWrites(pendingWrites, summary)
			if err := finishBlandImport(source.Identifier, pendingWrites, importErr,
				func() error { return rebuildBlandCacheAfterWrite(dbPath, state) }); err != nil {
				return err
			}
			writeBlandSummary(cmd.OutOrStdout(), summary)
		}
		return rebuildBlandCacheAfterWrite(dbPath, state)
	},
}

func accumulateBlandWrites(total, current *bland.ImportSummary) {
	if total == nil || current == nil {
		return
	}
	total.MeetingsAdded += current.MeetingsAdded
	total.MeetingsUpdated += current.MeetingsUpdated
}

func finishBlandImport(identifier string, summary *bland.ImportSummary, importErr error, refresh func() error) error {
	if importErr == nil {
		return nil
	}
	var refreshErr error
	if summary != nil && summary.MeetingsAdded+summary.MeetingsUpdated > 0 && refresh != nil {
		refreshErr = refresh()
	}
	return errors.Join(fmt.Errorf("bland call sync %s failed: %w", identifier, importErr), refreshErr)
}

func requireBlandRegistered(st *store.Store, identifier string) error {
	if _, err := st.GetSourceByTypeAndIdentifier(bland.SourceType, identifier); err != nil {
		if errors.Is(err, store.ErrSourceNotFound) {
			return fmt.Errorf("bland call source %q is not registered; run msgvault add-bland %s first",
				identifier, identifier)
		}
		return err
	}
	return nil
}

func writeBlandSummary(out io.Writer, summary *bland.ImportSummary) {
	_, _ = fmt.Fprintln(out, "\nBland calls sync complete!")
	_, _ = fmt.Fprintf(out, "  Meetings processed: %d\n", summary.MeetingsProcessed)
	_, _ = fmt.Fprintf(out, "  Meetings added:     %d\n", summary.MeetingsAdded)
	_, _ = fmt.Fprintf(out, "  Meetings updated:   %d\n", summary.MeetingsUpdated)
	if summary.Skipped > 0 {
		_, _ = fmt.Fprintf(out, "  Calls without ready artifacts: %d\n", summary.Skipped)
	}
	if summary.Errors > 0 {
		_, _ = fmt.Fprintf(out, "  Calls with unavailable provider evidence: %d\n", summary.Errors)
	}
	if summary.MaintenanceRetries > 0 {
		_, _ = fmt.Fprintf(out, "  Maintenance items: %d\n", summary.MaintenanceRetries)
	}
	if summary.PartialCoverage {
		_, _ = fmt.Fprintln(out, "  Coverage: partial discovery; rerun to continue")
	}
}

func runConfiguredBlandSync(ctx context.Context, st *store.Store, source config.BlandSource) error {
	state := invocationFromContext(ctx)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	if err := requireBlandRegistered(st, source.Identifier); err != nil {
		return err
	}
	if strings.TrimSpace(source.APIKey) == "" {
		return fmt.Errorf("bland call source %q has no API key", source.Identifier)
	}
	accountEmail, err := source.EffectiveAccountEmail()
	if err != nil {
		return err
	}
	importer := bland.NewImporter(st,
		configuredBlandClient(source))
	summary, importErr := importer.Import(ctx, bland.ImportOptions{
		FetchCorrectedTranscript: source.FetchCorrectedTranscript, Identifier: source.Identifier, AccountEmail: accountEmail, AttachmentsDir: state.cfg.AttachmentsDir(), MediaPolicy: source.MediaPolicy(),
	})
	refreshCtx := context.WithoutCancel(ctx)
	refresh := func() error {
		return rebuildBlandCacheAfterScheduledSync(refreshCtx, "bland:"+source.Identifier)
	}
	if err := finishBlandImport(source.Identifier, summary, importErr, refresh); err != nil {
		return err
	}
	return refresh()
}

func init() {
	syncBlandCmd.Flags().IntVar(&syncBlandLimit, "limit", 0,
		"max newly discovered calls per run; due artifact maintenance is additional (0 = unlimited)")
	syncBlandCmd.Flags().StringVar(&syncBlandAfter, "after", "",
		"call creation lower bound (YYYY-MM-DD; implies --full)")
	syncBlandCmd.Flags().BoolVar(&syncBlandFull, "full", false,
		"recheck full history and terminal artifact availability")
	syncBlandCmd.Flags().BoolVar(&syncBlandProbe, "probe", false,
		"validate capabilities and result shape without printing meeting content")
	rootCmd.AddCommand(addBlandCmd)
	rootCmd.AddCommand(addManualSyncCacheFlags(syncBlandCmd))
}
