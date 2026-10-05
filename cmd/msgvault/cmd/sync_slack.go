package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/slack"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

var (
	syncSlackLimit       int
	syncSlackFull        bool
	syncSlackNoThreads   bool
	syncSlackNoMedia     bool
	syncSlackMaintenance bool
)

func newSyncSlackCmd() *cobra.Command {
	var syncPrivateChannels, syncDMs, syncGroupDMs bool
	cmd := &cobra.Command{
		Use:   "sync-slack [team-id]",
		Short: "Sync Slack conversations (channels, group DMs, DMs)",
		Long: `Sync Slack conversations for registered workspaces.

The first run backfills each conversation's full history; later runs are
incremental. Tokens with search:read use search plus periodic history audits
to discover late thread replies. Without it, each sync revisits conversation
history for replies on old threads. Backfills and audits are resumable:
re-run after an interruption and the sync continues where it stopped.

Requires a workspace added with 'add-slack'. Use --full to start a repair
session: every message is re-fetched and upserted in place, so existing rows
are repaired without duplicates. A repair session survives interruptions and
--limit scoping — subsequent runs of any kind continue it until complete.

Examples:
  msgvault sync-slack
  msgvault sync-slack T0123456789
  msgvault sync-slack --limit 500
  msgvault sync-slack --full`,
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

			flagTeam := ""
			if len(args) > 0 {
				flagTeam = args[0]
			}
			s, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
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
				opts := slackImportOptions(teamID, userID, cfg)
				applySlackConversationOverrides(cmd, &opts, syncPrivateChannels, syncDMs, syncGroupDMs)
				opts.Limit = syncSlackLimit
				opts.Full = syncSlackFull
				opts.NoThreads = syncSlackNoThreads
				opts.Maintenance = syncSlackMaintenance
				opts.NoMedia = opts.NoMedia || syncSlackNoMedia
				opts.Progress = func(line string) { writeSlackProgress(cmd.OutOrStdout(), line) }
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
			cacheErr := rebuildCacheAfterManualSync(cfg.DatabaseDSN(), state)
			if ctx.Err() != nil {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "\nInterrupted — re-run sync-slack to resume.")
			} else if len(syncErrors) > 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "\nErrors:")
				for _, e := range syncErrors {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", e)
				}
			}
			return slackSyncExit(ctx.Err(), syncErrors, cacheErr)
		},
	}
	cmd.Flags().IntVar(&syncSlackLimit, "limit", 0, "max messages of work per conversation this run, thread replies and a workspace-wide sweep budget included (0 = no limit; every phase resumes next run so standing limited schedules converge; only the maintenance rescan is skipped)")
	cmd.Flags().BoolVar(&syncSlackFull, "full", false, "start (or continue) a repair session: re-fetch every message, upserting in place; interrupted or --limit-scoped repairs resume across later runs until complete")
	cmd.Flags().BoolVar(&syncSlackNoThreads, "no-threads", false, "skip thread-reply fetching (backfill inline fetches and the reply sweep) for this run")
	cmd.Flags().BoolVar(&syncSlackMaintenance, "maintenance", false, "run the maintenance rescan: repair edits and reaction changes on recent messages (archives ignore post-capture mutations by default)")
	cmd.Flags().BoolVar(&syncSlackNoMedia, "no-media", false, "skip file downloads for this run (files are recorded as pending; backfill-slack-media fetches them later)")
	cmd.Flags().BoolVar(&syncDMs, "dms", true, "include one-to-one DMs for this run, overriding config (true or false)")
	cmd.Flags().BoolVar(&syncPrivateChannels, "private-channels", true, "include private channels for this run, overriding config (true or false)")
	cmd.Flags().BoolVar(&syncGroupDMs, "group-dms", true, "include group DMs for this run, overriding config (true or false)")
	return cmd
}

func applySlackConversationOverrides(cmd *cobra.Command, opts *slack.ImportOptions, privateChannels, dms, groupDMs bool) {
	if cmd.Flags().Changed("private-channels") {
		opts.ExcludePrivateChannels = !privateChannels
	}
	if cmd.Flags().Changed("dms") {
		opts.ExcludeDMs = !dms
	}
	if cmd.Flags().Changed("group-dms") {
		opts.ExcludeGroupDMs = !groupDMs
	}
}

func writeSlackProgress(out io.Writer, line string) {
	_, _ = fmt.Fprintln(out, "  "+textutil.SanitizeTerminal(line))
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
	if sum.AttachmentsSkipped > 0 {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), ", %d media skipped by policy", sum.AttachmentsSkipped)
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

// slackSyncExit resolves sync-slack's exit error from the run's parts. An
// interrupted run must NEVER exit clean — schedulers and scripts read exit 0
// as "sync complete" — even when the cache rebuild succeeded; the sync it
// summarizes did not.
func slackSyncExit(ctxErr error, syncErrors []string, cacheErr error) error {
	if ctxErr != nil {
		return errors.Join(fmt.Errorf("interrupted: %w", ctxErr), cacheErr)
	}
	if len(syncErrors) > 0 {
		return errors.Join(
			fmt.Errorf("%d workspace(s) failed to sync: %s", len(syncErrors), strings.Join(syncErrors, "; ")),
			cacheErr,
		)
	}
	return cacheErr
}

