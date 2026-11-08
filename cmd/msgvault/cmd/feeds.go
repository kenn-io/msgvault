package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/feeds"
	"go.kenn.io/msgvault/internal/store"
)

func feedUserAgent() string {
	return "msgvault/" + Version + " (+https://msgvault.io)"
}

func newFeedSyncer(s *store.Store, state *invocation) *feeds.Syncer {
	var logger = state.logger
	return feeds.NewSyncer(s, feeds.NewFetcher(feedUserAgent()), logger)
}

func newAddFeedCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add-feed <url>... | add-feed --opml <file>",
		Short: "Follow RSS, Atom or JSON feeds",
		Long: `Follow feeds and archive their items. Each URL is fetched first: a URL that
is not a reachable RSS 2.0, Atom or JSON feed is reported and not followed.
The items it lists now are archived right away; later items arrive with
msgvault sync-feeds or the [feeds] schedule.

--opml follows every feed in an OPML file, the subscription export of most
feed readers, without fetching them; the next sync fetches them. Following a
removed feed again resumes it.`,
		Example: `  msgvault add-feed https://example.com/releases.atom
  msgvault add-feed --opml ~/Downloads/subscriptions.opml`,
		RunE: func(cmd *cobra.Command, args []string) error {
			opml, err := cmd.Flags().GetString("opml")
			if err != nil {
				return fmt.Errorf("read --opml: %w", err)
			}
			if (opml == "") == (len(args) == 0) {
				return usageErr(cmd, errors.New("pass feed URLs or --opml <file>, not both"))
			}
			if !isDaemonCLISubprocess() {
				if opml != "" {
					return runDaemonCLICommandHTTPFromCobraWithLocalFiles(cmd, args, nil)
				}
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}
			state := invocationFromCommand(cmd)
			if state == nil || state.cfg == nil {
				return errors.New("configuration is unavailable")
			}
			s, cleanup, err := openWritableStoreAndInitForIngestInvocation(cmd.Context(), state)
			if err != nil {
				return err
			}
			defer cleanup()
			if opml != "" {
				return followOPML(cmd.Context(), s, opml, cmd.OutOrStdout())
			}
			ctx, stop := withInterruptCancel(cmd, "\nInterrupted. Saving feed progress...")
			defer stop()
			summary, err := newFeedSyncer(s, state).Follow(ctx, args)
			err = errors.Join(err, runPostSourceCreateMigrationsForInvocation(ctx, s, state))
			printFeedSummary(cmd.OutOrStdout(), summary)
			cacheErr := rebuildCacheAfterManualSync(state.cfg.DatabaseDSN(), state)
			return errors.Join(err, feedFailures(summary), cacheErr)
		},
	}
	cmd.Flags().String("opml", "", "follow every feed listed in this OPML file")
	return addManualSyncCacheFlags(cmd)
}

func followOPML(ctx context.Context, s *store.Store, path string, out io.Writer) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open OPML file: %w", err)
	}
	defer func() { _ = file.Close() }()
	urls, err := feeds.ParseOPML(file)
	if err != nil {
		return err
	}
	registry := feeds.NewRegistry(s)
	var added, already int
	var invalid []error
	for _, raw := range urls {
		_, isNew, err := registry.Add(ctx, raw, "")
		switch {
		case errors.Is(err, feeds.ErrInvalidURL):
			invalid = append(invalid, fmt.Errorf("%q: %w", raw, err))
		case err != nil:
			return err
		case isNew:
			added++
		default:
			already++
		}
	}
	_, _ = fmt.Fprintf(out, "Following %d new feeds (%d already followed). Run msgvault sync-feeds to fetch them.\n", added, already)
	return errors.Join(invalid...)
}

func newListFeedsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list-feeds",
		Short: "List followed feeds and their status",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}
			state := invocationFromCommand(cmd)
			if state == nil {
				return errors.New("configuration is unavailable")
			}
			all, err := cmd.Flags().GetBool("all")
			if err != nil {
				return fmt.Errorf("read --all: %w", err)
			}
			asJSON, err := cmd.Flags().GetBool("json")
			if err != nil {
				return fmt.Errorf("read --json: %w", err)
			}
			s, cleanup, err := openWritableStoreAndInitForIngestInvocation(cmd.Context(), state)
			if err != nil {
				return err
			}
			defer cleanup()
			list, err := feeds.NewRegistry(s).List(cmd.Context())
			if err != nil {
				return err
			}
			shown := make([]feeds.Feed, 0, len(list))
			for _, feed := range list {
				if all || feed.Followed() {
					shown = append(shown, feed)
				}
			}
			if asJSON {
				return writeFeedsJSON(cmd.OutOrStdout(), shown)
			}
			return writeFeedsTable(cmd.OutOrStdout(), shown)
		},
	}
	cmd.Flags().Bool("all", false, "include removed feeds")
	cmd.Flags().Bool("json", false, "output JSON")
	return cmd
}

type feedJSON struct {
	URL       string     `json:"url"`
	Title     string     `json:"title,omitempty"`
	Followed  bool       `json:"followed"`
	AddedAt   *time.Time `json:"added_at,omitempty"`
	RemovedAt *time.Time `json:"removed_at,omitempty"`
	CheckedAt *time.Time `json:"checked_at,omitempty"`
	LastError string     `json:"last_error,omitempty"`
}

func writeFeedsJSON(out io.Writer, list []feeds.Feed) error {
	rows := make([]feedJSON, 0, len(list))
	for _, feed := range list {
		rows = append(rows, feedJSON{
			URL: feed.URL, Title: feed.State.Title, Followed: feed.Followed(),
			AddedAt: feed.State.AddedAt, RemovedAt: feed.State.RemovedAt,
			CheckedAt: feed.State.CheckedAt, LastError: feed.State.LastError,
		})
	}
	encoded, err := json.Marshal(rows, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("encode feeds: %w", err)
	}
	if _, err := fmt.Fprintln(out, string(encoded)); err != nil {
		return fmt.Errorf("write feeds: %w", err)
	}
	return nil
}

