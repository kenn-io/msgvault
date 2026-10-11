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
	"go.kenn.io/msgvault/internal/twilio"
)

var (
	syncTwilioLimit int
	syncTwilioAfter string
	syncTwilioFull  bool
	syncTwilioProbe bool
)

var (
	newTwilioClient                      = twilio.NewClient
	rebuildTwilioCacheAfterWrite         = rebuildCacheAfterManualSync
	rebuildTwilioCacheAfterScheduledSync = rebuildCacheAfterScheduledSync
)

func twilioClientOptions(source config.TwilioSource) twilio.Options {
	return twilio.Options{
		AccountSID: source.AccountSID, APIKeySID: source.APIKeySID,
		APIKeySecret: source.APIKeySecret, AuthToken: source.AuthToken,
		Region: source.Region, IntelligenceServiceSID: source.IntelligenceServiceSID,
	}
}

// newTwilioSourceClient builds one source's client; config load already
// validated and normalized its account email.
func newTwilioSourceClient(source config.TwilioSource) (*twilio.Client, string, error) {
	client, err := newTwilioClient(twilioClientOptions(source))
	return client, source.AccountEmail, err
}

const twilioConfigHint = `Add to your config.toml:

  [[twilio]]
  identifier = "twilio-personal"      # stable label for this identity
  account_email = "you@example.com"   # primary identity for relationships
  account_sid = "AC..."               # explicit account or subaccount
  api_key_sid = "SK..."               # region-specific key
  api_key_secret = "..."
  # auth_token = "..."                # alternative to API key credentials
  region = "us1"                      # ie1 and au1 are also supported
  enabled = true
  # schedule = "15 */6 * * *"         # optional daemon schedule`

func resolveTwilioSource(args []string, cfg *config.Config) (*config.TwilioSource, error) {
	if cfg == nil {
		return nil, errors.New("configuration is unavailable")
	}
	if len(cfg.Twilio) == 0 {
		return nil, errors.New("no [[twilio]] sources configured\n\n" + twilioConfigHint)
	}
	if len(args) > 0 {
		source := cfg.GetTwilioSource(args[0])
		if source == nil {
			identifiers := make([]string, 0, len(cfg.Twilio))
			for _, candidate := range cfg.Twilio {
				identifiers = append(identifiers, candidate.Identifier)
			}
			return nil, fmt.Errorf("no [[twilio]] entry with identifier %q (configured: %s)",
				args[0], strings.Join(identifiers, ", "))
		}
		return source, nil
	}
	if len(cfg.Twilio) > 1 {
		return nil, errors.New("multiple [[twilio]] sources configured; pass an identifier")
	}
	source := cfg.Twilio[0]
	return &source, nil
}

func resolveTwilioSources(args []string, probe bool, cfg *config.Config) ([]config.TwilioSource, error) {
	if cfg == nil {
		return nil, errors.New("configuration is unavailable")
	}
	if probe || len(args) > 0 || len(cfg.Twilio) == 1 {
		source, err := resolveTwilioSource(args, cfg)
		if err != nil {
			return nil, err
		}
		return []config.TwilioSource{*source}, nil
	}
	if len(cfg.Twilio) == 0 {
		return nil, errors.New("no [[twilio]] sources configured\n\n" + twilioConfigHint)
	}
	return cfg.Twilio, nil
}

func runTwilioProbe(ctx context.Context, out io.Writer, client *twilio.Client) error {
	recordings, _, err := client.ListRecordingsPage(ctx, time.Time{}, 1, "")
	if err != nil {
		return fmt.Errorf("probe Twilio recording access: %w", err)
	}
	_, _ = fmt.Fprintln(out, "Twilio recording probe succeeded.")
	_, _ = fmt.Fprintf(out, "  Recordings on first page (sample): %d\n", len(recordings))
	_, _ = fmt.Fprintln(out, "  Transcript products and audio retrieval: checked during sync")
	return nil
}

var addTwilioCmd = &cobra.Command{
	Use:   "add-twilio [identifier]",
	Short: "Register and validate a Twilio call source",
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
		source, err := resolveTwilioSource(args, cfg)
		if err != nil {
			return err
		}
		client, accountEmail, err := newTwilioSourceClient(*source)
		if err != nil {
			return err
		}
		if err := runTwilioProbe(cmd.Context(), cmd.OutOrStdout(), client); err != nil {
			return err
		}
		st, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
		if err != nil {
			return err
		}
		defer cleanup()
		if _, err := registerMeetingSource(cmd.OutOrStdout(), st, sourceTypeTwilio,
			source.Identifier, accountEmail); err != nil {
			return err
		}
		if err := runPostSourceCreateMigrationsForInvocation(st, state); err != nil {
			return fmt.Errorf("post-source-create migrations: %w", err)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "\nTwilio source %s registered.\n", source.Identifier)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Run: msgvault sync-twilio %s\n", source.Identifier)
		return nil
	},
}

