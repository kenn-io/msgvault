package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const acquisitionContractsCoverage = "Recognized contracts: fetch/fetch_error, fetch/batch_fetch_error, ingest/ingest_error, " +
	"ingest/attachment_error, ingest/remote_image_error, ingest/attachment_remote_image_error."

// AcquisitionDiagnosticOptions bounds details after classification. Totals cover
// the source's retained ledger in one read snapshot, independently of the page.
type AcquisitionDiagnosticOptions struct {
	SourceID            int64
	State               string
	AfterID             int64
	Limit               int
	RemoteImagesEnabled bool
}

type AcquisitionDiagnosticSummary struct {
	ActiveObligations   int64 `json:"active_obligations"`
	ResolvedObligations int64 `json:"resolved_obligations"`
	ActiveErrors        int64 `json:"active_errors"`
	HistoricalErrors    int64 `json:"historical_errors"`
	Historical          int64 `json:"historical"`
	Indeterminate       int64 `json:"indeterminate"`
	BlockedRemoteImages int64 `json:"blocked_remote_images"`
}

type AcquisitionDiagnosticItem struct {
	SourceMessageID string   `json:"source_message_id"`
	OutcomeID       int64    `json:"outcome_id"`
	SyncRunID       int64    `json:"sync_run_id"`
	Phase           string   `json:"phase"`
	ErrorKind       string   `json:"error_kind"`
	Operation       string   `json:"operation"`
	Synthetic       bool     `json:"synthetic"`
	HasError        bool     `json:"has_error"`
	ResolvedByID    int64    `json:"resolved_by_id"`
	LatestOutcomeID int64    `json:"latest_outcome_id"`
	LatestSyncRunID int64    `json:"latest_sync_run_id"`
	LatestPhase     string   `json:"latest_phase"`
	LatestStatus    string   `json:"latest_status"`
	LatestErrorKind string   `json:"latest_error_kind"`
	BlockedBy       []string `json:"blocked_by"`
}

type AcquisitionDiagnostic struct {
	Version               int                          `json:"version"`
	SourceID              int64                        `json:"source_id"`
	ObservedAt            time.Time                    `json:"observed_at"`
	IncrementalBoundaryID int64                        `json:"incremental_boundary_id"`
	LedgerThroughID       int64                        `json:"ledger_through_id"`
	Summary               AcquisitionDiagnosticSummary `json:"summary"`
	State                 string                       `json:"state"`
	Items                 []AcquisitionDiagnosticItem  `json:"items"`
	HasMore               bool                         `json:"has_more"`
	NextAfterID           int64                        `json:"next_after_id"`
	Coverage              []string                     `json:"coverage"`
	Blockers              []string                     `json:"blockers"`
}

// Materialize the replay selection once. Window aggregates carry the active
// outcome and explicit resolutions onto each message's errors without joining
// the ledger to itself. RANGE includes every outcome in the same run, even
// when older runs have later item IDs in a retained legacy ledger.
func acquisitionDiagnosticSQL() string {
	return `WITH active_ids AS MATERIALIZED (` + latestAcquisitionOutcomeIDsSQL(false) + `),
    outcomes AS (
        SELECT i.id, i.sync_run_id, i.source_message_id, i.phase, i.status, i.error_kind,
        ` + syntheticFileObligationSQL("i") + ` AS synthetic,
        ` + acquisitionOperationSQL("i") + ` AS operation
        FROM sync_run_items i JOIN sync_runs r ON r.id = i.sync_run_id WHERE r.source_id = ?
    ), ledger AS (
        SELECT outcomes.*,
        MAX(id) OVER message AS latest_outcome_id,
        MAX(CASE WHEN id IN (SELECT * FROM active_ids) AND status = 'error'
            AND operation <> 'unknown' THEN id ELSE 0 END) OVER operation_group AS active_outcome_id,
        MAX(CASE WHEN phase IN ('fetch', 'ingest') AND status = 'resolved' THEN id ELSE 0 END)
            OVER generations AS resolution_id,
        MAX(CASE WHEN phase IN ('fetch', 'ingest') AND status = 'skipped' THEN id ELSE 0 END)
            OVER generations AS skip_id
        FROM outcomes
        WINDOW message AS (PARTITION BY source_message_id),
        operation_group AS (PARTITION BY source_message_id, operation),
        generations AS (PARTITION BY source_message_id ORDER BY sync_run_id DESC
            RANGE BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW)
    ), classified AS (
        SELECT ledger.*, CASE
            WHEN operation <> 'unknown' AND status = 'skipped' THEN 'historical'
            WHEN operation <> 'unknown' AND resolution_id > id THEN 'resolved'
            WHEN operation = 'acquisition' AND skip_id > id THEN 'historical'
            WHEN operation <> 'unknown' AND active_outcome_id >= id THEN 'active'
            ELSE 'indeterminate' END AS state
        FROM ledger WHERE status = 'error' OR (status = 'skipped' AND operation <> 'unknown')
    ), grouped AS (
        SELECT source_message_id, state, MAX(id) AS id, MAX(latest_outcome_id) AS latest_outcome_id,
        MAX(CASE WHEN synthetic THEN 0 ELSE 1 END) AS has_error,
        MAX(CASE WHEN error_kind IN ('remote_image_error', 'attachment_remote_image_error') THEN 1 ELSE 0 END) AS remote_images
        FROM classified GROUP BY source_message_id, state
    ) `
}

