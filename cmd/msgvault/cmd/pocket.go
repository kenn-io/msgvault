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
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/pocket"
	"go.kenn.io/msgvault/internal/store"
)

func resolvePocketSources(args []string, cfg *config.Config) ([]config.PocketSource, error) {
	if cfg == nil {
		return nil, errors.New("configuration is unavailable")
	}
	if len(cfg.Pocket) == 0 {
		return nil, errors.New("no [[pocket]] sources configured; add account_email and api_key_env to config.toml")
	}
	if len(args) == 0 {
		return cfg.Pocket, nil
	}
	src := cfg.GetPocketSource(args[0])
	if src == nil {
		return nil, fmt.Errorf("no [[pocket]] entry with identifier %q", args[0])
	}
	return []config.PocketSource{*src}, nil
}

func pocketClient(src config.PocketSource) (*pocket.Client, error) {
	keyEnv := src.APIKeyEnv
	if keyEnv == "" {
		keyEnv = "POCKET_API_KEY"
	}
	key := strings.TrimSpace(os.Getenv(keyEnv))
	if key == "" {
		return nil, fmt.Errorf("pocket API key environment variable %s is not set on the daemon host", keyEnv)
	}
	return pocket.NewClient(pocket.DefaultBaseURL, pocket.DefaultMCPEndpoint, key), nil
}

func newAddPocketCmd() *cobra.Command {
	return &cobra.Command{Use: "add-pocket [identifier]", Short: "Verify and register a personal Pocket account", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		state := invocationFromCommand(cmd)
		if state == nil {
			return errors.New("configuration is unavailable")
		}
		sources, err := resolvePocketSources(args, state.cfg)
		if err != nil {
			return err
		}
		if len(sources) != 1 {
			return errors.New("multiple [[pocket]] sources configured; pass an identifier")
		}
		src := sources[0]
		email, err := src.EffectiveAccountEmail()
		if err != nil {
			return err
		}
		client, err := pocketClient(src)
		if err != nil {
			return err
		}
		account, err := client.CurrentAccount(cmd.Context())
		if err != nil {
			return err
		}
		if account.Email != email {
			return errors.New("pocket account email does not match configuration")
		}
		st, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
		if err != nil {
			return err
		}
		defer cleanup()
		if _, err = pocket.RegisterSource(st, src.Identifier, account); err != nil {
			return err
		}
		if err = runPostSourceCreateMigrationsForInvocation(st, state); err != nil {
			return fmt.Errorf("post-source-create migrations: %w", err)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Pocket source %s registered. Run msgvault sync-pocket %s.\n", src.Identifier, src.Identifier)
		return nil
	}}
}

func pocketImportOptions(cmd *cobra.Command) (pocket.ImportOptions, error) {
	var opts pocket.ImportOptions
	var err error
	opts.Limit, err = cmd.Flags().GetInt("limit")
	if err != nil {
		return opts, fmt.Errorf("read --limit: %w", err)
	}
	if opts.Limit < 0 {
		return opts, errors.New("--limit must be nonnegative")
	}
	opts.Full, err = cmd.Flags().GetBool("full")
	if err != nil {
		return opts, fmt.Errorf("read --full: %w", err)
	}
	after, err := cmd.Flags().GetString("after")
	if err != nil {
		return opts, fmt.Errorf("read --after: %w", err)
	}
	if after != "" {
		opts.StartedAfter, err = time.Parse(time.DateOnly, after)
		if err != nil {
			return opts, errors.New("invalid --after; expected YYYY-MM-DD")
		}
		opts.Full = true
	}
	return opts, nil
}

func newSyncPocketCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "sync-pocket [identifier]", Short: "Sync Pocket transcripts, summaries, and actions", Long: `Sync configured personal Pocket accounts into the meeting archive.

Each run enumerates all metadata and fetches details in least-attempted order.
Limited and failed runs preserve that order. Processing sections retain prior
content; successful explicit empty sections clear it. Source deletions do not
remove archived meetings. --full repairs projections and invalid sync state.
The daemon reads API keys from the configured environment variables.`, Args: cobra.MaximumNArgs(1)}
	cmd.Flags().Int("limit", 0, "maximum recordings fetched in this run (0 = unlimited)")
	cmd.Flags().String("after", "", "include meetings on or after this UTC date (YYYY-MM-DD; implies --full)")
	cmd.Flags().Bool("full", false, "repair projections and invalid state while preserving valid attempt order")
	addManualSyncCacheFlags(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) (err error) {
		opts, err := pocketImportOptions(cmd)
		if err != nil {
			return usageErr(cmd, err)
		}
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		state := invocationFromCommand(cmd)
		if state == nil {
			return errors.New("configuration is unavailable")
		}
		sources, err := resolvePocketSources(args, state.cfg)
		if err != nil {
			return err
		}
		st, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
		if err != nil {
			return err
		}
		defer cleanup()
		ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		total := &pocket.ImportSummary{}
		defer func() {
			err = finishPocketImport(ctx, total, err, func(context.Context) error { return rebuildCacheAfterManualSync(state.cfg.DatabaseDSN(), state) })
		}()
		for _, src := range sources {
			if err = ctx.Err(); err != nil {
				return err
			}
			opts.AccountEmail, err = src.EffectiveAccountEmail()
			if err != nil {
				return err
			}
			opts.Identifier = src.Identifier
			client, e := pocketClient(src)
			if e != nil {
				return e
			}
			sum, e := pocket.NewImporter(st, client).Import(ctx, opts)
			if sum != nil {
				total.MeetingsAdded += sum.MeetingsAdded
				total.MeetingsUpdated += sum.MeetingsUpdated
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Pocket %s: %d processed, %d added, %d updated, %d errors\n", src.Identifier, sum.MeetingsProcessed, sum.MeetingsAdded, sum.MeetingsUpdated, sum.Errors)
			}
			if e != nil {
				return e
			}
		}
		return nil
	}
	return cmd
}

func finishPocketImport(ctx context.Context, sum *pocket.ImportSummary, importErr error, refresh func(context.Context) error) error {
	err := errors.Join(importErr, ctx.Err())
	if refresh != nil && (err == nil || sum != nil && sum.MeetingsAdded+sum.MeetingsUpdated > 0) {
		err = errors.Join(err, refresh(context.WithoutCancel(ctx)))
	}
	return err
}

func runConfiguredPocketSync(ctx context.Context, st *store.Store, src config.PocketSource) error {
	if _, err := st.GetSourceByTypeAndIdentifier(pocket.SourceType, src.Identifier); err != nil {
		return fmt.Errorf("register Pocket with add-pocket first: %w", err)
	}
	email, err := src.EffectiveAccountEmail()
	if err != nil {
		return err
	}
	client, err := pocketClient(src)
	if err != nil {
		return err
	}
	sum, err := pocket.NewImporter(st, client).Import(ctx, pocket.ImportOptions{Identifier: src.Identifier, AccountEmail: email})
	return finishPocketImport(ctx, sum, err, func(refreshCtx context.Context) error {
		return rebuildCacheAfterScheduledSync(refreshCtx, "pocket:"+src.Identifier)
	})
}

func init() { rootCmd.AddCommand(newAddPocketCmd(), newSyncPocketCmd()) }
