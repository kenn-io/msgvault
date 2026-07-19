package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"

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
	Warnings          map[discordMediaWarningKind]int64
}

type discordMediaWarningKind string

const (
	discordMediaWarningTooLarge      discordMediaWarningKind = "too_large"
	discordMediaWarningInvalidURL    discordMediaWarningKind = "invalid_url"
	discordMediaWarningDownload      discordMediaWarningKind = "download"
	discordMediaWarningRedirect      discordMediaWarningKind = "redirect"
	discordMediaWarningStorage       discordMediaWarningKind = "storage"
	discordMediaWarningRefresh       discordMediaWarningKind = "refresh"
	discordMediaWarningUnrecoverable discordMediaWarningKind = "unrecoverable"
	discordMediaWarningCanceled      discordMediaWarningKind = "canceled"
	discordMediaWarningOther         discordMediaWarningKind = "other"
)

func newBackfillDiscordMediaLocalCmd(deps discordCommandDeps) *cobra.Command {
	opts := backfillDiscordMediaOptions{}
	cmd := &cobra.Command{
		Use:   "backfill-discord-media [guild-id-or-name]",
		Short: "Retry pending Discord attachment downloads",
		Long: `Refresh Discord message attachment metadata and retry pending media.

By default, every archived message with Discord attachments is scanned; already
complete messages are counted but require no download work. --only-incomplete
limits selection to messages that still have pending attachment rows.

With no guild argument, every registered guild is processed sequentially.
Source URLs are treated as private provenance and are never printed.`,
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
	var anyWrites bool
	runErr := runDiscordSources(cmd.Context(), sources, func(ctx context.Context, source *store.Source) error {
		summary, backfillErr := backfillDiscordSourceMedia(ctx, st, source, deps, opts.OnlyIncomplete)
		if summary.MessagesProcessed > 0 {
			anyWrites = true
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Discord media backfill: %s\n", discordSourceLabel(source))
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Messages processed: %d\n", summary.MessagesProcessed)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Downloaded: %d\n", summary.Downloaded)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Pending: %d\n", summary.Pending)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Unrecoverable: %d\n", summary.Unrecoverable)
		writeDiscordMediaWarnings(cmd.OutOrStdout(), summary.Warnings)
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
	onlyIncomplete bool,
) (discordMediaBackfillSummary, error) {
	summary := discordMediaBackfillSummary{Warnings: make(map[discordMediaWarningKind]int64)}
	client, err := newDiscordClientForSource(source, deps)
	if err != nil {
		return summary, err
	}
	provider := deps.providerConfig()
	archiver, err := discord.NewMediaArchiver(st, client, deps.attachmentsDir(), provider.MaxMediaBytes)
	if err != nil {
		return summary, fmt.Errorf("configure Discord media archiver: %w", err)
	}
	var messages []store.DiscordAttachmentMessage
	if onlyIncomplete {
		messages, err = st.ListDiscordPendingAttachmentMessages(source.ID)
	} else {
		messages, err = st.ListDiscordAttachmentMessages(source.ID)
	}
	if err != nil {
		return summary, fmt.Errorf("list Discord media messages: %w", err)
	}
	var errs []error
	for _, message := range messages {
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
			if item.Err != nil {
				summary.Warnings[classifyDiscordMediaWarning(item.Err)]++
			}
		}
	}
	return summary, errors.Join(errs...)
}

func classifyDiscordMediaWarning(err error) discordMediaWarningKind {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return discordMediaWarningCanceled
	case errors.Is(err, discord.ErrMediaTooLarge):
		return discordMediaWarningTooLarge
	case errors.Is(err, discord.ErrInvalidMediaURL):
		return discordMediaWarningInvalidURL
	case errors.Is(err, discord.ErrMediaRedirect):
		return discordMediaWarningRedirect
	case errors.Is(err, discord.ErrMediaDownload):
		return discordMediaWarningDownload
	case errors.Is(err, discord.ErrMediaStorage):
		return discordMediaWarningStorage
	case errors.Is(err, discord.ErrMediaRefresh):
		return discordMediaWarningRefresh
	case errors.Is(err, discord.ErrMediaUnrecoverable):
		return discordMediaWarningUnrecoverable
	default:
		return discordMediaWarningOther
	}
}

func writeDiscordMediaWarnings(out io.Writer, warnings map[discordMediaWarningKind]int64) {
	total := int64(0)
	for _, count := range warnings {
		total += count
	}
	if total == 0 {
		return
	}
	_, _ = fmt.Fprintf(out, "  Attachment warnings: %d\n", total)
	for _, item := range []struct {
		kind  discordMediaWarningKind
		label string
	}{
		{discordMediaWarningTooLarge, "Size cap exceeded"},
		{discordMediaWarningInvalidURL, "Invalid source URL"},
		{discordMediaWarningRedirect, "Redirect refused"},
		{discordMediaWarningDownload, "Download failed"},
		{discordMediaWarningStorage, "Storage failed"},
		{discordMediaWarningRefresh, "Refresh unavailable"},
		{discordMediaWarningUnrecoverable, "Attachment unrecoverable"},
		{discordMediaWarningCanceled, "Canceled"},
		{discordMediaWarningOther, "Other attachment error"},
	} {
		if count := warnings[item.kind]; count > 0 {
			_, _ = fmt.Fprintf(out, "    %s: %d\n", item.label, count)
		}
	}
}

func init() {
	rootCmd.AddCommand(newBackfillDiscordMediaLocalCmd(defaultDiscordCommandDeps()))
}