// slackImportOptions builds the config-derived import options shared by the
// CLI and scheduler paths (flag overlays are applied by the CLI caller).
func slackImportOptions(teamID, userID string, cfg *config.Config) slack.ImportOptions {
	policy := cfg.Slack.MediaPolicy(teamID)
	return slack.ImportOptions{
		TeamID:                 teamID,
		UserID:                 userID,
		AttachmentsDir:         cfg.AttachmentsDir(),
		MaxMediaBytes:          policy.MaxBytes,
		MediaPolicy:            policy,
		IncludeChannels:        cfg.Slack.Channels,
		ExcludeChannels:        cfg.Slack.ExcludeChannels,
		ExcludePrivateChannels: !cfg.Slack.PrivateChannelsEnabled(),
		ExcludeDMs:             !cfg.Slack.DMsEnabled(),
		ExcludeGroupDMs:        !cfg.Slack.GroupDMsEnabled(),
	}
}

// runConfiguredSlackSync is the daemon scheduler entrypoint: an incremental
// sync of every registered Slack workspace. Per-workspace failures are
// collected so one broken workspace does not starve the others.
func runConfiguredSlackSync(ctx context.Context, s *store.Store) error {
	state := invocationFromContext(ctx)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	cfg := state.cfg
	sources, err := resolveSlackSyncSources(s, "")
	if err != nil {
		return err
	}
	return runScheduledSlackAttempts(ctx, sources, scheduledSlackRotation,
		func(src *store.Source) (bool, error) {
			teamID, userID, ok := splitSlackIdentifier(src.Identifier)
			if !ok {
				return false, fmt.Errorf("slack %s: malformed identifier", src.Identifier)
			}
			token, terr := slack.LoadToken(cfg.TokensDir(), teamID, userID)
			if terr != nil {
				return false, fmt.Errorf("slack %s: %w", teamID, terr)
			}
			imp := slack.NewImporter(s, slack.NewClient("", token), teamID)
			if _, serr := imp.Import(ctx, slackImportOptions(teamID, userID, cfg)); serr != nil {
				return true, fmt.Errorf("slack %s: %w", teamID, serr)
			}
			return true, nil
		}, func() error {
			// Rebuild analytics after any import attempt: even a failed or
			// canceled attempt may have committed messages from healthy channels.
			return rebuildCacheAfterScheduledSync(context.WithoutCancel(ctx), "slack")
		})
}

// scheduledSlackRotation remembers where a preempted scheduled sync stopped,
// so one long-running workspace cannot starve workspaces later in store order.
var scheduledSlackRotation = &slackWorkspaceRotation{}

type slackWorkspaceRotation struct {
	mu   sync.Mutex
	next string
}

func (r *slackWorkspaceRotation) order(sources []*store.Source) []*store.Source {
	r.mu.Lock()
	next := r.next
	r.mu.Unlock()
	start := -1
	for idx, src := range sources {
		if src.Identifier == next {
			start = idx
			break
		}
	}
	if start <= 0 {
		return slices.Clone(sources)
	}
	return append(slices.Clone(sources[start:]), sources[:start]...)
}

func (r *slackWorkspaceRotation) resumeAt(identifier string) {
	r.mu.Lock()
	r.next = identifier
	r.mu.Unlock()
}

// runScheduledSlackAttempts isolates workspace failures, rebuilds analytics
// after import attempts, and resumes after the workspace interrupted by a
// scheduler yield. A cooperative preemption request also ends the current run
// after the current workspace has had a chance to checkpoint.
func runScheduledSlackAttempts(
	ctx context.Context,
	sources []*store.Source,
	rotation *slackWorkspaceRotation,
	attempt func(*store.Source) (bool, error),
	rebuild func() error,
) error {
	var errs []error
	attempted := false
	resumeAt := ""
	ordered := rotation.order(sources)
	for idx, src := range ordered {
		if ctx.Err() != nil || jobctx.PreemptionRequested(ctx) {
			resumeAt = src.Identifier
			break
		}
		started, err := attempt(src)
		attempted = attempted || started
		if err != nil {
			errs = append(errs, err)
		}
		if ctx.Err() != nil || jobctx.PreemptionRequested(ctx) {
			resumeAt = ordered[(idx+1)%len(ordered)].Identifier
			break
		}
	}
	rotation.resumeAt(resumeAt)
	if attempted {
		if err := rebuild(); err != nil {
			errs = append(errs, err)
		}
	}
	if ctx.Err() != nil {
		errs = append(errs, ctx.Err())
	}
	return errors.Join(errs...)
}

func init() {
	rootCmd.AddCommand(addManualSyncCacheFlags(newSyncSlackCmd()))
}