func acquisitionDiagnosticSummarySQL() string {
	return acquisitionDiagnosticSQL() + `SELECT
        COALESCE(SUM(CASE WHEN state = 'active' THEN 1 ELSE 0 END), 0),
        COALESCE(SUM(CASE WHEN state = 'resolved' THEN 1 ELSE 0 END), 0),
        COALESCE(SUM(CASE WHEN state = 'active' AND has_error = 1 THEN 1 ELSE 0 END), 0),
        (SELECT COUNT(*) FROM (SELECT source_message_id FROM grouped
            WHERE state IN ('resolved', 'historical') AND has_error = 1 GROUP BY source_message_id) historical_errors),
        COALESCE(SUM(CASE WHEN state = 'historical' THEN 1 ELSE 0 END), 0),
        COALESCE(SUM(CASE WHEN state = 'indeterminate' THEN 1 ELSE 0 END), 0),
        COALESCE(SUM(CASE WHEN state = 'active' AND remote_images = 1 THEN 1 ELSE 0 END), 0)
        FROM grouped`
}

func acquisitionDiagnosticPageSQL() string {
	// Apply the page before hydrating details. Both lookups use base-table
	// primary keys, rather than repeatedly scanning the windowed ledger CTE.
	return acquisitionDiagnosticSQL() + `, page AS MATERIALIZED (
        SELECT id, latest_outcome_id, has_error, remote_images FROM grouped WHERE state = ? AND id > ? ORDER BY id LIMIT ?
    ) SELECT e.source_message_id, e.id, e.sync_run_id, e.phase, COALESCE(e.error_kind, ''),
        ` + acquisitionOperationSQL("e") + `, ` + syntheticFileObligationSQL("e") + `, page.has_error, page.remote_images,
        last.id, last.sync_run_id, last.phase, last.status, COALESCE(last.error_kind, '')
        FROM page JOIN sync_run_items e ON e.id = page.id
        JOIN sync_run_items last ON last.id = page.latest_outcome_id ORDER BY e.id`
}

func acquisitionDiagnosticResolutionsSQL(count int) string {
	return `SELECT i.source_message_id, i.id, i.sync_run_id
		FROM sync_run_items i JOIN sync_runs r ON r.id = i.sync_run_id
		WHERE r.source_id = ? AND i.id > ? AND i.phase IN ('fetch', 'ingest') AND i.status = 'resolved'
		AND i.source_message_id IN (` + strings.TrimSuffix(strings.Repeat("?,", count), ",") + `) ORDER BY i.id`
}

