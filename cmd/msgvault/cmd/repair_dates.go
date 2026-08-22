package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

const dateRepairSampleLimit = 10

var repairDatesApply bool

var repairDatesCmd = &cobra.Command{
	Use:   "repair-dates",
	Short: "Repair missing or implausible email sent dates",
	Long: `Scan email messages whose stored sent_at is missing, before 1990, or
more than 30 days in the future. For each candidate, resolve a canonical sent
time from the Date header, the oldest plausible Received timestamp, or stored
source metadata.

The default is a read-only report. Pass --apply to update the archive, write an
audit ledger under the data directory, and rebuild the analytics cache for
SQLite archives. Original source files and remote servers are never modified.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		return runRepairDatesLocal(cmd, repairDatesApply, time.Now().UTC())
	},
}

type plannedDateRepair struct {
	store.MessageDateRepair

	source    mime.DateSource
	oldSentAt sql.NullTime
}

// unresolvedDateReason categorizes why a candidate could not be resolved to
// a plausible sent time, so operators can see what signal is missing instead
// of just a bare count.
type unresolvedDateReason string

const (
	unresolvedDateHeaderUnusable unresolvedDateReason = "date-header-unusable"
	unresolvedReceivedUnusable   unresolvedDateReason = "received-headers-unusable"
	unresolvedNoDateSignal       unresolvedDateReason = "no-date-signal"
)

type unresolvedDateCandidate struct {
	id     int64
	reason unresolvedDateReason
}

type dateRepairPlan struct {
	candidateCount  int
	repairs         []plannedDateRepair
	rawMIMEFailures int
	unresolved      []unresolvedDateCandidate
}

func runRepairDatesLocal(
	cmd *cobra.Command,
	apply bool,
	now time.Time,
) (runErr error) {
	ctx := cmd.Context()
	st, cleanup, err := openWritableStoreAndInit()
	if err != nil {
		return err
	}
	defer cleanup()

	plan, err := scanAndPlanDateRepairs(ctx, st, now)
	if err != nil {
		return err
	}
	printDateRepairPlan(cmd, plan)

	if !apply {
		if _, err := fmt.Fprintln(
			cmd.OutOrStdout(),
			"\nDry run: no rows were modified. Re-run with --apply to write repairs.",
		); err != nil {
			return fmt.Errorf("write date repair dry-run summary: %w", err)
		}
		return nil
	}
	if len(plan.repairs) == 0 {
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), "Nothing to repair."); err != nil {
			return fmt.Errorf("write empty date repair summary: %w", err)
		}
		return nil
	}

	lowerBound, upperBound := mime.PlausibleDateBounds(now)
	ledgerPath, err := availableDateRepairLedgerPath(cfg.Data.DataDir, now)
	if err != nil {
		return err
	}
	ledger := newDateRepairLedger(plan, now)
	if err := writeDateRepairLedger(ledgerPath, ledger); err != nil {
		return fmt.Errorf("write date repair ledger: %w", err)
	}

	usesAnalyticsCache := dateRepairUsesAnalyticsCache(cfg.DatabaseDSN())
	unlockCache := func() error { return nil }
	if usesAnalyticsCache {
		releaseCacheLocks, err := lockCacheAndInvalidateSyncState(cfg.AnalyticsDir())
		if err != nil {
			return fmt.Errorf("protect analytics cache for date repair: %w", err)
		}
		released := false
		unlockCache = func() error {
			if released {
				return nil
			}
			released = true
			return wrapError(releaseCacheLocks(), "release analytics cache lock")
		}
		defer func() {
			runErr = errors.Join(runErr, unlockCache())
		}()
	}

	if err := applyPlannedDateRepairs(
		ctx,
		st,
		plan,
		lowerBound,
		upperBound,
		ledgerPath,
		ledger,
	); err != nil {
		return err
	}

	appliedAt := time.Now().UTC()
	ledger.AppliedAt = &appliedAt
	if err := st.MarkAllContactStateDirtyContext(ctx); err != nil {
		ledger.Status = "applied-invalidation-failed"
		ledger.Error = err.Error()
		if ledgerErr := writeDateRepairLedger(ledgerPath, ledger); ledgerErr != nil {
			err = errors.Join(
				err,
				fmt.Errorf("record contact-state invalidation failure in audit ledger: %w",
					ledgerErr),
			)
		}
		return dateRepairContactInvalidationError(err)
	}

	ledger.Status = "applied"
	if err := writeDateRepairLedger(ledgerPath, ledger); err != nil {
		return dateRepairPostApplyRecoveryError(
			"audit ledger finalization failed",
			err,
		)
	}

	cacheRebuilt := false
	if usesAnalyticsCache {
		if _, err := buildCacheLocked(
			cfg.DatabaseDSN(),
			cfg.AnalyticsDir(),
			true,
			false,
			publishLockHeld,
			analyticsBuilderOverrides(cfg.Analytics),
		); err != nil {
			ledger.Status = "applied-cache-failed"
			ledger.Error = err.Error()
			_ = writeDateRepairLedger(ledgerPath, ledger)
			return dateRepairCacheRefreshError(err)
		}
		cacheRebuilt = true
	}

	ledger.Status = "complete"
	ledger.CacheRebuilt = cacheRebuilt
	completedAt := time.Now().UTC()
	ledger.CompletedAt = &completedAt
	if err := writeDateRepairLedger(ledgerPath, ledger); err != nil {
		return fmt.Errorf(
			"date repairs and cache refresh completed, but audit ledger finalization failed: %w",
			err,
		)
	}

	cacheSummary := "Analytics cache not used for PostgreSQL."
	if cacheRebuilt {
		cacheSummary = "Analytics cache rebuilt."
	}
	if _, err = fmt.Fprintf(
		cmd.OutOrStdout(),
		"Repaired %d message(s).\nRepair ledger: %s\n%s\n",
		len(plan.repairs),
		ledgerPath,
		cacheSummary,
	); err != nil {
		return fmt.Errorf("write completed date repair summary: %w", err)
	}
	return nil
}

func dateRepairUsesAnalyticsCache(dsn string) bool {
	return !store.IsPostgresURL(dsn)
}

func applyPlannedDateRepairs(
	ctx context.Context,
	st *store.Store,
	plan *dateRepairPlan,
	lowerBound, upperBound time.Time,
	ledgerPath string,
	ledger *dateRepairLedger,
) error {
	updates := make([]store.MessageDateRepair, len(plan.repairs))
	for i := range plan.repairs {
		updates[i] = plan.repairs[i].MessageDateRepair
	}
	if err := st.ApplyMessageDateRepairs(
		ctx,
		updates,
		lowerBound,
		upperBound,
	); err != nil {
		ledger.Status = "failed"
		ledger.Error = err.Error()
		if ledgerErr := writeDateRepairLedger(ledgerPath, ledger); ledgerErr != nil {
			return errors.Join(
				err,
				fmt.Errorf("record failed date repair in audit ledger: %w", ledgerErr),
			)
		}
		return err
	}
	return nil
}

func dateRepairCacheRefreshError(err error) error {
	return dateRepairPostApplyRecoveryError("analytics cache refresh failed", err)
}

func dateRepairContactInvalidationError(err error) error {
	return fmt.Errorf(
		"date repairs applied, but contact state invalidation failed; "+
			"keep the repair ledger and run a full activity/contact-state rebuild "+
			"before relying on contact dates: %w",
		err,
	)
}

func dateRepairPostApplyRecoveryError(problem string, err error) error {
	return fmt.Errorf(
		"date repairs applied, but %s; "+
			"run `msgvault build-cache --full-rebuild` to retry: %w",
		problem,
		err,
	)
}

func scanAndPlanDateRepairs(
	ctx context.Context,
	st *store.Store,
	now time.Time,
) (*dateRepairPlan, error) {
	lowerBound, upperBound := mime.PlausibleDateBounds(now)
	candidates, err := st.ListMessageDateRepairCandidates(
		ctx,
		lowerBound,
		upperBound,
	)
	if err != nil {
		return nil, err
	}

	plan := &dateRepairPlan{candidateCount: len(candidates)}
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		parsed := &mime.Message{}
		raw, rawErr := st.GetMessageRaw(candidate.ID)
		if rawErr != nil {
			plan.rawMIMEFailures++
		} else {
			var parseErr error
			parsed, parseErr = mime.Parse(raw)
			if parseErr != nil {
				plan.rawMIMEFailures++
				parsed = &mime.Message{}
			}
		}

		var fallback time.Time
		if candidate.InternalDate.Valid {
			fallback = candidate.InternalDate.Time
		}
		resolved, source := mime.ResolveMessageDate(
			parsed.Date,
			parsed.ReceivedDates,
			fallback,
			now,
		)
		if resolved.IsZero() {
			plan.unresolved = append(plan.unresolved, unresolvedDateCandidate{
				id:     candidate.ID,
				reason: classifyUnresolvedDate(parsed),
			})
			continue
		}
		plan.repairs = append(plan.repairs, plannedDateRepair{
			ID:                     candidate.ID,
			SentAt:                 resolved,
			ExpectedLastModifiedAt: candidate.LastModifiedAt,
			source:                 source,
			oldSentAt:              candidate.SentAt,
		})
	}
	return plan, nil
}

// unresolvedDateReasonOrder fixes the print order to match the precedence
// ResolveMessageDate checks signals in: header, then received, then neither.
var unresolvedDateReasonOrder = []unresolvedDateReason{
	unresolvedDateHeaderUnusable,
	unresolvedReceivedUnusable,
	unresolvedNoDateSignal,
}

var unresolvedDateReasonLabel = map[unresolvedDateReason]string{
	unresolvedDateHeaderUnusable: "Date header present but not a plausible date",
	unresolvedReceivedUnusable:   "Received headers present but none plausible",
	unresolvedNoDateSignal:       "no Date header, no Received headers",
}

// printUnresolvedDateSamples groups unresolved candidates by reason and
// prints a capped sample of message IDs per group, so operators can look up
// exactly which messages are still corrupt instead of only a bare count.
func printUnresolvedDateSamples(out io.Writer, unresolved []unresolvedDateCandidate) {
	byReason := make(map[unresolvedDateReason][]int64, len(unresolvedDateReasonOrder))
	for _, u := range unresolved {
		byReason[u.reason] = append(byReason[u.reason], u.id)
	}
	for _, reason := range unresolvedDateReasonOrder {
		ids := byReason[reason]
		if len(ids) == 0 {
			continue
		}
		_, _ = fmt.Fprintf(out, "  %s: %d (%s)\n", reason, len(ids), unresolvedDateReasonLabel[reason])
		for i, id := range ids {
			if i >= dateRepairSampleLimit {
				_, _ = fmt.Fprintf(out, "    ... and %d more\n", len(ids)-dateRepairSampleLimit)
				break
			}
			_, _ = fmt.Fprintf(out, "    message %d\n", id)
		}
	}
}

// classifyUnresolvedDate explains why ResolveMessageDate produced no
// plausible time, in the same header/received/fallback order it checks.
func classifyUnresolvedDate(parsed *mime.Message) unresolvedDateReason {
	if parsed.RawDateHeader != "" {
		return unresolvedDateHeaderUnusable
	}
	if len(parsed.ReceivedDates) > 0 {
		return unresolvedReceivedUnusable
	}
	return unresolvedNoDateSignal
}

func printDateRepairPlan(cmd *cobra.Command, plan *dateRepairPlan) {
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(
		out,
		"Found %d message(s) with missing or implausible sent_at.\n",
		plan.candidateCount,
	)
	_, _ = fmt.Fprintf(out, "Repairable: %d\n", len(plan.repairs))
	if plan.rawMIMEFailures > 0 {
		_, _ = fmt.Fprintf(
			out,
			"Candidates lacking usable raw MIME: %d "+
				"(source metadata may still resolve them)\n",
			plan.rawMIMEFailures,
		)
	}
	if len(plan.unresolved) > 0 {
		_, _ = fmt.Fprintf(out, "No plausible replacement: %d\n", len(plan.unresolved))
		printUnresolvedDateSamples(out, plan.unresolved)
	}
	for i, repair := range plan.repairs {
		if i >= dateRepairSampleLimit {
			_, _ = fmt.Fprintf(
				out,
				"... and %d more\n",
				len(plan.repairs)-dateRepairSampleLimit,
			)
			break
		}
		old := "missing"
		if repair.oldSentAt.Valid {
			old = repair.oldSentAt.Time.UTC().Format(time.RFC3339)
		}
		_, _ = fmt.Fprintf(
			out,
			"  message %d: %s -> %s (%s)\n",
			repair.ID,
			old,
			repair.SentAt.UTC().Format(time.RFC3339),
			repair.source,
		)
	}
}

type dateRepairLedger struct {
	Version      int                     `json:"version"`
	Kind         string                  `json:"kind"`
	Status       string                  `json:"status"`
	StartedAt    time.Time               `json:"started_at"`
	AppliedAt    *time.Time              `json:"applied_at,omitempty"`
	CompletedAt  *time.Time              `json:"completed_at,omitempty"`
	CacheRebuilt bool                    `json:"cache_rebuilt"`
	Error        string                  `json:"error,omitempty"`
	Repairs      []dateRepairLedgerEntry `json:"repairs"`
}

type dateRepairLedgerEntry struct {
	MessageID int64           `json:"message_id"`
	OldSentAt *time.Time      `json:"old_sent_at"`
	NewSentAt time.Time       `json:"new_sent_at"`
	Source    mime.DateSource `json:"source"`
}

func newDateRepairLedger(
	plan *dateRepairPlan,
	now time.Time,
) *dateRepairLedger {
	entries := make([]dateRepairLedgerEntry, len(plan.repairs))
	for i, repair := range plan.repairs {
		var oldSentAt *time.Time
		if repair.oldSentAt.Valid {
			old := repair.oldSentAt.Time.UTC()
			oldSentAt = &old
		}
		entries[i] = dateRepairLedgerEntry{
			MessageID: repair.ID,
			OldSentAt: oldSentAt,
			NewSentAt: repair.SentAt.UTC(),
			Source:    repair.source,
		}
	}
	return &dateRepairLedger{
		Version:   1,
		Kind:      "message-date-repair",
		Status:    "applying",
		StartedAt: now.UTC(),
		Repairs:   entries,
	}
}

func availableDateRepairLedgerPath(dataDir string, now time.Time) (string, error) {
	dir := filepath.Join(dataDir, "repairs")
	stem := "dates-" + now.UTC().Format("20060102T150405.000000000Z")
	for suffix := 0; ; suffix++ {
		filename := stem + ".json"
		if suffix > 0 {
			filename = fmt.Sprintf("%s-%d.json", stem, suffix)
		}
		path := filepath.Join(dir, filename)
		_, err := os.Lstat(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			return path, nil
		case err != nil:
			return "", fmt.Errorf("check date repair ledger path: %w", err)
		}
	}
}

func writeDateRepairLedger(path string, ledger *dateRepairLedger) error {
	if ledger == nil {
		return errors.New("nil date repair ledger")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create repairs directory: %w", err)
	}
	file, err := os.CreateTemp(dir, ".dates-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary ledger: %w", err)
	}
	tempPath := file.Name()
	defer func() { _ = os.Remove(tempPath) }()

	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("set ledger permissions: %w", err)
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(ledger); err != nil {
		_ = file.Close()
		return fmt.Errorf("encode ledger: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync ledger: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close ledger: %w", err)
	}
	if err := replaceOutputFile(tempPath, path); err != nil {
		return fmt.Errorf("publish ledger: %w", err)
	}
	return nil
}

func init() {
	repairDatesCmd.Flags().BoolVar(
		&repairDatesApply,
		"apply",
		false,
		"write repaired dates to the archive",
	)
	rootCmd.AddCommand(repairDatesCmd)
}