func writeFeedsTable(out io.Writer, list []feeds.Feed) error {
	if len(list) == 0 {
		if _, err := fmt.Fprintln(out, "No feeds followed. Add one with msgvault add-feed <url>."); err != nil {
			return fmt.Errorf("write feeds: %w", err)
		}
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "TITLE\tURL\tLAST CHECK\tSTATUS")
	for _, feed := range list {
		checked := "never"
		if feed.State.CheckedAt != nil {
			checked = feed.State.CheckedAt.Local().Format("2006-01-02 15:04")
		}
		status := "ok"
		switch {
		case !feed.Followed():
			status = "removed"
		case feed.State.LastError != "":
			status = "error: " + feed.State.LastError
		case feed.State.CheckedAt == nil:
			status = "waiting for sync"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", feed.State.Title, feed.URL, checked, status)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("write feeds: %w", err)
	}
	return nil
}

func newRemoveFeedCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove-feed <url>...",
		Short: "Stop following feeds; keep their items",
		Long: `Stop fetching feeds. Items already archived stay searchable; add the feed
again to resume it.`,
		Example: "  msgvault remove-feed https://example.com/releases.atom",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}
			state := invocationFromCommand(cmd)
			if state == nil {
				return errors.New("configuration is unavailable")
			}
			s, cleanup, err := openWritableStoreAndInitForIngestInvocation(cmd.Context(), state)
			if err != nil {
				return err
			}
			defer cleanup()
			registry := feeds.NewRegistry(s)
			var errs []error
			for _, raw := range args {
				feed, err := registry.Remove(cmd.Context(), raw)
				if err != nil {
					errs = append(errs, err)
					continue
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Stopped following %s; its archived items stay.\n", feed.URL)
			}
			return errors.Join(errs...)
		},
	}
}

func newSyncFeedsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sync-feeds [url]...",
		Short: "Archive new items from followed feeds",
		Long: `Fetch every followed feed, or the given ones, and archive each item the
archive does not hold yet. Items are archived once, keyed by their GUID or ID.
Unchanged feeds answer from their HTTP validators without being downloaded;
--full downloads every feed again. A feed that fails is reported and the
others still sync.`,
		Example: `  msgvault sync-feeds
  msgvault sync-feeds https://example.com/releases.atom`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}
			state := invocationFromCommand(cmd)
			if state == nil || state.cfg == nil {
				return errors.New("configuration is unavailable")
			}
			full, err := cmd.Flags().GetBool("full")
			if err != nil {
				return fmt.Errorf("read --full: %w", err)
			}
			s, cleanup, err := openWritableStoreAndInitForIngestInvocation(cmd.Context(), state)
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, stop := withInterruptCancel(cmd, "\nInterrupted. Saving feed progress...")
			defer stop()
			summary, err := newFeedSyncer(s, state).Sync(ctx, feeds.SyncOptions{URLs: args, Full: full})
			printFeedSummary(cmd.OutOrStdout(), summary)
			cacheErr := rebuildCacheAfterManualSync(state.cfg.DatabaseDSN(), state)
			return errors.Join(recordSyncAttempt(ctx, feeds.SourceType, feeds.SourceIdentifier, err), feedFailures(summary), cacheErr)
		},
	}
	cmd.Flags().Bool("full", false, "download every feed again, ignoring stored HTTP validators")
	return addManualSyncCacheFlags(cmd)
}

// runConfiguredFeedSync is the [feeds] scheduler job.
func runConfiguredFeedSync(ctx context.Context, s *store.Store) error {
	state := invocationFromContext(ctx)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	_, err := newFeedSyncer(s, state).Sync(ctx, feeds.SyncOptions{})
	err = recordSyncAttempt(ctx, feeds.SourceType, feeds.SourceIdentifier, err)
	return errors.Join(err, rebuildCacheAfterScheduledSync(context.WithoutCancel(ctx), feeds.SourceType))
}

func printFeedSummary(out io.Writer, summary feeds.Summary) {
	for _, feed := range summary.Feeds {
		name := feed.Title
		if name == "" {
			name = feed.URL
		}
		switch {
		case feed.Err != nil:
			_, _ = fmt.Fprintf(out, "%s: failed: %v\n", name, feed.Err)
		case feed.NotModified:
			_, _ = fmt.Fprintf(out, "%s: unchanged\n", name)
		default:
			_, _ = fmt.Fprintf(out, "%s: %d new of %d items", name, feed.Added, feed.Items)
			if feed.Skipped > 0 {
				_, _ = fmt.Fprintf(out, ", %d skipped", feed.Skipped)
			}
			_, _ = fmt.Fprintln(out)
		}
	}
	if len(summary.Feeds) > 1 {
		_, _ = fmt.Fprintf(out, "%d feeds, %d new items, %d failed\n", len(summary.Feeds), summary.Added(), summary.Failed())
	}
}

// feedFailures makes a command exit non-zero when a feed it was asked to
// fetch failed; the summary already named each one.
func feedFailures(summary feeds.Summary) error {
	if failed := summary.Failed(); failed > 0 {
		return fmt.Errorf("%d of %d feeds failed", failed, len(summary.Feeds))
	}
	return nil
}

func init() {
	rootCmd.AddCommand(newAddFeedCmd(), newListFeedsCmd(), newRemoveFeedCmd(), newSyncFeedsCmd())
}