// DiagnoseAcquisitionContext performs SELECTs in a read-only snapshot. An SQL
// error or cancellation discards the whole report. No sync owner is acquired.
func (s *Store) DiagnoseAcquisitionContext(ctx context.Context, opts AcquisitionDiagnosticOptions) (*AcquisitionDiagnostic, error) {
	if opts.SourceID <= 0 || opts.AfterID < 0 || opts.Limit < 1 || opts.Limit > 1000 {
		return nil, errors.New("source ID must be positive, after ID nonnegative, and limit between 1 and 1000")
	}
	if opts.State == "" {
		opts.State = "active"
	}
	if opts.State != "active" && opts.State != "resolved" && opts.State != "historical" && opts.State != "indeterminate" {
		return nil, errors.New("state must be active, resolved, historical, or indeterminate")
	}
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	source, err := s.GetSourceByIDContext(ctx, opts.SourceID)
	if err != nil {
		return nil, err
	}
	if source.SourceType != "gmail" {
		return nil, errors.New("acquisition diagnostic requires a Gmail source")
	}
	report := &AcquisitionDiagnostic{
		Version: 1, SourceID: opts.SourceID, ObservedAt: time.Now().UTC(), State: opts.State,
		Items: []AcquisitionDiagnosticItem{}, Blockers: []string{},
		Coverage: []string{
			"Retained sync ledger for this source in one read-only snapshot; counts are messages per state and may overlap.",
			"Active obligations use the acquisition replay selection since the latest completed incremental; resolved requires a later explicit resolution.",
			acquisitionContractsCoverage,
			"Errors exclude the shipped synthetic file-saving marker. Details contain at most one outcome per message in the selected state.",
			"Historical includes withdrawn skipped items retaining a recognized error contract and acquisition failures followed by a compatible skip. Withdrawal does not prove acquisition or identify its cause.",
			"Indeterminate includes failures hidden by the replay boundary without resolution and outcomes with no known acquisition contract.",
			"Source blockers describe the normal replay path; provider availability and permanent impossibility are not tested. Resolution is a ledger acknowledgement, not a physical file integrity check.",
			"No physical RAW/blob integrity, MIME occurrence, FTS freshness, remote inventory, or historical omissions check. The absence of known active obligations does not prove archive completeness.",
			"Pages use outcome IDs after classification; concurrent sync can change later invocations. Repeat against a quiet archive for consistent traversal.",
		},
	}
	if source.HistoryOnly {
		report.Blockers = append(report.Blockers, "source_history_only")
	}
	if source.MergedIntoSourceID != 0 {
		report.Blockers = append(report.Blockers, "source_merged")
	}
	if err := s.db.QueryRowContext(ctx, `SELECT
		COALESCE((SELECT id FROM sync_runs WHERE source_id = ? AND sync_type = 'incremental'
		AND status = 'completed' ORDER BY completed_at DESC, id DESC LIMIT 1), 0),
		COALESCE((SELECT MAX(i.id) FROM sync_run_items i JOIN sync_runs r ON r.id = i.sync_run_id WHERE r.source_id = ?), 0)`,
		opts.SourceID, opts.SourceID).Scan(&report.IncrementalBoundaryID, &report.LedgerThroughID); err != nil {
		return nil, fmt.Errorf("read acquisition coverage: %w", err)
	}
	args := []any{opts.SourceID, opts.SourceID, opts.SourceID}
	var imageBlocked int64
	if err := s.db.QueryRowContext(ctx, acquisitionDiagnosticSummarySQL(), args...).Scan(
		&report.Summary.ActiveObligations, &report.Summary.ResolvedObligations,
		&report.Summary.ActiveErrors, &report.Summary.HistoricalErrors, &report.Summary.Historical, &report.Summary.Indeterminate, &imageBlocked); err != nil {
		return nil, fmt.Errorf("aggregate acquisition diagnostic: %w", err)
	}
	if !opts.RemoteImagesEnabled {
		report.Summary.BlockedRemoteImages = imageBlocked
	}
	args = append(args, opts.State, opts.AfterID, opts.Limit+1)
	rows, err := s.db.QueryContext(ctx, acquisitionDiagnosticPageSQL(), args...)
	if err != nil {
		return nil, fmt.Errorf("page acquisition diagnostic: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var item AcquisitionDiagnosticItem
		var hasError, images int
		if err := rows.Scan(&item.SourceMessageID, &item.OutcomeID, &item.SyncRunID, &item.Phase,
			&item.ErrorKind, &item.Operation, &item.Synthetic, &hasError, &images, &item.LatestOutcomeID, &item.LatestSyncRunID,
			&item.LatestPhase, &item.LatestStatus, &item.LatestErrorKind); err != nil {
			return nil, fmt.Errorf("scan acquisition diagnostic: %w", err)
		}
		if len(report.Items) == opts.Limit {
			report.HasMore = true
			break
		}
		item.HasError = hasError != 0
		item.BlockedBy = []string{}
		if opts.State == "active" {
			item.BlockedBy = append(item.BlockedBy, report.Blockers...)
			if images != 0 && !opts.RemoteImagesEnabled {
				item.BlockedBy = append(item.BlockedBy, "remote_image_archiving_disabled")
			}
		}
		report.Items = append(report.Items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read acquisition diagnostic: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close acquisition diagnostic page: %w", err)
	}
	if err := s.fillAcquisitionResolutions(ctx, report); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if report.HasMore {
		report.NextAfterID = report.Items[len(report.Items)-1].OutcomeID
	}
	return report, nil
}

// Find the first compatible acknowledgement for all page items in one scan.
// This is detail enrichment, not another classification or replay selection.
// Keeping it separate avoids a correlated ledger scan for every detail and
// preserves the original earliest-resolution references, including legacy
// outcomes whose run IDs and item IDs are not in the same order.
func (s *Store) fillAcquisitionResolutions(ctx context.Context, report *AcquisitionDiagnostic) error {
	pending := make(map[string]*AcquisitionDiagnosticItem)
	args := []any{report.SourceID}
	var ids []any
	var firstID int64
	for i := range report.Items {
		item := &report.Items[i]
		if report.State != "resolved" && item.Operation != "unknown" {
			continue
		}
		pending[item.SourceMessageID] = item
		ids = append(ids, item.SourceMessageID)
		if firstID == 0 || item.OutcomeID < firstID {
			firstID = item.OutcomeID
		}
	}
	if len(pending) == 0 {
		return nil
	}
	args = append(args, firstID)
	args = append(args, ids...)
	rows, err := s.db.QueryContext(ctx, acquisitionDiagnosticResolutionsSQL(len(ids)), args...)
	if err != nil {
		return fmt.Errorf("find acquisition resolutions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var messageID string
		var id, runID int64
		if err := rows.Scan(&messageID, &id, &runID); err != nil {
			return fmt.Errorf("scan acquisition resolution: %w", err)
		}
		if item := pending[messageID]; item != nil && id > item.OutcomeID && runID >= item.SyncRunID {
			item.ResolvedByID = id
			delete(pending, messageID)
			if len(pending) == 0 {
				break
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read acquisition resolutions: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close acquisition resolutions: %w", err)
	}
	return nil
}

// String provides a compact human summary without message contents or ledger
// error text, which may contain addresses, URLs, paths, or provider responses.
func (r AcquisitionDiagnostic) String() string {
	var out strings.Builder
	fmt.Fprintf(&out, "Gmail source %d:", r.SourceID)
	if r.Summary.ActiveObligations != 0 {
		fmt.Fprintf(&out, " %d active obligations,", r.Summary.ActiveObligations)
	}
	fmt.Fprintf(&out, " %d resolved obligations\n%d active errors, %d historical errors, %d indeterminate messages\n",
		r.Summary.ResolvedObligations, r.Summary.ActiveErrors, r.Summary.HistoricalErrors, r.Summary.Indeterminate)
	if r.Summary.ActiveObligations == 0 {
		out.WriteString("No known active obligation in the examined scope.\n")
	}
	fmt.Fprintf(&out, "Details: %s; incremental boundary run %d; ledger through item %d\n", r.State, r.IncrementalBoundaryID, r.LedgerThroughID)
	for _, item := range r.Items {
		fmt.Fprintf(&out, "  %q: %s/%s (run %d, item %d, synthetic=%t, error=%t)", item.SourceMessageID, item.Phase, item.ErrorKind, item.SyncRunID, item.OutcomeID, item.Synthetic, item.HasError)
		fmt.Fprintf(&out, " latest=%s/%s (run %d, item %d)", item.LatestPhase, item.LatestStatus, item.LatestSyncRunID, item.LatestOutcomeID)
		if len(item.BlockedBy) > 0 {
			fmt.Fprintf(&out, " blocked by %s", strings.Join(item.BlockedBy, ", "))
		}
		out.WriteByte('\n')
	}
	if r.HasMore {
		fmt.Fprintf(&out, "More details: --state %s --after-id %d\n", r.State, r.NextAfterID)
	}
	for _, limit := range r.Coverage {
		if limit == acquisitionContractsCoverage {
			continue
		}
		fmt.Fprintf(&out, "Coverage: %s\n", limit)
	}
	return out.String()
}
