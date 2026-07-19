package cmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/discord"
	"go.kenn.io/msgvault/internal/store"
)

type backfillDiscordMediaOptions struct {
	OnlyIncomplete bool
}

type discordMediaBackfillSummary struct {
	MessagesProcessed int64
	Downloaded        int64
	Pending           int64
	Unrecoverable     int64
}

func newBackfillDiscordMediaLocalCmd(deps discordCommandDeps) *cobra.Command {
	opts := backfillDiscordMediaOptions{}
	cmd := &cobra.Command{
		Use:   "backfill-discord-media [guild-id-or-name]",
		Short: "Retry pending Discord attachment downloads",
		Long: `Refresh Discord message attachment metadata and retry pending media.

With no argument, every registered guild is processed sequentially. Source
URLs are treated as private provenance and are never printed.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			selector := ""
			if len(args) == 1 {
				selector = args[0]
			}
			return runBackfillDiscordMedia(cmd, deps, selector, opts)
		},
	}
	cmd.Flags().BoolVar(&opts.OnlyIncomplete, "only-incomplete", false, "retry only attachments that remain incomplete")
	return cmd
}

func runBackfillDiscordMedia(
	cmd *cobra.Command,
	deps discordCommandDeps,
	selector string,
	opts backfillDiscordMediaOptions,
) error {
	st, cleanup, err := deps.openStore()
	if err != nil {
		return err
	}
	defer cleanup()
	sources, err := resolveDiscordSources(st, selector)
	if err != nil {
		return usageErr(cmd, err)
	}
	_ = opts.OnlyIncomplete // all media retries are pending-marker driven and therefore idempotent
	var anyWrites bool
	runErr := runDiscordSources(cmd.Context(), sources, func(ctx context.Context, source *store.Source) error {
		summary, backfillErr := backfillDiscordSourceMedia(ctx, st, source, deps)
		if summary.MessagesProcessed > 0 {
			anyWrites = true
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Discord media backfill: %s\n", discordSourceLabel(source))
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Messages processed: %d\n", summary.MessagesProcessed)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Downloaded: %d\n", summary.Downloaded)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Pending: %d\n", summary.Pending)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Unrecoverable: %d\n", summary.Unrecoverable)
		return backfillErr
	})
	if anyWrites {
		if cacheErr := deps.rebuildCache(deps.databaseDSN()); cacheErr != nil {
			runErr = errors.Join(runErr, cacheErr)
		}
	}
	return runErr
}

func backfillDiscordSourceMedia(
	ctx context.Context,
	st *store.Store,
	source *store.Source,
	deps discordCommandDeps,
) (discordMediaBackfillSummary, error) {
	var summary discordMediaBackfillSummary
	client, err := newDiscordClientForSource(source, deps)
	if err != nil {
		return summary, err
	}
	provider := deps.providerConfig()
	archiver, err := discord.NewMediaArchiver(st, client, deps.attachmentsDir(), provider.MaxMediaBytes)
	if err != nil {
		return summary, fmt.Errorf("configure Discord media archiver: %w", err)
	}
	pending, err := st.ListDiscordPendingAttachmentMessages(source.ID)
	if err != nil {
		return summary, fmt.Errorf("list pending Discord media: %w", err)
	}
	var errs []error
	for _, message := range pending {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		result, backfillErr := archiver.BackfillMessage(
			ctx, message.MessageID, message.ChatID, message.SourceMessageID,
		)
		summary.MessagesProcessed++
		if backfillErr != nil {
			errs = append(errs, fmt.Errorf("backfill archived message %s: %w", message.SourceMessageID, backfillErr))
			continue
		}
		for _, item := range result.Items {
			switch item.Outcome {
			case discord.MediaDownloaded:
				summary.Downloaded++
			case discord.MediaPending:
				summary.Pending++
			case discord.MediaUnrecoverable:
				summary.Unrecoverable++
			}
		}
	}
	return summary, errors.Join(errs...)
}

func init() {
	rootCmd.AddCommand(newBackfillDiscordMediaLocalCmd(defaultDiscordCommandDeps()))
}
