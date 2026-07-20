package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/slack"
	"go.kenn.io/msgvault/internal/store"
)

var (
	syncSlackLimit       int
	syncSlackFull        bool
	syncSlackNoThreads   bool
	syncSlackNoMedia     bool
	syncSlackMaintenance bool
)

func newSyncSlackCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sync-slack [team-id]",
		Short: "Sync Slack conversations (channels, group DMs, DMs)",
		Long: `Sync Slack conversations for registered workspaces.

The first run backfills each conversation's full history; later runs are
incremental, fetching only new messages and polling recent threads for late
replies. Backfills are resumable: re-run after an interruption and the sync
continues where it stopped.

Requires a workspace added with 'add-slack'. Use --full to ignore stored
cursors and re-fetch every message; re-fetched messages are upserted in
place, so this repairs existing rows (and catches old thread replies beyond
the tracking window) without creating duplicates.

Examples:
  msgvault sync-slack
  msgvault sync-slack T0123456789
  msgvault sync-slack --limit 500
  msgvault sync-slack --full`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}

			flagTeam := ""
			if len(args) > 0 {
				flagTeam = args[0]
			}
			s, cleanup, err := openWritableStoreAndInitForIngest()
			if err != nil {
				return err
			}
			defer cleanup()
			sources, err := resolveSlackSyncSources(s, flagTeam)
			if err != nil {
				return err
			}
			ctx, stop := withInterruptCancel(cmd, "\nInterrupted. Saving checkpoint...")
			defer stop()

			var syncErrors []string
			for _, src := range sources {
				if ctx.Err() != nil {
					break
				}
				teamID, userID, ok := splitSlackIdentifier(src.Identifier)
				if !ok {
					syncErrors = append(syncErrors, src.Identifier+": malformed slack identifier")
					continue
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Syncing Slack workspace %s\n", teamID)
				token, terr := slack.LoadToken(cfg.TokensDir(), teamID, userID)
				if terr != nil {
					syncErrors = append(syncErrors, fmt.Sprintf("%s: %v", teamID, terr))
					continue
				}
				imp := slack.NewImporter(s, slack.NewClient("", token), teamID)
				opts := slackImportOptions(teamID, userID)
				opts.Limit = syncSlackLimit
				opts.Full = syncSlackFull
				opts.NoThreads = syncSlackNoThreads
				opts.Maintenance = syncSlackMaintenance
				opts.NoMedia = opts.NoMedia || syncSlackNoMedia
				opts.Progress = func(line string) { _, _ = fmt.Fprintln(cmd.OutOrStdout(), "  "+line) }
				sum, serr := imp.Import(ctx, opts)
				if ctx.Err() != nil {
					break
				}
				// One broken workspace must not block the others; collect and
				// keep syncing (multi-account convention).
				if serr != nil {
					syncErrors = append(syncErrors, fmt.Sprintf("%s: %v", teamID, serr))
					continue
				}
				printSlackSummary(cmd, teamID, sum)
			}

			// Successful workspaces' messages must reach the analytics cache
			// regardless of interruptions or per-workspace failures.
			cacheErr := rebuildCacheAfterWrite(cfg.DatabaseDSN())
			if ctx.Err() != nil {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "\nInterrupted — re-run sync-slack to resume.")
				return cacheErr
			}
			if len(syncErrors) > 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "\nErrors:")
				for _, e := range syncErrors {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", e)
				}
				return errors.Join(
					fmt.Errorf("%d workspace(s) failed to sync: %s", len(syncErrors), strings.Join(syncErrors, "; ")),
					cacheErr,
				)
			}
			return cacheErr
		},
	}
	cmd.Flags().IntVar(&syncSlackLimit, "limit", 0, "max messages per conversation this run (0 = no limit; limited runs resume on the next run and skip the maintenance rescan)")
	cmd.Flags().BoolVar(&syncSlackFull, "full", false, "ignore stored cursors and re-fetch every message (repairs/backfills existing rows in place)")
	cmd.Flags().BoolVar(&syncSlackNoThreads, "no-threads", false, "skip thread-reply fetching (backfill inline fetches and the reply sweep) for this run")
	cmd.Flags().BoolVar(&syncSlackMaintenance, "maintenance", false, "run the maintenance rescan: repair edits and reaction changes on recent messages (archives ignore post-capture mutations by default)")
	cmd.Flags().BoolVar(&syncSlackNoMedia, "no-media", false, "skip file downloads for this run (files are recorded as pending; backfill-slack-media fetches them later)")
	return cmd
}

