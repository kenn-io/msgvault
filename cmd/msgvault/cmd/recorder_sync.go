package cmd

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/store"
)

// syncRecorderSources preserves cumulative cache refresh when a later source fails.
func syncRecorderSources[S recorderCommandSource, R any](cmd *cobra.Command, args []string, configured meetingSources[S], name, kind, afterText string, identifier func(S) string, importSource func(context.Context, *store.Store, S, time.Time) (*R, error), writes func(*R) int64, summarize func(io.Writer, *R), refresh func(string, *invocation) error) error {
	state := invocationFromCommand(cmd)
	cfg := state.cfg
	sources, err := configured.selected(args)
	if err != nil {
		return err
	}
	var after time.Time
	if afterText != "" {
		parsed, err := time.Parse(time.DateOnly, afterText)
		if err != nil {
			return usageErr(cmd, fmt.Errorf("invalid --after %q (expected YYYY-MM-DD): %w", afterText, err))
		}
		after = parsed.UTC()
	}
	for _, source := range sources {
		if _, err := source.EffectiveAccountEmail(); err != nil {
			return err
		}
	}

	st, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
	if err != nil {
		return err
	}
	defer cleanup()
	dbPath := cfg.DatabaseDSN()

	ctx, stop := withInterruptCancel(cmd, "\nInterrupted. Finishing current meeting...")
	defer stop()

	var pendingWrites int64
	for _, source := range sources {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Syncing %s for %s\n\n", name, identifier(source))
		summary, importErr := importSource(ctx, st, source, after)
		if summary != nil {
			pendingWrites += writes(summary)
		}
		run := meetingSyncRun{
			provider: kind, identifier: identifier(source),
			writes: pendingWrites, err: importErr,
		}
		refreshOnFailure := func() error { return refresh(dbPath, state) }
		if err := run.finish(refreshOnFailure); err != nil {
			return err
		}
		summarize(cmd.OutOrStdout(), summary)
	}
	if pendingWrites > 0 {
		return refresh(dbPath, state)
	}
	return nil
}

// syncConfiguredRecorder keeps registration, attempt reporting and scheduled cache retries together.
func syncConfiguredRecorder[S recorderCommandSource, R any](ctx context.Context, st *store.Store, source S, kind, identifier string, importSource func() (*R, error), writes func(*R) int64, refresh func(context.Context, string) error) error {
	missing := fmt.Errorf("%s source %q is not registered; run msgvault add-%s %s first", kind, identifier, kind, identifier)
	registered, err := requireRegisteredMeetingSource(st, kind, identifier, missing)
	if err != nil {
		return recordSyncAttempt(ctx, kind, identifier, err)
	}
	if registered.MergedIntoSourceID != 0 {
		return nil
	}
	if _, err := source.EffectiveAccountEmail(); err != nil {
		return recordSyncAttempt(ctx, kind, identifier, err)
	}
	summary, importErr := importSource()
	var count int64
	if summary != nil {
		count = writes(summary)
	}
	run := meetingSyncRun{provider: kind, identifier: identifier, writes: count, err: importErr}
	_ = recordSyncAttempt(ctx, kind, identifier, importErr)
	// Retry the cache even when an earlier write was throttled and this scan has no changes.
	return run.finishScheduled(ctx, kind+":"+identifier, refresh)
}

func writeRecorderSummary(out io.Writer, name string, duration time.Duration, processed, added, updated, stored, deferred, inProgress int64) {
	_, _ = fmt.Fprintf(out, "\n%s sync complete!\n", name)
	_, _ = fmt.Fprintf(out, "  Duration:           %s\n", duration.Round(time.Second))
	_, _ = fmt.Fprintf(out, "  Meetings processed: %d\n", processed)
	_, _ = fmt.Fprintf(out, "  Meetings added:     %d\n", added)
	_, _ = fmt.Fprintf(out, "  Meetings updated:   %d\n", updated)
	if stored > 0 || deferred > 0 {
		_, _ = fmt.Fprintf(out, "  Recordings stored:  %d (deferred: %d)\n", stored, deferred)
	}
	if inProgress > 0 {
		_, _ = fmt.Fprintf(out, "  Still in progress:  %d (archived by a later sync)\n", inProgress)
	}
}
