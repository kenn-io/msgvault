package cmd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/threads"
)

func newSyncThreadsCmd(deps threadsCommandDeps) *cobra.Command {
	cmd := newSyncThreadsLocalCmd(deps)
	local := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		return local(cmd, args)
	}
	return cmd
}
func newSyncThreadsLocalCmd(deps threadsCommandDeps) *cobra.Command {
	var after, post string
	var full bool
	cmd := &cobra.Command{Use: "sync-threads <identifier>", Short: "Archive available Threads posts and replies", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if invocationFromCommand(cmd) != nil && deps.bind != nil {
			deps = deps.bind(cmd.Context())
		}
		if _, _, err := threads.ParseSourceIdentifier(args[0]); err != nil {
			return usageErr(cmd, err)
		}
		request := threads.SyncRequest{Full: full, PostID: post}
		if after != "" {
			parsed, err := time.Parse(time.RFC3339Nano, after)
			if err != nil {
				return usageErr(cmd, errors.New("Threads --after must be RFC3339"))
			}
			request.After = parsed
		}
		if err := request.Validate(); err != nil {
			return usageErr(cmd, err)
		}
		st, cleanup, err := deps.openStore()
		if err != nil {
			return err
		}
		defer cleanup()
		source, err := st.GetSourceByTypeAndIdentifier(sourceTypeThreads, args[0])
		if err != nil {
			return err
		}
		summary, runErr := importThreadsSource(cmd.Context(), st, source, deps, request, threads.ManualRequestLimit, nil)
		// A page may have committed physical writes before a later error, even if
		// the safe checkpoint still has zero counters. Ownership is released first.
		if summary != nil && summary.SyncRunID != 0 && deps.rebuildCache != nil {
			runErr = errors.Join(runErr, deps.rebuildCache(deps.databaseDSN()))
		}
		if summary != nil {
			asJSON, _ := cmd.Flags().GetBool("json")
			if asJSON {
				runErr = errors.Join(runErr, printJSONTo(cmd.OutOrStdout(), summary))
			} else {
				oldest := "none"
				if !summary.OldestObserved.IsZero() {
					oldest = summary.OldestObserved.UTC().Format(time.RFC3339Nano)
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Threads sync %s\n  history_complete: %t\n  requested_floor: %s\n  oldest_observed: %s\n  sweep: %d/%d\n  pending_walks: %d (at least %d pages)\n  unavailable: %d; incomplete: %d\n  messages_added: %d; messages_updated: %d\n", summary.Status, summary.HistoryComplete, summary.RequestedFloor.Format(time.RFC3339Nano), oldest, summary.SweepPosition, summary.SweepBoundary, summary.PendingWalks, summary.PendingPages, summary.Unavailable, summary.Incomplete, summary.MessagesAdded, summary.MessagesUpdated)
			}
		}
		return runErr
	}}
	cmd.Flags().StringVar(&after, "after", "", "Inclusive history floor (RFC3339)")
	cmd.Flags().StringVar(&post, "post", "", "Archived own root media ID")
	cmd.Flags().BoolVar(&full, "full", false, "Start or resume a full read epoch")
	cmd.Flags().Bool("json", false, "Output the import summary as JSON")
	addManualSyncCacheFlags(cmd)
	return cmd
}

// importThreadsSource is shared by manual, scheduled and Sync now dispatch.
// Credential refresh and every page share one budget and one lifecycle lease.
func importThreadsSource(ctx context.Context, st *store.Store, source *store.Source, deps threadsCommandDeps, request threads.SyncRequest, limit int, progress func(string)) (summary *threads.ImportSummary, retErr error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	if source == nil || source.SourceType != sourceTypeThreads {
		return nil, errors.New("Threads source is required")
	}
	if _, _, err := threads.ParseSourceIdentifier(source.Identifier); err != nil {
		return nil, err
	}
	budget := threads.NewRequestBudget(threads.BudgetOptions{MaxAttempts: limit, Now: deps.now, Wait: deps.wait})
	ctx, cancel := budget.RunContext(ctx)
	defer cancel()
	manager := deps.tokenManager()
	retErr = manager.WithSourceLock(ctx, source.Identifier, func() (runErr error) {
		current, err := st.GetSourceByIDContext(ctx, source.ID)
		if err != nil {
			return err
		}
		if current.SourceType != sourceTypeThreads || current.Identifier != source.Identifier || current.HistoryOnly || current.MergedIntoSourceID != 0 {
			return store.ErrSourceRetired
		}
		execution, err := st.AcquireSyncExecutionContext(ctx, source.ID)
		if err != nil {
			return err
		}
		defer func() { runErr = errors.Join(runErr, execution.Release()) }()
		if deps.limiter != nil {
			budget.BindLimiter(deps.limiter(source.Identifier))
		}
		record, err := manager.EnsureCurrent(ctx, source.Identifier, deps.oauthOptions(budget))
		if err != nil {
			if _, paused := errors.AsType[*threads.PauseError](err); paused {
				summary = &threads.ImportSummary{Status: store.SyncStatusPaused}
				return nil
			}
			return err
		}
		api, err := deps.newAPI(record, budget)
		if err != nil {
			return err
		}
		summary, err = threads.NewImporter(st, api).Import(ctx, threads.ImportOptions{SourceID: source.ID, SourceIdentifier: source.Identifier, Request: request, Budget: budget, Execution: execution, Now: deps.now, Progress: progress})
		if apiErr, ok := errors.AsType[*threads.APIError](err); ok && apiErr.CredentialInvalid() {
			err = errors.Join(err, manager.Invalidate(record))
		}
		return err
	})
	return summary, retErr
}
func init() { rootCmd.AddCommand(newSyncThreadsCmd(defaultThreadsCommandDeps())) }
