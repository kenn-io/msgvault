package cmd

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	gotd "github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/telegram"
	telegramapi "go.kenn.io/msgvault/internal/telegram/api"
)

type telegramSyncDeps struct {
	bind         func(context.Context) telegramSyncDeps
	openStore    func() (*store.Store, func(), error)
	tokensDir    func() string
	connect      telegramConnect
	refreshCache func() error
}

func defaultTelegramSyncDeps(ctx context.Context) telegramSyncDeps {
	state := invocationFromContext(ctx)
	return telegramSyncDeps{
		bind: defaultTelegramSyncDeps,
		openStore: func() (*store.Store, func(), error) {
			if state == nil || state.cfg == nil {
				return nil, nil, errors.New("configuration is unavailable")
			}
			return openWritableStoreAndInitForIngestInvocation(state)
		},
		tokensDir: func() string {
			if state == nil || state.cfg == nil {
				return ""
			}
			return state.cfg.TokensDir()
		},
		connect: connectTelegram,
		refreshCache: func() error {
			if state == nil || state.cfg == nil {
				return errors.New("configuration is unavailable")
			}
			return rebuildCacheAfterManualSync(state.cfg.DatabaseDSN(), state)
		},
	}
}

func newSyncTelegramCmd(deps telegramSyncDeps) *cobra.Command {
	cmd := newSyncTelegramLocalCmd(deps)
	runLocal := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		return runLocal(cmd, args)
	}
	return cmd
}

func newSyncTelegramLocalCmd(deps telegramSyncDeps) *cobra.Command {
	var includeChannels bool
	var chatID int64
	var full bool
	var takeout bool
	var afterValue string
	cmd := &cobra.Command{
		Use:   "sync-telegram [account]",
		Short: "Sync registered Telegram accounts",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if invocationFromContext(cmd.Context()) != nil && deps.bind != nil {
				deps = deps.bind(cmd.Context())
			}
			if cmd.Flags().Changed("chat") && chatID == 0 {
				return usageErr(cmd, errors.New("Telegram chat ID must not be zero"))
			}
			after, err := parseDiscordAfter(afterValue)
			if err != nil {
				return usageErr(cmd, err)
			}
			selector := ""
			if len(args) != 0 {
				selector = args[0]
			}
			st, cleanup, err := deps.openStore()
			if err != nil {
				return err
			}
			defer cleanup()
			sources, err := resolveTelegramSyncSources(st, selector)
			if err != nil {
				return usageErr(cmd, err)
			}
			var runErr error
			for _, source := range sources {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Syncing Telegram account: %s\n", source.Identifier)
				if err := syncTelegramSource(cmd.Context(), st, source, deps.tokensDir(), time.Now().UTC(), includeChannels, chatID, telegramapi.HistoryOptions{Full: full || afterValue != "", After: after, Takeout: takeout}, deps.connect); err != nil {
					runErr = errors.Join(runErr, fmt.Errorf("sync Telegram account %s: %w", source.Identifier, err))
				}
			}
			return errors.Join(runErr, deps.refreshCache())
		},
	}
	cmd.Flags().BoolVar(&includeChannels, "include-channels", false, "include broadcast channels")
	cmd.Flags().Int64Var(&chatID, "chat", 0, "sync only the given marked Telegram chat ID")
	cmd.Flags().BoolVar(&full, "full", false, "re-fetch all available history")
	cmd.Flags().BoolVar(&takeout, "takeout", false, "request a takeout session for the first full backfill")
	cmd.Flags().StringVar(&afterValue, "after", "", "re-fetch history after this exclusive time (YYYY-MM-DD or RFC3339)")
	addManualSyncCacheFlags(cmd)
	return cmd
}

func init() { rootCmd.AddCommand(newSyncTelegramCmd(defaultTelegramSyncDeps(context.Background()))) }

type telegramConnect func(context.Context, *telegramapi.SessionStore, func(context.Context, *tg.Client) error) error

func connectTelegram(ctx context.Context, session *telegramapi.SessionStore, run func(context.Context, *tg.Client) error) error {
	appID, appHash := session.ApplicationCredentials()
	client := gotd.NewClient(appID, appHash, telegramapi.ReadClientOptions(session, Version))
	return client.Run(ctx, func(ctx context.Context) error { return run(ctx, client.API()) })
}

// syncTelegramSource holds the credential lifecycle lock for the whole run.
func syncTelegramSource(ctx context.Context, st *store.Store, source *store.Source, dir string, now time.Time, includeChannels bool, chatID int64, options telegramapi.HistoryOptions, connect telegramConnect) error {
	if st == nil || source == nil || source.SourceType != telegram.SourceType || source.ID <= 0 || connect == nil {
		return errors.New("invalid Telegram sync source")
	}
	value, ok := strings.CutPrefix(source.Identifier, "user")
	ownerID, err := strconv.ParseInt(value, 10, 64)
	if !ok || err != nil || ownerID <= 0 || source.Identifier != telegram.OwnerSourceIdentifier(ownerID) {
		return errors.New("invalid Telegram source identifier")
	}
	return telegramapi.WithLifecycleLockContext(ctx, dir, func() error {
		return telegram.WithLiveSyncState(ctx, st, source.ID, func(ctx context.Context, scoped *store.Store, state *telegram.SyncState, save func(context.Context, *telegram.SyncState) error) error {
			session, err := telegramapi.OpenSessionStore(dir, ownerID)
			if err != nil {
				return fmt.Errorf("open Telegram session: %w", err)
			}
			return connect(ctx, session, func(ctx context.Context, client *tg.Client) error {
				return telegramapi.RunAuthenticatedBackfillWithOptions(ctx, scoped, source.ID, client, state, now, includeChannels, chatID, options, save)
			})
		})
	})
}

// resolveTelegramSyncSources selects exact registered identifiers and never
// starts a sync for a retired source.
func resolveTelegramSyncSources(st *store.Store, selector string) ([]*store.Source, error) {
	sources, err := st.ListSources(telegram.SourceType)
	if err != nil {
		return nil, fmt.Errorf("list Telegram sources: %w", err)
	}
	if selector == "" {
		if len(sources) == 0 {
			return nil, errors.New("no Telegram accounts are registered; run 'msgvault add-telegram' first")
		}
		active := activeSyncSources(sources)
		if len(active) == 0 {
			return nil, fmt.Errorf("no active Telegram accounts are registered: %w", store.ErrSourceRetired)
		}
		return active, nil
	}
	for _, source := range sources {
		if source.Identifier == selector {
			if source.MergedIntoSourceID != 0 {
				return nil, fmt.Errorf("Telegram account %q is retired: %w", selector, store.ErrSourceRetired)
			}
			return []*store.Source{source}, nil
		}
	}
	return nil, fmt.Errorf("Telegram account %q is not registered", selector)
}
