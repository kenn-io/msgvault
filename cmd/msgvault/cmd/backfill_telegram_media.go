package cmd

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/gotd/td/tg"
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/telegram"
	telegramapi "go.kenn.io/msgvault/internal/telegram/api"
)

func newBackfillTelegramMediaCmd(deps telegramSyncDeps) *cobra.Command {
	cmd := newBackfillTelegramMediaLocalCmd(deps)
	runLocal := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		return runLocal(cmd, args)
	}
	return cmd
}

func newBackfillTelegramMediaLocalCmd(deps telegramSyncDeps) *cobra.Command {
	var maxMB int64
	var maxParticipants int
	cmd := &cobra.Command{
		Use: "backfill-telegram-media [account]", Short: "Retry pending Telegram attachment downloads", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if invocationFromContext(cmd.Context()) != nil && deps.bind != nil {
				deps = deps.bind(cmd.Context())
			}
			if maxMB <= 0 || maxMB > math.MaxInt64/(1<<20) || maxParticipants < 0 {
				return usageErr(cmd, errors.New("media size must be positive and participant limit must be nonnegative"))
			}
			selector := ""
			if len(args) > 0 {
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
			var result error
			for _, source := range sources {
				policy := attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll, MaxParticipants: maxParticipants, MaxBytes: maxMB << 20}
				if deps.mediaPolicy != nil {
					policy = deps.mediaPolicy(source.Identifier)
				}
				if cmd.Flags().Changed("max-media-mb") {
					policy.MaxBytes = maxMB << 20
				}
				if cmd.Flags().Changed("max-participants") {
					policy.MaxParticipants = maxParticipants
				}
				options := telegramapi.MediaOptions{AttachmentsDir: deps.attachmentsDir(), TempDir: deps.tokensDir(), Policy: policy}
				summary, err := backfillTelegramSourceMedia(cmd.Context(), st, source, deps.tokensDir(), options, deps.connect)
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Telegram media backfill %s: %d downloaded, %d skipped, %d pending\n", source.Identifier, summary.Stored, summary.Skipped, summary.Failed)
				if err != nil {
					result = errors.Join(result, fmt.Errorf("backfill Telegram account %s: %w", source.Identifier, err))
				}
			}
			return errors.Join(result, deps.refreshCache())
		},
	}
	cmd.Flags().Int64Var(&maxMB, "max-media-mb", attachmentpolicy.DefaultChatMaxBytes>>20, "maximum attachment size in MiB")
	cmd.Flags().IntVar(&maxParticipants, "max-participants", config.DefaultMediaMaxParticipants, "maximum conversation participants (0 disables limit)")
	addManualSyncCacheFlags(cmd)
	return cmd
}

func backfillTelegramSourceMedia(ctx context.Context, st *store.Store, source *store.Source, dir string, options telegramapi.MediaOptions, connect telegramConnect) (summary telegramapi.MediaBackfillSummary, retErr error) {
	if st == nil || source == nil || source.SourceType != telegram.SourceType || source.ID <= 0 || connect == nil {
		return summary, errors.New("invalid Telegram media backfill source")
	}
	value, ok := strings.CutPrefix(source.Identifier, "user")
	ownerID, err := strconv.ParseInt(value, 10, 64)
	if !ok || err != nil || ownerID <= 0 || source.Identifier != telegram.OwnerSourceIdentifier(ownerID) {
		return summary, errors.New("invalid Telegram source identifier")
	}
	retErr = telegramapi.WithLifecycleLockContext(ctx, dir, func() (retErr error) {
		execution, err := st.AcquireSyncExecutionContext(ctx, source.ID)
		if err != nil {
			return err
		}
		defer func() { retErr = errors.Join(retErr, execution.Release()) }()
		runID, err := execution.StartSyncContext(ctx, "backfill-telegram-media", "")
		if err != nil {
			return err
		}
		scoped := st.ScopedToSync(source.ID, runID)
		complete := false
		defer func() {
			if !complete && retErr != nil {
				retErr = errors.Join(retErr, scoped.InterruptSyncWithCheckpoint(ctx, runID, retErr, nil))
			}
		}()
		session, err := telegramapi.OpenSessionStore(dir, ownerID)
		if err != nil {
			return err
		}
		err = connect(ctx, session, func(ctx context.Context, client *tg.Client) error {
			download, ok := ctx.Value(telegramMediaTransportKey{}).(telegramMediaDownload)
			if !ok || download == nil {
				return errors.New("Telegram media transport is unavailable")
			}
			options.Download = download
			var err error
			summary, err = telegramapi.RunAuthenticatedMediaBackfill(ctx, scoped, source.ID, client, options)
			return err
		})
		if err != nil {
			return err
		}
		if err := scoped.CompleteSyncContext(ctx, runID, ""); err != nil {
			return err
		}
		complete = true
		return nil
	})
	return summary, retErr
}

func init() {
	rootCmd.AddCommand(newBackfillTelegramMediaCmd(defaultTelegramSyncDeps(context.Background())))
}
