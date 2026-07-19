package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/discord"
	"go.kenn.io/msgvault/internal/store"
)

type syncDiscordOptions struct {
	Full  bool
	After string
}

func newSyncDiscordCmd(deps discordCommandDeps) *cobra.Command {
	cmd := newSyncDiscordLocalCmd(deps)
	runLocal := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		return runLocal(cmd, args)
	}
	return cmd
}

func newSyncDiscordLocalCmd(deps discordCommandDeps) *cobra.Command {
	opts := syncDiscordOptions{}
	cmd := &cobra.Command{
		Use:   "sync-discord [guild-id-or-name]",
		Short: "Sync registered Discord guild history",
		Long: `Sync one registered Discord guild, or every registered guild when no
argument is supplied. Guilds run sequentially in stable source order, and one
guild failure does not prevent later guilds from running.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			selector := ""
			if len(args) == 1 {
				selector = args[0]
			}
			return runSyncDiscord(cmd, deps, selector, opts)
		},
	}
	cmd.Flags().BoolVar(&opts.Full, "full", false, "ignore stored cursors and re-fetch all available history")
	cmd.Flags().StringVar(&opts.After, "after", "", "exclusive lower bound (YYYY-MM-DD or RFC3339)")
	return cmd
}

func runSyncDiscord(cmd *cobra.Command, deps discordCommandDeps, selector string, opts syncDiscordOptions) error {
	after, err := parseDiscordAfter(opts.After)
	if err != nil {
		return usageErr(cmd, err)
	}
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
		label := discordSourceLabel(source)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Syncing Discord guild: %s\n", label)
		summary, importErr := importDiscordSource(
			ctx, st, source, deps, opts.Full, after, writeDiscordProgress(cmd.OutOrStdout()),
		)
		writeDiscordSyncIssues(cmd.OutOrStdout(), summary)
		// A nonzero sync run means the importer reached its durable lifecycle.
		// Core message persistence can precede later participant, media, or
		// reply failures, so MessagesProcessed is not a safe write indicator.
		if summary != nil && summary.SyncRunID != 0 {
			anyWrites = true
		}
		if importErr != nil {
			return importErr
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Discord sync complete: %s\n", label)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Containers processed: %d\n", summary.ContainersProcessed)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Messages added: %d\n", summary.MessagesAdded)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Messages updated: %d\n", summary.MessagesUpdated)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Media downloaded: %d\n", summary.MediaDownloaded)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Media pending: %d\n", summary.MediaPending)
		return nil
	})
	if anyWrites {
		if cacheErr := deps.rebuildCache(deps.databaseDSN()); cacheErr != nil {
			runErr = errors.Join(runErr, cacheErr)
		}
	}
	return runErr
}

func writeDiscordSyncIssues(out io.Writer, summary *discord.ImportSummary) {
	if summary == nil {
		return
	}
	for _, issue := range summary.CatalogIssues {
		label := "Discord catalog unavailable"
		switch issue.Scope {
		case discord.CatalogScopeGuildChannels:
			label = "Guild channel catalog unavailable"
		case discord.CatalogScopeActiveThreads:
			label = "Active thread catalog unavailable"
		case discord.CatalogScopePublicArchive:
			label = "Public archived threads unavailable"
		case discord.CatalogScopePrivateArchive:
			label = "Private archived threads unavailable"
		}
		target := issue.GuildID
		if issue.ParentID != "" {
			target = issue.ParentID
		}
		_, _ = fmt.Fprintf(out, "  Warning: %s for %s (%s)\n", label, target, discordIssueStatus(issue.StatusCode, issue.DiscordCode, string(issue.Kind)))
	}
	for _, issue := range summary.ContainerIssues {
		label := "Container unavailable"
		switch issue.Kind {
		case discord.ContainerIssueForbidden:
			label = "Container inaccessible"
		case discord.ContainerIssueUnknownChannel:
			label = "Container missing upstream"
		}
		_, _ = fmt.Fprintf(out, "  Warning: %s: %s (%s)\n", label, issue.ContainerID, discordIssueStatus(issue.StatusCode, issue.DiscordCode, string(issue.Kind)))
	}
}

func discordIssueStatus(statusCode, discordCode int, fallback string) string {
	parts := make([]string, 0, 2)
	if statusCode != 0 {
		parts = append(parts, fmt.Sprintf("HTTP %d", statusCode))
	}
	if discordCode != 0 {
		parts = append(parts, fmt.Sprintf("Discord code %d", discordCode))
	}
	if len(parts) == 0 {
		return fallback
	}
	return strings.Join(parts, ", ")
}

// importDiscordSource is the shared manual/scheduler production path. Task 11's
// daemon dispatcher calls this helper after applying its operation gate.
func importDiscordSource(
	ctx context.Context,
	st *store.Store,
	source *store.Source,
	deps discordCommandDeps,
	full bool,
	after time.Time,
	progress func(string),
) (*discord.ImportSummary, error) {
	importer, err := newDiscordImporterForSource(st, source, deps)
	if err != nil {
		return nil, err
	}
	return importer.Import(ctx, discordImportOptions(source, deps, full, after, progress))
}

func init() {
	rootCmd.AddCommand(newSyncDiscordCmd(defaultDiscordCommandDeps()))
}