func printSlackSummary(cmd *cobra.Command, teamID string, sum *slack.ImportSummary) {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  %s done in %s: %d conversations, %d messages",
		teamID, sum.Duration.Round(time.Second), sum.ConversationsProcessed, sum.MessagesProcessed)
	if sum.RepliesFetched > 0 {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), ", %d thread replies", sum.RepliesFetched)
	}
	if sum.AttachmentsDownloaded > 0 {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), ", %d files", sum.AttachmentsDownloaded)
	}
	if sum.AttachmentsPending > 0 {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), ", %d media pending (see backfill-slack-media)", sum.AttachmentsPending)
	}
	if sum.Errors > 0 {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), ", %d errors", sum.Errors)
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout())
}

// splitSlackIdentifier parses a slack source identifier ("<team>:<user>").
func splitSlackIdentifier(identifier string) (teamID, userID string, ok bool) {
	teamID, userID, ok = strings.Cut(identifier, ":")
	return teamID, userID, ok && teamID != "" && userID != ""
}

// resolveSlackSyncSources returns the slack sources to sync: the one for the
// given team ID, or all registered workspaces.
func resolveSlackSyncSources(s *store.Store, flagTeam string) ([]*store.Source, error) {
	sources, err := s.ListSources(sourceTypeSlack)
	if err != nil {
		return nil, fmt.Errorf("list slack sources: %w", err)
	}
	if flagTeam == "" {
		if len(sources) == 0 {
			return nil, errors.New("no Slack workspaces registered (run 'add-slack' first)")
		}
		return sources, nil
	}
	var out []*store.Source
	for _, src := range sources {
		if teamID, _, ok := splitSlackIdentifier(src.Identifier); ok && teamID == flagTeam {
			out = append(out, src)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("slack workspace %q is not registered (run 'add-slack' first)", flagTeam)
	}
	return out, nil
}

// slackImportOptions builds the config-derived import options shared by the
// CLI and scheduler paths (flag overlays are applied by the CLI caller).
func slackImportOptions(teamID, userID string) slack.ImportOptions {
	return slack.ImportOptions{
		TeamID:          teamID,
		UserID:          userID,
		AttachmentsDir:  cfg.AttachmentsDir(),
		NoMedia:         !cfg.Slack.MediaEnabled(),
		MaxMediaBytes:   cfg.Slack.MaxMediaBytes(),
		IncludeChannels: cfg.Slack.Channels,
		ExcludeChannels: cfg.Slack.ExcludeChannels,
	}
}

// runConfiguredSlackSync is the daemon scheduler entrypoint: an incremental
// sync of every registered Slack workspace. Per-workspace failures are
// collected so one broken workspace does not starve the others.
func runConfiguredSlackSync(ctx context.Context, s *store.Store) error {
	sources, err := resolveSlackSyncSources(s, "")
	if err != nil {
		return err
	}
	var errs []error
	attempted := 0
	for _, src := range sources {
		if ctx.Err() != nil {
			break
		}
		teamID, userID, ok := splitSlackIdentifier(src.Identifier)
		if !ok {
			errs = append(errs, fmt.Errorf("slack %s: malformed identifier", src.Identifier))
			continue
		}
		token, terr := slack.LoadToken(cfg.TokensDir(), teamID, userID)
		if terr != nil {
			errs = append(errs, fmt.Errorf("slack %s: %w", teamID, terr))
			continue
		}
		attempted++
		imp := slack.NewImporter(s, slack.NewClient("", token), teamID)
		if _, serr := imp.Import(ctx, slackImportOptions(teamID, userID)); serr != nil {
			errs = append(errs, fmt.Errorf("slack %s: %w", teamID, serr))
		}
	}
	// Rebuild analytics after any attempt: even a failed or canceled attempt
	// may have committed messages from healthy conversations.
	if attempted > 0 {
		if rerr := rebuildCacheAfterScheduledSync(context.WithoutCancel(ctx), "slack"); rerr != nil {
			errs = append(errs, rerr)
		}
	}
	if ctx.Err() != nil {
		errs = append(errs, ctx.Err())
	}
	return errors.Join(errs...)
}

func init() {
	rootCmd.AddCommand(newSyncSlackCmd())
}
