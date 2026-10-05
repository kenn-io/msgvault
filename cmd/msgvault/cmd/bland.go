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
	newBlandClient                      = bland.NewClient
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

func runBlandProbe(ctx context.Context, out io.Writer, client *bland.Client) error {
	result, err := client.ListCalls(ctx, bland.ListOptions{Limit: 1, Newest: true})
	if err != nil {
		return fmt.Errorf("probe Bland call access: %w", err)
	}
	_, _ = fmt.Fprintf(out, "Bland existing-call access available. Sampled calls: %d\n", len(result.Calls))
	if len(result.Calls) > 0 {
		id := result.Calls[0].ID
		// The importer archives without details or postcall data Bland lacks or
		// refuses, so the probe only notes them.
		if _, err := client.GetCall(ctx, id); !probeNote(out, "Call details", err) {
			return err
		}
		if _, err := client.GetPostCall(ctx, id); !probeNote(out, "Retained postcall history", err) {
			return err
		}
	}
	return nil
}

// probeNote prints what the sampled call's read found and reports whether the
// probe can go on: a missing or refused read is a note, any other error isn't.
func probeNote(out io.Writer, what string, err error) bool {
	httpErr, isHTTP := errors.AsType[*bland.HTTPError](err)
	switch {
	case err == nil:
		_, _ = fmt.Fprintf(out, "%s available.\n", what)
	case errors.Is(err, bland.ErrNotFound):
		_, _ = fmt.Fprintf(out, "%s unavailable for sampled call.\n", what)
	case isHTTP && !httpErr.Retryable():
		_, _ = fmt.Fprintf(out, "%s refused for sampled call (HTTP %d).\n", what, httpErr.StatusCode)
	default:
		return false
	}
	return true
}

func configuredBlandClient(source config.BlandSource) *bland.Client {
	client := newBlandClient(bland.DefaultBaseURL, source.APIKey)
	client.EncryptedKey = source.EncryptedKey
	return client
}

// newBlandSourceClient checks one source's API key and builds its client.
func newBlandSourceClient(source config.BlandSource) (*bland.Client, error) {
	if strings.TrimSpace(source.APIKey) == "" {
		return nil, fmt.Errorf("[[bland]] entry %q has no API key\n\n%s", source.Identifier, blandConfigHint)
	}
	return configuredBlandClient(source), nil
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
		client, err := newBlandSourceClient(*source)
		if err != nil {
			return err
		}
		if err := runBlandProbe(cmd.Context(), cmd.OutOrStdout(), client); err != nil {
			return err
		}
		st, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
		if err != nil {
			return err
		}
		defer cleanup()
		if _, err := registerMeetingSource(cmd.OutOrStdout(), st, bland.SourceType,
			source.Identifier, source.AccountEmail); err != nil {
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
	Long: `Archive Bland calls, their summaries, retained transcripts and recordings as meetings.
With no identifier, sync every configured [[bland]] source. --limit bounds the
calls fetched, and a limited run that stops early prints the command that
resumes it. --after uses a UTC date and implies
--full. --probe reports capabilities without printing call content. This
command does not place calls or request webhooks.`,
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
		if syncBlandProbe {
			client, err := newBlandSourceClient(sources[0])
			if err != nil {
				return err
			}
			return runBlandProbe(cmd.Context(), cmd.OutOrStdout(), client)
		}

		st, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
		if err != nil {
			return err
		}
		defer cleanup()
		dbPath := cfg.DatabaseDSN()
		// Each source syncs even when another fails; the cache refreshes once.
		written := &bland.ImportSummary{}
		var errs []error
		for _, source := range sources {
			err := requireBlandRegistered(st, source.Identifier)
			var client *bland.Client
			if err == nil {
				client, err = newBlandSourceClient(source)
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("bland sync %s failed: %w", source.Identifier, err))
				continue
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Syncing Bland calls for %s\n\n", source.Identifier)
			summary, importErr := bland.NewImporter(st, client).Import(cmd.Context(), bland.ImportOptions{
				Identifier: source.Identifier, AccountEmail: source.AccountEmail,
				Full: syncBlandFull || !after.IsZero(), Limit: syncBlandLimit,
				CreatedAfter: after, AttachmentsDir: cfg.AttachmentsDir(), MediaPolicy: source.MediaPolicy(),
			})
			accumulateCallWrites(written, summary)
			if summary != nil {
				writeCallSyncSummary(cmd.OutOrStdout(), "Bland", "sync-bland", source.Identifier, summary, importErr != nil, callResumeFlags(syncBlandLimit, syncBlandAfter, syncBlandFull))
			}
			if importErr != nil {
				errs = append(errs, fmt.Errorf("bland sync %s failed: %w", source.Identifier, importErr))
			}
		}
		return finishCallSync(errors.Join(errs...), written, func() error { return rebuildBlandCacheAfterWrite(dbPath, state) })
	},
}

// requireBlandRegistered checks the source is registered before its API key,
// so an unregistered entry names the command that registers it.
func requireBlandRegistered(st *store.Store, identifier string) error {
	if _, err := st.GetSourceByTypeAndIdentifier(bland.SourceType, identifier); err != nil {
		if errors.Is(err, store.ErrSourceNotFound) {
			return fmt.Errorf("bland call source %q is not registered; run msgvault add-bland %s first", identifier, identifier)
		}
		return err
	}
	return nil
}

func runConfiguredBlandSync(ctx context.Context, st *store.Store, source config.BlandSource) error {
	if err := requireBlandRegistered(st, source.Identifier); err != nil {
		return err
	}
	state := invocationFromContext(ctx)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	client, err := newBlandSourceClient(source)
	if err != nil {
		return err
	}
	summary, importErr := bland.NewImporter(st, client).Import(ctx, bland.ImportOptions{
		Identifier: source.Identifier, AccountEmail: source.AccountEmail,
		AttachmentsDir: state.cfg.AttachmentsDir(), MediaPolicy: source.MediaPolicy(),
	})
	if summary != nil && state.logger != nil {
		for _, diagnostic := range summary.Diagnostics {
			state.logger.Warn("bland sync diagnostic", "source", source.Identifier, "diagnostic", diagnostic)
		}
	}
	refreshCtx := context.WithoutCancel(ctx)
	refresh := func() error { return rebuildBlandCacheAfterScheduledSync(refreshCtx, "bland:"+source.Identifier) }
	if importErr != nil {
		importErr = fmt.Errorf("bland sync %s failed: %w", source.Identifier, importErr)
	}
	return finishCallSync(importErr, summary, refresh)
}

func init() {
	syncBlandCmd.Flags().IntVar(&syncBlandLimit, "limit", 0,
		"max calls fetched per run (0 = unlimited)")
	syncBlandCmd.Flags().StringVar(&syncBlandAfter, "after", "",
		"UTC creation lower bound (YYYY-MM-DD; implies --full)")
	syncBlandCmd.Flags().BoolVar(&syncBlandFull, "full", false,
		"recheck full history and terminal artifact availability")
	syncBlandCmd.Flags().BoolVar(&syncBlandProbe, "probe", false,
		"validate capabilities and result shape without printing meeting content")
	rootCmd.AddCommand(addBlandCmd)
	rootCmd.AddCommand(addManualSyncCacheFlags(syncBlandCmd))
}