var syncTwilioCmd = &cobra.Command{
	Use:   "sync-twilio [identifier]",
	Short: "Sync Twilio calls and recordings",
	Long: `Archive existing Twilio calls, recording audio, and retained transcripts as meetings.
With no identifier, sync every configured [[twilio]] source. --limit bounds call
processing, and a limited run that stops early prints the command that resumes
it. --after uses a UTC date and implies --full. --probe reports sample counts without printing call evidence.
This command does not place calls or enable paid processing.`,
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
		sources, err := resolveTwilioSources(args, syncTwilioProbe, cfg)
		if err != nil {
			return err
		}

		var after time.Time
		if syncTwilioAfter != "" {
			parsed, err := time.Parse(time.DateOnly, syncTwilioAfter)
			if err != nil {
				return usageErr(cmd, fmt.Errorf("invalid --after %q (expected YYYY-MM-DD): %w", syncTwilioAfter, err))
			}
			after = parsed.UTC()
		}
		if syncTwilioProbe {
			client, _, err := newTwilioSourceClient(sources[0])
			if err != nil {
				return err
			}
			return runTwilioProbe(cmd.Context(), cmd.OutOrStdout(), client)
		}

		st, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
		if err != nil {
			return err
		}
		defer cleanup()
		dbPath := cfg.DatabaseDSN()
		// Each source syncs even when another fails; the cache refreshes once.
		written := &twilio.ImportSummary{}
		var errs []error
		for _, source := range sources {
			client, accountEmail, err := newTwilioSourceClient(source)
			if err == nil {
				err = requireTwilioRegistered(st, source.Identifier)
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("twilio sync %s failed: %w", source.Identifier, err))
				continue
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Syncing Twilio calls for %s\n\n", source.Identifier)
			summary, importErr := twilio.NewImporter(st, client).Import(cmd.Context(), twilio.ImportOptions{
				Identifier: source.Identifier, AccountEmail: accountEmail,
				Full: syncTwilioFull || !after.IsZero(), Limit: syncTwilioLimit,
				CreatedAfter: after, AttachmentsDir: cfg.AttachmentsDir(), MediaPolicy: source.MediaPolicy(),
			})
			accumulateCallWrites(written, summary)
			if summary != nil {
				writeCallSyncSummary(cmd.OutOrStdout(), "Twilio", "sync-twilio", source.Identifier, summary, importErr != nil, callResumeFlags(syncTwilioLimit, syncTwilioAfter, syncTwilioFull))
			}
			if importErr != nil {
				errs = append(errs, fmt.Errorf("twilio sync %s failed: %w", source.Identifier, importErr))
			}
		}
		return finishCallSync(errors.Join(errs...), written, func() error { return rebuildTwilioCacheAfterWrite(dbPath, state) })
	},
}

func requireTwilioRegistered(st *store.Store, identifier string) error {
	if _, err := st.GetSourceByTypeAndIdentifier(twilio.SourceType, identifier); err != nil {
		if errors.Is(err, store.ErrSourceNotFound) {
			return fmt.Errorf("twilio source %q is not registered; run msgvault add-twilio %s first", identifier, identifier)
		}
		return err
	}
	return nil
}

func runConfiguredTwilioSync(ctx context.Context, st *store.Store, source config.TwilioSource) error {
	if err := requireTwilioRegistered(st, source.Identifier); err != nil {
		return err
	}
	state := invocationFromContext(ctx)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	client, accountEmail, err := newTwilioSourceClient(source)
	if err != nil {
		return err
	}
	summary, importErr := twilio.NewImporter(st, client).Import(ctx, twilio.ImportOptions{
		Identifier: source.Identifier, AccountEmail: accountEmail,
		AttachmentsDir: state.cfg.AttachmentsDir(), MediaPolicy: source.MediaPolicy(),
	})
	if summary != nil && state.logger != nil {
		for _, diagnostic := range summary.Diagnostics {
			state.logger.Warn("twilio sync diagnostic", "source", source.Identifier, "diagnostic", diagnostic)
		}
	}
	refreshCtx := context.WithoutCancel(ctx)
	refresh := func() error { return rebuildTwilioCacheAfterScheduledSync(refreshCtx, "twilio:"+source.Identifier) }
	if importErr != nil {
		importErr = fmt.Errorf("twilio sync %s failed: %w", source.Identifier, importErr)
	}
	return finishCallSync(importErr, summary, refresh)
}

func init() {
	syncTwilioCmd.Flags().IntVar(&syncTwilioLimit, "limit", 0,
		"max calls processed per run (0 = unlimited)")
	syncTwilioCmd.Flags().StringVar(&syncTwilioAfter, "after", "",
		"UTC creation lower bound (YYYY-MM-DD; implies --full)")
	syncTwilioCmd.Flags().BoolVar(&syncTwilioFull, "full", false,
		"recheck known calls and unavailable artifacts")
	syncTwilioCmd.Flags().BoolVar(&syncTwilioProbe, "probe", false,
		"validate capabilities and result shape without printing meeting content")
	rootCmd.AddCommand(addTwilioCmd)
	rootCmd.AddCommand(addManualSyncCacheFlags(syncTwilioCmd))
}
