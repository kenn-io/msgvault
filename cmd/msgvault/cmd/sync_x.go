package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
	xapi "go.kenn.io/msgvault/internal/x"
)

func newSyncXCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use: "sync-x", Short: "Collect a registered X account's posts and mentions",
		Long: `Collect posts and public mentions for one existing X source through the selected daemon.
--after is an exclusive lower bound for a new collection window.
--archive --after previews cached from:/to: searches without network access.
Repeat with --max-spend USD to run or resume one lifetime-capped archive job.
--job-id selects a saved job; --new-job starts a fresh window. Repeating a cap
never renews its allowance. A changed --after selects a separate interval.
Historical handles are unverified; old bare mentions, quotes and nested replies
may be absent. Unavailable content cannot be retrieved. Estimates are not an
exact invoice or a recovery guarantee.
--export-removals emits the complete archive-wide removal record as JSON and its current head separately.
--remove-post locally removes one post and all known edits without credentials or budget.
--acknowledge-read-model separately clears only the reviewed local read-model hold.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			args := []string{"sync-x"}
			for _, name := range []string{"source-id", "remove-post", "export-removals", "after", "json", "build-cache", "no-build-cache", "acknowledge-read-model", "archive", "max-spend", "job-id", "new-job"} {
				flag := cmd.Flags().Lookup(name)
				if flag.Changed {
					args = append(args, "--"+name+"="+flag.Value.String())
				}
			}
			if _, err := api.ParseCLIRunXSync(api.CLIRunRequest{Args: args}); err != nil {
				return usageErr(cmd, err)
			}
			inv := invocationFromContext(cmd.Context())
			if inv == nil || inv.cfg == nil {
				return errors.New("X collection requires configuration")
			}
			if isAgentMode(inv) {
				return errors.New("X collection requires owner authority")
			}
			return runXSyncHTTP(cmd, args)
		},
	}
	cmd.Flags().String("source-id", "", "Existing X source ID (required except for --export-removals)")
	cmd.Flags().Bool("export-removals", false, "Export the complete archive removal record and separately retained current head without provider access")
	cmd.Flags().String("remove-post", "", "Remove one native post and all known edits locally, without provider access")
	cmd.Flags().String("after", "", "Exclusive lower bound (YYYY-MM-DD or RFC3339)")
	cmd.Flags().Bool("json", false, "Print exact collection counts and coverage as JSON")
	cmd.Flags().Bool("acknowledge-read-model", false, "Clear only the reviewed local read-model hold without network access")
	cmd.Flags().Bool("archive", false, "Preview or explicitly run a lifetime-capped historical search")
	cmd.Flags().String("max-spend", "", "Lifetime archive cap in USD; omit for a network-free preview, explicit zero admits no requests")
	cmd.Flags().String("job-id", "", "Select a saved archive job without changing its frozen queries or window")
	cmd.Flags().Bool("new-job", false, "Start a fresh archive job using the current verified cached handle and ceiling")
	cmd.MarkFlagsMutuallyExclusive("job-id", "new-job")
	addManualSyncCacheFlags(cmd)
	return cmd
}

// The X collection transport never sends workstation environment or cwd and
// cannot open a secondary archive or fall back to a local subprocess.
func runXSyncHTTP(cmd *cobra.Command, args []string) error {
	client, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	return client.RunCLICommand(cmd.Context(), daemonclient.CLIRunRequest{Args: args}, func(stream, data string) error {
		switch stream {
		case cliStreamStdout:
			_, err := fmt.Fprint(cmd.OutOrStdout(), data)
			return err
		case cliStreamStderr:
			_, err := fmt.Fprint(cmd.ErrOrStderr(), data)
			return err
		}
		return nil
	})
}

func validateXCollectionSource(source *store.Source) error {
	if source == nil || source.ID <= 0 || source.SourceType != sourceTypeX {
		return errors.New("source must be an existing X source")
	}
	if err := xapi.ValidateID(source.Identifier); err != nil {
		return errors.New("X source owner must be its canonical numeric user ID")
	}
	if source.MergedIntoSourceID != 0 || source.HistoryOnly {
		return store.ErrSourceRetired
	}
	return nil
}

// importXSource is shared by manual collection and the future scheduler. The
// importer owns source execution and wraps this raw credential-bound API once.
func importXSource(ctx context.Context, st *store.Store, cfg *config.Config, source *store.Source, opts xapi.ImportOptions) (*xapi.ImportSummary, error) {
	return importXSourceWithDeps(ctx, st, cfg, source, opts, defaultXCommandDeps())
}
func importXSourceWithDeps(ctx context.Context, st *store.Store, cfg *config.Config, source *store.Source, opts xapi.ImportOptions, d xCommandDeps) (*xapi.ImportSummary, error) {
	if st == nil || cfg == nil {
		return nil, errors.New("X collection configuration is unavailable")
	}
	if err := validateXCollectionSource(source); err != nil {
		return nil, err
	}
	current, err := st.GetSourceByIDContext(ctx, source.ID)
	if err != nil {
		return nil, err
	}
	if err := validateXCollectionSource(current); err != nil {
		return nil, err
	}
	if current.Identifier != source.Identifier || (opts.SourceID != 0 && opts.SourceID != current.ID) || (opts.OwnerID != "" && opts.OwnerID != current.Identifier) {
		return nil, errors.New("X collection source/owner mismatch")
	}
	policy, err := cfg.XAccount(current.Identifier)
	if err != nil {
		return nil, err
	}
	credentials := d.manager(cfg.TokensDir()).Credentials(current.Identifier, d.httpClient)
	raw := xapi.NewClient(d.baseURL(), credentials.HTTPClient())
	opts.SourceID = current.ID
	opts.OwnerID = current.Identifier
	cleaner := d.cleanup
	if cleaner == nil {
		cleaner = NewXContentCleanup(st, cfg, nil)
	}
	imp := xapi.NewImporter(st, raw, policy, credentials)
	imp.SetContentCleanup(cleaner.DuringSync)
	summary, importErr := imp.Import(ctx, opts)
	// Import has released its source execution. The owner can now drain a
	// connected component spanning other sources, without nested locks.
	if summary != nil && summary.SyncRunID != 0 {
		importErr = errors.Join(importErr, cleaner.Resume(ctx))
	}
	return summary, importErr
}

type xReadModelAcknowledgement struct {
	SourceID int64  `json:"source_id"`
	Status   string `json:"status"`
	Revision int64  `json:"revision"`
}

func acknowledgeXReadModel(ctx context.Context, st *store.Store, source *store.Source) (result xReadModelAcknowledgement, retErr error) {
	if err := validateXCollectionSource(source); err != nil {
		return result, err
	}
	execution, err := st.AcquireSyncExecutionContext(ctx, source.ID)
	if err != nil {
		return result, err
	}
	defer func() { retErr = errors.Join(retErr, execution.Release()) }()
	current, err := st.GetSourceByIDContext(ctx, source.ID)
	if err != nil {
		return result, err
	}
	if err := validateXCollectionSource(current); err != nil {
		return result, err
	}
	if current.Identifier != source.Identifier {
		return result, errors.New("X collection owner changed")
	}
	state, err := st.LoadXControlContext(ctx, source.ID)
	if err != nil {
		return result, err
	}
	if err := st.AcknowledgeXReadModelContext(ctx, source.ID, current.Identifier, state.Revision); err != nil {
		return result, err
	}
	state, err = st.LoadXControlContext(ctx, source.ID)
	if err != nil {
		return result, err
	}
	return xReadModelAcknowledgement{SourceID: source.ID, Status: "read_model_acknowledged", Revision: state.Revision}, nil
}

func (a *storeAPIAdapter) runCLISyncX(ctx context.Context, req api.CLIRunRequest, emit func(api.CLIRunEvent) error) error {
	if req.Grant != nil {
		return xRegisterError("not_permitted", errors.New("X collection requires owner authority"))
	}
	request, err := api.ParseCLIRunXSync(req)
	if err != nil {
		return xRegisterError("invalid_args", err)
	}
	if a.store == nil {
		return errors.New("X collection archive is unavailable")
	}
	if request.ExportRemovals {
		record, head, err := a.store.CaptureXRemovalRecordContext(ctx)
		if err != nil {
			return err
		}
		if emit != nil {
			if err := emit(api.CLIRunEvent{Type: cliStreamStderr, Data: "X removal record current head: " + head + "\n"}); err != nil {
				return err
			}
		}
		return emitXSyncJSON(record, emit)
	}
	source, err := a.store.GetSourceByIDContext(ctx, request.SourceID)
	if err != nil {
		return err
	}
	if request.RemovePost != "" {
		if source == nil || source.SourceType != sourceTypeX {
			return errors.New("removal requires an existing X source")
		}
		cleaner := a.xCleanup
		if cleaner == nil {
			cleaner = NewXContentCleanup(a.store, a.config, nil)
		}
		work, err := cleaner.RemovePost(ctx, source.ID, request.RemovePost, "removed")
		if err != nil {
			return err
		}
		if request.JSON {
			return emitXSyncJSON(work, emit)
		}
		return emitXSyncText(fmt.Sprintf("Removed X post %s for source %d; all known edits remain suppressed.\n", work.CanonicalID, work.SourceID), emit)
	}
	if err := validateXCollectionSource(source); err != nil {
		return err
	}
	if request.AcknowledgeReadModel {
		result, err := acknowledgeXReadModel(ctx, a.store, source)
		if err != nil {
			return err
		}
		if request.JSON {
			return emitXSyncJSON(result, emit)
		}
		return emitXSyncText(fmt.Sprintf("Acknowledged X read-model hold for source %d (revision %d).\n", result.SourceID, result.Revision), emit)
	}
	d := defaultXCommandDeps()
	if a.xDeps != nil {
		d = *a.xDeps
	}
	d.cleanup = a.xCleanup
	summary, importErr := importXSourceWithDeps(ctx, a.store, a.config, source, xapi.ImportOptions{After: request.After, Archive: request.Archive, MaxSpendMicros: request.MaxSpendMicros, JobID: request.JobID, NewJob: request.NewJob}, d)
	var outputErr, cacheErr error
	if summary != nil {
		if request.JSON {
			outputErr = emitXSyncJSON(summary, emit)
		} else {
			outputErr = emitXSyncText(xSyncSummaryText(summary), emit)
		}
		// A started run can commit message/parent repair before a later failure.
		// Keep the daemon's ordinary manual-sync cache behavior on partial work.
		if summary.SyncRunID != 0 && (summary.Archive == nil || !summary.Archive.Preview) {
			cacheErr = a.queueCacheRefreshAfterManualSync(request.BuildCache, request.NoBuildCache)
		}
	}
	return errors.Join(importErr, outputErr, cacheErr)
}
func emitXSyncJSON(value any, emit func(api.CLIRunEvent) error) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return emitXSyncText(string(data)+"\n", emit)
}
func emitXSyncText(data string, emit func(api.CLIRunEvent) error) error {
	if emit == nil {
		return nil
	}
	return emit(api.CLIRunEvent{Type: cliStreamStdout, Data: data})
}
func xSyncSummaryText(s *xapi.ImportSummary) string {
	var out strings.Builder
	mode := "collection"
	if s.Mode == "archive" || s.Archive != nil {
		mode = "archive"
	}
	fmt.Fprintf(&out, "X %s %s: source %d\n", mode, s.Status, s.SourceID)
	fmt.Fprintf(&out, "  messages_processed: %d\n  messages_added: %d\n  messages_updated: %d\n", s.MessagesProcessed, s.MessagesAdded, s.MessagesUpdated)
	if s.Archive == nil {
		fmt.Fprintf(&out, "  own_complete: %t\n  own_watermark: %s\n  mentions_complete: %t\n  mentions_watermark: %s\n  history_gap_possible: %t\n", s.OwnComplete, s.OwnWatermark, s.MentionsComplete, s.MentionsWatermark, s.HistoryGapPossible)
	} else {
		fmt.Fprintf(&out, "  Live own complete: %t\n  Live own watermark: %s\n  Live mentions complete: %t\n  Live mentions watermark: %s\n", s.OwnComplete, s.OwnWatermark, s.MentionsComplete, s.MentionsWatermark)
	}
	fmt.Fprintf(&out, "  Run estimated: USD %s\n  Run unsettled reserved: USD %s\n", xUSDMicros(s.RunEstimatedMicros), xUSDMicros(s.RunReservedMicros))
	fmt.Fprintf(&out, "  Month-to-date estimated: USD %s\n  Month-to-date unsettled reserved: USD %s\n", xUSDMicros(s.EstimatedMicros), xUSDMicros(s.ReservedMicros))
	if s.Code != "" {
		fmt.Fprintf(&out, "  code: %s\n", s.Code)
		if s.Code == "archive_scope" {
			fmt.Fprintln(&out, "Select an existing job and keep its original --after; use --new-job only for a new allowance.")
		}
	}
	if s.CredentialError != "" {
		fmt.Fprintf(&out, "  credential_error: %s\n", s.CredentialError)
	}
	if s.ModelError != "" {
		fmt.Fprintf(&out, "  model_error: %s\n", s.ModelError)
	}
	if s.Archive != nil {
		writeXArchiveSummary(&out, s.Archive, s.Code)
	}
	return out.String()
}

func writeXArchiveSummary(out *strings.Builder, a *xapi.ArchiveSummary, code string) {
	if a.JobID != "" {
		fmt.Fprintf(out, "  Job ID: %s\n", a.JobID)
	} else {
		fmt.Fprintln(out, "  Job ID: not created (preview)")
	}
	fmt.Fprintf(out, "  Verified cached handle: %s\n  Own query: %s\n  Mentions query: %s\n", a.Username, a.OwnQuery, a.MentionsQuery)
	fmt.Fprintf(out, "  After (exclusive): %s\n  Before (frozen): %s\n", a.After.UTC().Format(time.RFC3339Nano), a.Before.UTC().Format(time.RFC3339Nano))
	fmt.Fprintf(out, "  Conservative page reservation (%d results): USD %s\n", a.PageResults, xUSDMicros(a.PageReservationMicros))
	if a.CapMicros != nil {
		fmt.Fprintf(out, "  Lifetime cap: USD %s\n", xUSDMicros(*a.CapMicros))
	}
	fmt.Fprintf(out, "  Lifetime estimated spent: USD %s\n  Lifetime unsettled reserved: USD %s\n", xUSDMicros(a.SpentMicros), xUSDMicros(a.ReservedMicros))
	if a.RemainingMicros != nil {
		fmt.Fprintf(out, "  Remaining allowance: USD %s\n", xUSDMicros(*a.RemainingMicros))
	}
	fmt.Fprintf(out, "  Archive own walk complete: %t\n  Archive mentions walk complete: %t\n", a.OwnComplete, a.MentionsComplete)
	if a.TotalUnknown {
		fmt.Fprintln(out, "  Total estimate: unknown")
	} else {
		fmt.Fprintf(out, "  Total estimate: USD %s\n", xUSDMicros(a.SpentMicros))
	}
	if a.Preview {
		fmt.Fprintln(out, "Repeat with an explicit --max-spend to run or resume; preview creates no job or resource request.")
	} else if code != "" {
		if code == "budget_exhausted" {
			fmt.Fprintln(out, "Budget stop: review the monthly budget or explicitly raise this job's lifetime cap before retrying.")
		} else {
			fmt.Fprintln(out, "Resolve the reported provider, credential or coverage error before retrying; unsettled requests remain reserved.")
		}
		fmt.Fprintf(out, "Resume job %s with --job-id and an explicit --max-spend; repeating a cap never renews it.\n", a.JobID)
	}
	fmt.Fprintln(out, "Limits: historical handles are unverified; old bare mentions, quotes and nested replies may be absent.")
	fmt.Fprintln(out, "Unavailable content cannot be retrieved. Estimates are not an exact invoice or a recovery guarantee.")
}

// Integral formatting preserves the exact estimate without floating point.
func xUSDMicros(micros int64) string {
	return fmt.Sprintf("%d.%06d", micros/1_000_000, micros%1_000_000)
}
func init() { rootCmd.AddCommand(newSyncXCmd()) }
