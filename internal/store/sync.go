package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

const (
	SyncStatusRunning                      = "running"
	SyncStatusCompleted                    = "completed"
	SyncStatusFailed                       = "failed"
	GmailHistoryRecoveryRequestFingerprint = "gmail-history-recovery:v1"

	SyncRunItemStatusError   = "error"
	SyncRunItemStatusSkipped = "skipped"

	manualTransactionCleanupTimeout = 5 * time.Second
	syncWorkerExitedMessage         = "sync worker exited before recording completion"
	syncRunRestartedError           = "daemon_restarted"
)

// ErrSyncRunNotFound is returned by the sync-run getters (GetActiveSync,
// GetLatestCheckpointedSync, GetLastSuccessfulSync) when no matching run
// exists. Wrapped via fmt.Errorf so callers can use errors.Is to tell
// absence apart from real DB errors.
var ErrSyncRunNotFound = errors.New("sync run not found")

// ErrSyncAlreadyActive is returned when a source already has a running sync.
// Callers must wait for that worker to exit before starting another one.
var ErrSyncAlreadyActive = errors.New("sync already active")

// ErrSyncRunSuperseded is returned when a terminal write belongs to a sync
// generation that is no longer running or current for its source.
var ErrSyncRunSuperseded = errors.New("sync run superseded")

type syncGeneration struct {
	sourceID int64
	runID    int64
}

// ScopedToSync returns a Store view whose mutating transactions are fenced to
// one exact running source generation. The view shares the database pool and
// immutable configuration with s; callers must not Close it. A
// superseded or failed generation rejects later writes with
// ErrSyncRunSuperseded before the archive mutation starts.
func (s *Store) ScopedToSync(sourceID, syncRunID int64) *Store {
	base := s.withoutSyncScope()
	return &Store{
		db:                   base.db,
		dbPath:               base.dbPath,
		sqliteFilesystemPath: base.sqliteFilesystemPath,
		dialect:              base.dialect,
		readOnly:             base.readOnly,
		fts5Available:        base.fts5Available,

		syncGeneration:     &syncGeneration{sourceID: sourceID, runID: syncRunID},
		syncBase:           base,
		syncExecutionLocks: base.syncExecutionLocks,

		initSchemaWindowHook:                  base.initSchemaWindowHook,
		attributeSeedReadHook:                 base.attributeSeedReadHook,
		contentChangedBackfillBatchHook:       base.contentChangedBackfillBatchHook,
		backfillFTSBatchErrHook:               base.backfillFTSBatchErrHook,
		attachmentRoleRepairPreparedHook:      base.attachmentRoleRepairPreparedHook,
		cardDAVConflictResolveSnapshotHook:    base.cardDAVConflictResolveSnapshotHook,
		cardDAVTombstonePrepareSnapshotHook:   base.cardDAVTombstonePrepareSnapshotHook,
		identityMatchAcceptBeforeDecisionHook: base.identityMatchAcceptBeforeDecisionHook,
		personOperationBeforeIdentityLockHook: base.personOperationBeforeIdentityLockHook,
		personMergeAfterSnapshotHook:          base.personMergeAfterSnapshotHook,

		contentChangedBackfillBatchSizeOverride: base.contentChangedBackfillBatchSizeOverride,
	}
}

func (s *Store) withoutSyncScope() *Store {
	if s.syncGeneration == nil {
		return s
	}
	return s.syncBase
}

func (s *Store) fenceSyncGenerationTx(
	ctx context.Context, tx *loggedTx,
) error {
	if s.syncGeneration == nil {
		return nil
	}
	var runID int64
	err := tx.QueryRowContext(ctx, `
		UPDATE sync_runs
		SET messages_processed = messages_processed
		WHERE id = ? AND source_id = ? AND status = 'running'
		RETURNING id`, s.syncGeneration.runID, s.syncGeneration.sourceID).Scan(&runID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("fence sync run %d for source %d: %w",
			s.syncGeneration.runID, s.syncGeneration.sourceID,
			ErrSyncRunSuperseded)
	}
	if err != nil {
		return fmt.Errorf("fence sync run %d for source %d: %w",
			s.syncGeneration.runID, s.syncGeneration.sourceID, err)
	}
	return nil
}

func (s *Store) requireSyncSource(sourceID int64) error {
	if s.syncGeneration == nil || s.syncGeneration.sourceID == sourceID {
		return nil
	}
	return fmt.Errorf("sync run %d is scoped to source %d, not source %d",
		s.syncGeneration.runID, s.syncGeneration.sourceID, sourceID)
}

func (s *Store) requireSyncMessageSourceTx(
	tx querier, messageID int64,
) error {
	if s.syncGeneration == nil {
		return nil
	}
	var sourceID int64
	if err := tx.QueryRow(`SELECT source_id FROM messages WHERE id = ?`,
		messageID).Scan(&sourceID); err != nil {
		return fmt.Errorf("read message %d sync source: %w", messageID, err)
	}
	return s.requireSyncSource(sourceID)
}

func (s *Store) requireSyncConversationSourceTx(
	tx querier, conversationID int64,
) error {
	if s.syncGeneration == nil {
		return nil
	}
	var sourceID int64
	if err := tx.QueryRow(`SELECT source_id FROM conversations WHERE id = ?`,
		conversationID).Scan(&sourceID); err != nil {
		return fmt.Errorf("read conversation %d sync source: %w", conversationID, err)
	}
	return s.requireSyncSource(sourceID)
}

func (s *Store) withSyncMessageWriteContext(
	ctx context.Context,
	messageID int64,
	write func(querier) error,
) error {
	if s.syncGeneration == nil {
		return write(boundQuerier{ctx: ctx, q: s.db})
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		if err := s.requireSyncMessageSourceTx(q, messageID); err != nil {
			return err
		}
		return write(q)
	})
}

func (s *Store) withSyncConversationWriteContext(
	ctx context.Context,
	conversationID int64,
	write func(querier) error,
) error {
	if s.syncGeneration == nil {
		return write(boundQuerier{ctx: ctx, q: s.db})
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		if err := s.requireSyncConversationSourceTx(q, conversationID); err != nil {
			return err
		}
		return write(q)
	})
}

func (s *Store) withSyncSourceWriteContext(
	ctx context.Context,
	sourceID int64,
	write func(querier) error,
) error {
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	if s.syncGeneration == nil {
		return write(boundQuerier{ctx: ctx, q: s.db})
	}
	base := s.withoutSyncScope()
	return base.withTxContext(ctx, func(tx *loggedTx) error {
		if err := lockSyncSourceTx(ctx, tx, sourceID); err != nil {
			return err
		}
		if err := s.fenceSyncGenerationTx(ctx, tx); err != nil {
			return err
		}
		return write(boundQuerier{ctx: ctx, q: tx})
	})
}

// ErrSourceImportItemNotFound is returned by GetSourceImportItem when no
// import-item row matches. Wrapped via fmt.Errorf for errors.Is checks.
var ErrSourceImportItemNotFound = errors.New("source import item not found")

// dbTimeLayouts lists formats used by SQLite/go-sqlite3 for timestamp storage.
// This matches the full set from SQLiteTimestampFormats in mattn/go-sqlite3,
// plus RFC3339/RFC3339Nano as fallbacks for maximum compatibility.
// The order matters: more specific formats (with fractional seconds/timezones) come first.
var dbTimeLayouts = []string{
	// Formats from mattn/go-sqlite3 SQLiteTimestampFormats
	"2006-01-02 15:04:05.999999999-07:00", // space-separated with fractional seconds and TZ
	"2006-01-02T15:04:05.999999999-07:00", // T-separated with fractional seconds and TZ
	"2006-01-02 15:04:05.999999999",       // space-separated with fractional seconds
	"2006-01-02T15:04:05.999999999",       // T-separated with fractional seconds
	"2006-01-02 15:04:05",                 // SQLite datetime('now') format
	"2006-01-02T15:04:05",                 // T-separated basic
	"2006-01-02 15:04",                    // space-separated without seconds
	"2006-01-02T15:04",                    // T-separated without seconds
	"2006-01-02",                          // date only
	// Additional fallback formats
	time.RFC3339,     // go-sqlite3 DATETIME column format (e.g., "2006-01-02T15:04:05Z")
	time.RFC3339Nano, // RFC3339 with nanoseconds (e.g., "2006-01-02T15:04:05.999999999Z07:00")
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// parseDBTime attempts to parse a timestamp string using known SQLite/go-sqlite3 formats.
func parseDBTime(s string) (time.Time, error) {
	for _, layout := range dbTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized timestamp format %q", s)
}

// requireNullTime extracts a non-NULL time.Time from a sql.NullTime, with
// a clear error mentioning the field name. Required timestamps
// (created_at, updated_at, started_at) violate a schema invariant if NULL,
// so this surfaces the violation rather than silently zero-valuing.
func requireNullTime(nt sql.NullTime, field string) (time.Time, error) {
	if !nt.Valid {
		return time.Time{}, fmt.Errorf("%s: required timestamp is NULL", field)
	}
	return nt.Time, nil
}

func scanSource(sc scanner) (*Source, error) {
	// Scan timestamps into sql.NullTime / time.Time. The pgx/v5 stdlib
	// driver decodes TIMESTAMP/TIMESTAMPTZ as time.Time at the driver
	// level and refuses to convert that to *string; go-sqlite3 also
	// accepts time.Time destinations and parses its stored formats
	// internally, so a single typed scan path works for both backends.
	// Required fields are scanned through sql.NullTime so a NULL value
	// (a schema invariant violation) is reported with field context
	// rather than the driver's opaque "unsupported Scan" error.
	var source Source
	var createdAt, updatedAt sql.NullTime
	err := sc.Scan(
		&source.ID, &source.SourceType, &source.Identifier, &source.DisplayName,
		&source.GoogleUserID, &source.LastSyncAt, &source.SyncCursor, &source.SyncConfig,
		&source.OAuthApp, &createdAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}
	source.CreatedAt, err = requireNullTime(createdAt, "created_at")
	if err != nil {
		return nil, fmt.Errorf("source %d: %w", source.ID, err)
	}
	source.UpdatedAt, err = requireNullTime(updatedAt, "updated_at")
	if err != nil {
		return nil, fmt.Errorf("source %d: %w", source.ID, err)
	}
	return &source, nil
}

func scanSyncRun(sc scanner) (*SyncRun, error) {
	// Scan timestamps into typed columns — see scanSource for the
	// dialect-portability rationale.
	var run SyncRun
	var startedAt sql.NullTime
	err := sc.Scan(
		&run.ID, &run.SourceID, &startedAt, &run.CompletedAt, &run.Status,
		&run.MessagesProcessed, &run.MessagesAdded, &run.MessagesUpdated, &run.ErrorsCount,
		&run.ErrorMessage, &run.CursorBefore, &run.CursorAfter, &run.RequestFingerprint,
	)
	if err != nil {
		return nil, err
	}
	run.StartedAt, err = requireNullTime(startedAt, "started_at")
	if err != nil {
		return nil, fmt.Errorf("sync_run %d: %w", run.ID, err)
	}
	return &run, nil
}

// SyncRun represents a sync operation in progress or completed.
type SyncRun struct {
	ID                 int64
	SourceID           int64
	StartedAt          time.Time
	CompletedAt        sql.NullTime
	Status             string // SyncStatusRunning, SyncStatusCompleted, SyncStatusFailed
	MessagesProcessed  int64
	MessagesAdded      int64
	MessagesUpdated    int64
	ErrorsCount        int64
	ErrorMessage       sql.NullString
	CursorBefore       sql.NullString // Page token for resumption
	CursorAfter        sql.NullString // Final history ID
	RequestFingerprint sql.NullString // Full-sync request identity for safe resumption
}

// RecoverSyncRunsContext terminalizes source runs whose owning daemon ended.
// Native checkpoints and counters remain untouched so a later invocation can
// resume through the source-specific recovery path.
func (s *Store) RecoverSyncRunsContext(ctx context.Context, recoveredAt time.Time) (int64, error) {
	if recoveredAt.IsZero() {
		return 0, errors.New("sync run recovery time is required")
	}
	result, err := s.db.ExecContext(ctx, s.Rebind(`UPDATE sync_runs
		SET status = 'failed', completed_at = ?, error_message = ?
		WHERE status = 'running'`), s.dialect.TimestampParam(recoveredAt.UTC()), syncRunRestartedError)
	if err != nil {
		return 0, fmt.Errorf("recover source sync runs: %w", err)
	}
	recovered, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count recovered source sync runs: %w", err)
	}
	return recovered, nil
}

// Checkpoint represents sync progress for resumption.
type Checkpoint struct {
	PageToken         string
	MessagesProcessed int64
	MessagesAdded     int64
	MessagesUpdated   int64
	ErrorsCount       int64
}

// SyncRunItem records an individual item outcome within a sync run.
// Error items are actionable and count toward SyncRun.ErrorsCount; skipped
// items preserve expected churn such as Gmail messages deleted before fetch.
type SyncRunItem struct {
	ID              int64
	SyncRunID       int64
	SourceMessageID string
	Phase           string
	Status          string
	ErrorKind       string
	ErrorMessage    string
	CreatedAt       time.Time
}

type SourceImportItem struct {
	ID              int64
	SourceID        int64
	Provider        string
	ProviderID      string
	Name            string
	Checksum        string
	Size            int64
	ModifiedAt      sql.NullTime
	ImportedAt      sql.NullTime
	Status          string
	RecordsImported int
	ErrorMessage    sql.NullString
}

// StartSync creates a new sync run record and returns its ID. It first takes a
// non-blocking execution lock for the source and holds that lock until the run
// completes or fails. SQLite uses an OS file lock; PostgreSQL uses a session
// advisory lock on a dedicated connection. Both release automatically when
// the worker process exits. Once the execution lock is held, any running row
// for that source has no live owner and is failed before the new row is added.
// The stale-row transition and INSERT share a writer-locked transaction.
func (s *Store) StartSync(sourceID int64, syncType string) (int64, error) {
	return s.StartSyncContext(context.Background(), sourceID, syncType)
}

// StartSyncContext is the request-aware form of StartSync.
func (s *Store) StartSyncContext(ctx context.Context, sourceID int64, syncType string) (int64, error) {
	return s.startSyncContext(ctx, sourceID, syncType, "")
}

// StartSyncOperation creates a sync run attributed to an operation previously
// persisted with CreateSyncOperation. Every phase in a multi-run sync uses the
// same operation ID.
func (s *Store) StartSyncOperation(sourceID int64, operationID string) (int64, error) {
	return s.startSyncContext(context.Background(), sourceID, "", operationID)
}

func (s *Store) startSyncContext(
	ctx context.Context, sourceID int64, syncType, operationID string,
) (int64, error) {
	return s.startSyncContextWithLock(ctx, sourceID, syncType, operationID, "", nil)
}

func (s *Store) startSyncContextWithLock(
	ctx context.Context,
	sourceID int64,
	syncType, operationID, requestFingerprint string,
	heldLock syncExecutionLock,
) (int64, error) {
	const maxAttempts = 5
	for range maxAttempts {
		id, err := s.startSyncOnce(
			ctx, sourceID, syncType, operationID, requestFingerprint, heldLock,
		)
		if err == nil {
			return id, nil
		}
		if !s.dialect.IsBusyError(err) {
			return 0, err
		}
	}
	return 0, fmt.Errorf("start sync: gave up after %d retries on busy", maxAttempts)
}

func (s *Store) startSyncOnce(
	ctx context.Context,
	sourceID int64,
	syncType, operationID, requestFingerprint string,
	heldLock syncExecutionLock,
) (retID int64, retErr error) {
	executionLock := heldLock
	releaseWhenDone := executionLock == nil
	if releaseWhenDone {
		var err error
		executionLock, err = s.acquireSyncExecutionLock(ctx, sourceID)
		if err != nil {
			return 0, fmt.Errorf("acquire source %d sync execution lock: %w", sourceID, err)
		}
		defer func() {
			if retErr != nil {
				retErr = errors.Join(retErr, s.abandonSyncExecutionLock(sourceID, executionLock))
			}
		}()
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("acquire connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, s.dialect.BeginWriteSQL()); err != nil {
		return 0, fmt.Errorf("begin write tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			rollbackCtx, rollbackCancel := context.WithTimeout(
				context.WithoutCancel(ctx),
				manualTransactionCleanupTimeout,
			)
			defer rollbackCancel()
			_, _ = conn.ExecContext(rollbackCtx, "ROLLBACK")
		}
	}()

	rebind := s.dialect.Rebind
	now := s.dialect.Now()

	// Serialize against concurrent StartSync for the same source.
	// SQLite already serializes writers under BEGIN IMMEDIATE; PG
	// needs an explicit row lock on the source so the read snapshot
	// for the check below cannot miss a concurrently committed
	// running run.
	if releaseWhenDone {
		if err := s.recoverAbandonedSyncSourceQueries(ctx, conn, sourceID, now); err != nil {
			return 0, err
		}
	} else {
		var lockedID int64
		if err := conn.QueryRowContext(ctx,
			rebind(`SELECT id FROM sources WHERE id = ?`+s.dialect.SelectForUpdate()),
			sourceID,
		).Scan(&lockedID); err != nil {
			return 0, fmt.Errorf("lock source row: %w", err)
		}
	}
	if err := s.rejectConflictingSyncOperation(ctx, conn, sourceID, operationID); err != nil {
		return 0, err
	}
	if operationID != "" {
		result, err := conn.ExecContext(ctx, rebind(fmt.Sprintf(`
			UPDATE sync_operations
			SET status = 'running', started_at = COALESCE(started_at, %s)
			WHERE id = ? AND source_id = ? AND status IN ('pending', 'running')
		`, now)), operationID, sourceID)
		if err != nil {
			return 0, fmt.Errorf("start sync operation %q: %w", operationID, err)
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("start sync operation %q: rows affected: %w", operationID, err)
		}
		if updated != 1 {
			return 0, fmt.Errorf("start sync operation %q: %w", operationID, ErrSyncRunNotFound)
		}
	}
	var syncRunID int64
	var personSweepLowerBound int64
	if err := conn.QueryRowContext(ctx, `
		SELECT sequence FROM person_sweep_change_clock WHERE singleton = TRUE`,
	).Scan(&personSweepLowerBound); err != nil {
		return 0, fmt.Errorf("capture sync person sweep lower bound: %w", err)
	}
	if err := conn.QueryRowContext(ctx,
		rebind(fmt.Sprintf(`
				INSERT INTO sync_runs (
					source_id, sync_type, started_at, status, messages_processed,
					messages_added, messages_updated, errors_count,
					request_fingerprint, operation_id
				)
				VALUES (?, ?, %s, 'running', 0, 0, 0, 0, NULLIF(?, ''), NULLIF(?, ''))
				RETURNING id
			`, now)),
		sourceID, syncType, requestFingerprint, operationID,
	).Scan(&syncRunID); err != nil {
		return 0, fmt.Errorf("insert sync_run: %w", err)
	}
	if _, err := conn.ExecContext(ctx, rebind(`
		INSERT INTO person_sweep_sync_publications
			(sync_run_id, source_id, lower_sequence)
		VALUES (?, ?, ?)`), syncRunID, sourceID, personSweepLowerBound); err != nil {
		return 0, fmt.Errorf("record sync person sweep lower bound: %w", err)
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	committed = true
	s.registerSyncExecutionLock(sourceID, syncRunID, executionLock, releaseWhenDone)
	return syncRunID, nil
}

func (s *Store) recoverAbandonedSyncSource(ctx context.Context, sourceID int64) error {
	return s.withoutSyncScope().withTxContext(ctx, func(tx *loggedTx) error {
		return s.recoverAbandonedSyncSourceQueries(ctx, tx, sourceID, s.dialect.Now())
	})
}

func (s *Store) recoverAbandonedSyncSourceQueries(
	ctx context.Context, q contextStatementQuerier, sourceID int64, now string,
) error {
	var lockedID int64
	if err := q.QueryRowContext(ctx,
		s.Rebind(`SELECT id FROM sources WHERE id = ?`+s.dialect.SelectForUpdate()),
		sourceID,
	).Scan(&lockedID); err != nil {
		return fmt.Errorf("lock source row: %w", err)
	}
	if _, err := q.ExecContext(ctx, s.Rebind(fmt.Sprintf(`
		UPDATE sync_operations
		SET status = 'failed', finished_at = %s
		WHERE source_id = ? AND status = 'running'
		  AND EXISTS (
			SELECT 1 FROM sync_runs
			WHERE operation_id = sync_operations.id AND status = 'running'
		  )`, now)), sourceID); err != nil {
		return fmt.Errorf("fail abandoned sync operation: %w", err)
	}
	if _, err := q.ExecContext(ctx, s.Rebind(fmt.Sprintf(`
		UPDATE sync_runs
		SET status = 'failed', completed_at = %s,
			error_message = ?
		WHERE source_id = ? AND status = 'running'`, now)), syncWorkerExitedMessage, sourceID); err != nil {
		return fmt.Errorf("fail abandoned sync: %w", err)
	}
	return nil
}

// SyncOperation is the durable status of one higher-level sync invocation.
// A Gmail history recovery can contribute more than one SyncRun.
type SyncOperation struct {
	ID         string
	SourceID   int64
	Status     string
	CreatedAt  time.Time
	StartedAt  sql.NullTime
	FinishedAt sql.NullTime
	Runs       []*SyncRun
}

func (s *Store) rejectConflictingSyncOperation(
	ctx context.Context, q contextStatementQuerier, sourceID int64, operationID string,
) error {
	var conflictingID string
	err := q.QueryRowContext(ctx, s.Rebind(`
		SELECT id
		FROM sync_operations
		WHERE source_id = ?
		  AND status IN ('pending', 'running')
		  AND id != ?
		LIMIT 1
	`), sourceID, operationID).Scan(&conflictingID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check source %d sync operation: %w", sourceID, err)
	}
	return fmt.Errorf("source %d sync operation %q: %w", sourceID, conflictingID, ErrSyncAlreadyActive)
}

// CreateSyncOperation reserves a source for a pending higher-level sync
// invocation before its worker starts.
func (s *Store) CreateSyncOperation(
	sourceID int64, operationID string,
) (operation *SyncOperation, retErr error) {
	if operationID == "" {
		return nil, errors.New("create sync operation: empty operation ID")
	}
	execution, err := s.AcquireSyncExecutionContext(context.Background(), sourceID)
	if err != nil {
		return nil, fmt.Errorf("reserve source %d for sync operation: %w", sourceID, err)
	}
	defer func() {
		if err := execution.Release(); err != nil {
			operation = nil
			retErr = errors.Join(retErr, fmt.Errorf("release source %d sync operation reservation: %w", sourceID, err))
		}
	}()

	var createdAt sql.NullTime
	err = s.withTx(func(tx *loggedTx) error {
		if err := lockSyncSourceTx(context.Background(), tx, sourceID); err != nil {
			return err
		}
		if err := s.rejectConflictingSyncOperation(
			context.Background(), tx, sourceID, operationID,
		); err != nil {
			return err
		}
		return tx.QueryRow(fmt.Sprintf(`
			INSERT INTO sync_operations (id, source_id, status, created_at)
			VALUES (?, ?, 'pending', %s)
			RETURNING created_at
		`, s.dialect.Now()), operationID, sourceID).Scan(&createdAt)
	})
	if err != nil {
		return nil, fmt.Errorf("create sync operation %q: %w", operationID, err)
	}
	created, err := requireNullTime(createdAt, "created_at")
	if err != nil {
		return nil, fmt.Errorf("create sync operation %q: %w", operationID, err)
	}
	return &SyncOperation{
		ID: operationID, SourceID: sourceID, Status: "pending", CreatedAt: created,
	}, nil
}

// FailUnfinishedSyncOperationsContext marks operations owned by an earlier
// daemon process. Callers must hold exclusive daemon ownership so no live
// worker can still be running or preparing an operation.
func (s *Store) FailUnfinishedSyncOperationsContext(ctx context.Context) (int64, error) {
	base := s.withoutSyncScope()
	var failed int64
	err := base.withTxContext(ctx, func(tx *loggedTx) error {
		now := base.dialect.Now()
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
			UPDATE sync_runs
			SET status = 'failed', completed_at = %s,
				error_message = ?
			WHERE status = 'running'
			  AND operation_id IN (
				SELECT id FROM sync_operations
				WHERE status IN ('pending', 'running', 'failed')
			  )
		`, now), syncWorkerExitedMessage); err != nil {
			return fmt.Errorf("fail unfinished sync operation runs: %w", err)
		}
		result, err := tx.ExecContext(ctx, fmt.Sprintf(`
			UPDATE sync_operations
			SET status = 'failed', finished_at = %s
			WHERE status IN ('pending', 'running')
		`, now))
		if err != nil {
			return fmt.Errorf("fail unfinished sync operations: %w", err)
		}
		failed, err = result.RowsAffected()
		if err != nil {
			return fmt.Errorf("fail unfinished sync operations: rows affected: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return failed, nil
}

// GetSyncOperation returns every sync run attributed to operationID.
func (s *Store) GetSyncOperation(operationID string) (*SyncOperation, error) {
	return s.getSyncOperation(operationID)
}

func (s *Store) getSyncOperation(operationID string) (*SyncOperation, error) {
	op := &SyncOperation{ID: operationID}
	var createdAt sql.NullTime
	err := s.db.QueryRow(`
		SELECT source_id, status, created_at, started_at, finished_at
		FROM sync_operations
		WHERE id = ?
	`, operationID).Scan(
		&op.SourceID, &op.Status, &createdAt, &op.StartedAt, &op.FinishedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("sync operation %q: %w", operationID, ErrSyncRunNotFound)
	}
	if err != nil {
		return nil, err
	}
	op.CreatedAt, err = requireNullTime(createdAt, "created_at")
	if err != nil {
		return nil, fmt.Errorf("sync operation %q: %w", operationID, err)
	}
	rows, err := s.db.Query(`
		SELECT id, source_id, started_at, completed_at, status,
		       messages_processed, messages_added, messages_updated, errors_count,
		       error_message, cursor_before, cursor_after
		FROM sync_runs
		WHERE operation_id = ?
		ORDER BY id
	`, operationID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var run SyncRun
		var startedAt sql.NullTime
		if err := rows.Scan(
			&run.ID, &run.SourceID, &startedAt, &run.CompletedAt, &run.Status,
			&run.MessagesProcessed, &run.MessagesAdded, &run.MessagesUpdated, &run.ErrorsCount,
			&run.ErrorMessage, &run.CursorBefore, &run.CursorAfter,
		); err != nil {
			return nil, err
		}
		run.StartedAt, err = requireNullTime(startedAt, "started_at")
		if err != nil {
			return nil, fmt.Errorf("sync_run %d: %w", run.ID, err)
		}
		op.Runs = append(op.Runs, &run)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return op, nil
}

func (s *Store) recoverSyncSourceIfUnowned(ctx context.Context, sourceID int64) (bool, error) {
	execution, err := s.AcquireSyncExecutionContext(ctx, sourceID)
	if errors.Is(err, ErrSyncAlreadyActive) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, execution.Release()
}

// FinishSyncOperation records the first terminal result for an operation.
// Repeated finalization is idempotent and cannot replace that result.
func (s *Store) FinishSyncOperation(operationID, status string) error {
	if status != "done" && status != "failed" {
		return fmt.Errorf("invalid sync operation status %q", status)
	}
	base := s.withoutSyncScope()
	return base.withTx(func(tx *loggedTx) error {
		now := base.dialect.Now()
		result, err := tx.Exec(fmt.Sprintf(`
			UPDATE sync_operations
			SET status = ?, finished_at = %s
			WHERE id = ? AND status IN ('pending', 'running')
		`, now), status, operationID)
		if err != nil {
			return err
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("finish sync operation %q: rows affected: %w", operationID, err)
		}
		currentStatus := status
		if updated != 1 {
			if err := tx.QueryRow(`SELECT status FROM sync_operations WHERE id = ?`, operationID).Scan(&currentStatus); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("finish sync operation %q: %w", operationID, ErrSyncRunNotFound)
				}
				return fmt.Errorf("read finished sync operation %q: %w", operationID, err)
			}
		}
		if currentStatus != "done" && currentStatus != SyncStatusFailed {
			return fmt.Errorf("finish sync operation %q: unexpected status %q", operationID, currentStatus)
		}
		if currentStatus != SyncStatusFailed {
			return nil
		}
		if _, err := tx.Exec(fmt.Sprintf(`
			UPDATE sync_runs
			SET status = 'failed', completed_at = %s, error_message = ?
			WHERE operation_id = ? AND status = 'running'
		`, now), syncWorkerExitedMessage, operationID); err != nil {
			return fmt.Errorf("fail running sync runs for operation %q: %w", operationID, err)
		}
		return nil
	})
}

// UpdateSyncCheckpoint saves progress for resumption.
func (s *Store) UpdateSyncCheckpoint(syncID int64, cp *Checkpoint) error {
	return s.UpdateSyncCheckpointContext(context.Background(), syncID, cp)
}

// UpdateSyncCheckpointContext is the request-aware form of
// UpdateSyncCheckpoint.
func (s *Store) UpdateSyncCheckpointContext(ctx context.Context, syncID int64, cp *Checkpoint) error {
	write := func(q querier) error {
		_, err := q.Exec(`
		UPDATE sync_runs
		SET cursor_before = ?,
		    messages_processed = ?,
		    messages_added = ?,
		    messages_updated = ?,
		    errors_count = ?
		WHERE id = ?
		`, cp.PageToken, cp.MessagesProcessed, cp.MessagesAdded, cp.MessagesUpdated, cp.ErrorsCount, syncID)
		return err
	}
	if s.syncGeneration != nil {
		if syncID != s.syncGeneration.runID {
			return fmt.Errorf("checkpoint sync run %d through scoped run %d: %w",
				syncID, s.syncGeneration.runID, ErrSyncRunSuperseded)
		}
		return s.withTxContext(ctx, func(tx *loggedTx) error {
			return write(boundQuerier{ctx: ctx, q: tx})
		})
	}
	return write(boundQuerier{ctx: ctx, q: s.db})
}

// PinSyncHandoffCursorContext records the history cursor a resumable recovery
// must catch up from after its full enumeration. A completed sync overwrites
// cursor_after with the same value when it publishes the source cursor.
func (s *Store) PinSyncHandoffCursorContext(ctx context.Context, syncID int64, cursor string) error {
	if cursor == "" {
		return errors.New("pin sync handoff cursor: empty cursor")
	}
	if s.syncGeneration == nil {
		return errors.New("pin sync handoff cursor: sync-scoped store required")
	}
	if syncID != s.syncGeneration.runID {
		return fmt.Errorf("pin handoff cursor for sync run %d through scoped run %d: %w",
			syncID, s.syncGeneration.runID, ErrSyncRunSuperseded)
	}
	write := func(q querier) error {
		_, err := q.Exec(`UPDATE sync_runs SET cursor_after = ? WHERE id = ?`, cursor, syncID)
		return err
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		return write(boundQuerier{ctx: ctx, q: tx})
	})
}

// RecordSyncRunItem records a per-item sync outcome for diagnostics.
func (s *Store) RecordSyncRunItem(item SyncRunItem) error {
	_, err := s.db.Exec(fmt.Sprintf(`
		INSERT INTO sync_run_items (
			sync_run_id, source_message_id, phase, status,
			error_kind, error_message, created_at
		) VALUES (?, ?, ?, ?, ?, ?, %s)
	`, s.dialect.Now()),
		item.SyncRunID, item.SourceMessageID, item.Phase, item.Status,
		item.ErrorKind, item.ErrorMessage,
	)
	if err != nil {
		return fmt.Errorf("insert sync_run_item: %w", err)
	}
	return nil
}

// CountSyncRunItems returns the number of recorded per-item sync outcomes for
// a run. If status is non-empty, only items with that status are counted.
func (s *Store) CountSyncRunItems(syncRunID int64, status string) (int64, error) {
	query := `SELECT COUNT(*) FROM sync_run_items WHERE sync_run_id = ?`
	args := []any{syncRunID}
	if status != "" {
		query += ` AND status = ?`
		args = append(args, status)
	}
	var count int64
	if err := s.db.QueryRow(query, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("count sync_run_items: %w", err)
	}
	return count, nil
}

// ListSyncRunItems returns the newest recorded per-item sync outcomes for a
// run. If status is non-empty, only items with that status are returned.
func (s *Store) ListSyncRunItems(syncRunID int64, status string, limit int) ([]SyncRunItem, error) {
	if limit <= 0 {
		return nil, nil
	}

	query := `
		SELECT id, sync_run_id, source_message_id, phase, status,
		       error_kind, error_message, created_at
		FROM sync_run_items
		WHERE sync_run_id = ?
	`
	args := []any{syncRunID}
	if status != "" {
		query += ` AND status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list sync_run_items: %w", err)
	}
	defer func() { _ = rows.Close() }()

	items := make([]SyncRunItem, 0)
	for rows.Next() {
		var item SyncRunItem
		var createdAt sql.NullTime
		if err := rows.Scan(
			&item.ID, &item.SyncRunID, &item.SourceMessageID, &item.Phase,
			&item.Status, &item.ErrorKind, &item.ErrorMessage, &createdAt,
		); err != nil {
			return nil, fmt.Errorf("scan sync_run_item: %w", err)
		}
		var err error
		item.CreatedAt, err = requireNullTime(createdAt, "created_at")
		if err != nil {
			return nil, fmt.Errorf("sync_run_item %d: %w", item.ID, err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sync_run_items: %w", err)
	}
	return items, nil
}

// CompleteSync marks a sync as successfully completed.
func (s *Store) CompleteSync(syncID int64, finalHistoryID string) error {
	return s.CompleteSyncContext(context.Background(), syncID, finalHistoryID)
}

// CompleteSyncContext is the request-aware form of CompleteSync.
func (s *Store) CompleteSyncContext(ctx context.Context, syncID int64, finalHistoryID string) error {
	// Terminal writes already fence status/current-generation inside their
	// callback. Avoid the scoped mutation pre-fence here: cursor publication
	// takes the source row before the run row, matching StartSync's lock order.
	completionStore := s.withoutSyncScope()
	err := completionStore.withTxContext(ctx, func(tx *loggedTx) error {
		var sourceID int64
		err := tx.QueryRowContext(ctx, s.Rebind(fmt.Sprintf(`
			UPDATE sync_runs
			SET status = 'completed', completed_at = %s, cursor_after = ?
			WHERE id = ? AND status = 'running'
			RETURNING source_id`, s.dialect.Now())), finalHistoryID, syncID).Scan(&sourceID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("complete sync %d: %w", syncID, ErrSyncRunSuperseded)
		}
		if err != nil {
			return fmt.Errorf("complete sync %d: %w", syncID, err)
		}
		if err := completionStore.coalescePersonSweepChangesTx(ctx, tx, syncID, sourceID); err != nil {
			return fmt.Errorf("complete sync %d: %w", syncID, err)
		}
		return nil
	})
	if err == nil {
		completionStore.optimizeSQLiteBestEffort(ctx, "successful sync")
	}
	return completionStore.finalizeSyncExecution(syncID, err)
}

// CompleteSyncAndUpdateSourceCursor atomically publishes a source cursor and
// completes its still-current sync generation.
func (s *Store) CompleteSyncAndUpdateSourceCursor(
	syncID int64, sourceID int64, finalHistoryID string,
) error {
	return s.CompleteSyncAndUpdateSourceCursorContext(
		context.Background(), syncID, sourceID, finalHistoryID,
	)
}

// CompleteSyncAndUpdateSourceCursorContext is the request-aware form of
// CompleteSyncAndUpdateSourceCursor. It shares the source lock used by
// StartSync, so a newer generation cannot start between cursor publication and
// run completion.
func (s *Store) CompleteSyncAndUpdateSourceCursorContext(
	ctx context.Context, syncID int64, sourceID int64, finalHistoryID string,
) error {
	return s.completeSyncAndUpdateSourceContext(
		ctx, syncID, sourceID, finalHistoryID, true,
	)
}

// CompleteSyncAndPreserveSourceCursorContext completes a bounded full sync
// without treating its partial mailbox view as an incremental-sync baseline.
// The source still records when the successful run finished.
func (s *Store) CompleteSyncAndPreserveSourceCursorContext(
	ctx context.Context, syncID int64, sourceID int64, finalHistoryID string,
) error {
	return s.completeSyncAndUpdateSourceContext(
		ctx, syncID, sourceID, finalHistoryID, false,
	)
}

func (s *Store) completeSyncAndUpdateSourceContext(
	ctx context.Context,
	syncID int64,
	sourceID int64,
	finalHistoryID string,
	updateCursor bool,
) error {
	completionStore := s.withoutSyncScope()
	err := completionStore.withTxContext(ctx, func(tx *loggedTx) error {
		if err := validateCurrentSyncGeneration(
			ctx, tx, sourceID, syncID, SyncStatusRunning,
		); err != nil {
			return fmt.Errorf("complete sync %d: %w", syncID, err)
		}

		now := s.dialect.Now()
		updateSourceSQL := fmt.Sprintf(`
			UPDATE sources
			SET last_sync_at = %s, updated_at = %s
			WHERE id = ?
		`, now, now)
		args := []any{sourceID}
		if updateCursor {
			updateSourceSQL = fmt.Sprintf(`
				UPDATE sources
				SET sync_cursor = ?, last_sync_at = %s, updated_at = %s
				WHERE id = ?
			`, now, now)
			args = []any{finalHistoryID, sourceID}
		}
		result, err := tx.ExecContext(ctx, updateSourceSQL, args...)
		if err != nil {
			return fmt.Errorf("complete sync %d: update source cursor: %w", syncID, err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("complete sync %d: inspect source cursor update: %w", syncID, err)
		}
		if rows != 1 {
			return fmt.Errorf("complete sync %d: source %d not found", syncID, sourceID)
		}

		result, err = tx.ExecContext(ctx, fmt.Sprintf(`
			UPDATE sync_runs
			SET status = 'completed',
			    completed_at = %s,
			    cursor_after = ?
			WHERE id = ? AND source_id = ? AND status = 'running'
		`, now), finalHistoryID, syncID, sourceID)
		if err != nil {
			return fmt.Errorf("complete sync %d: %w", syncID, err)
		}
		rows, err = result.RowsAffected()
		if err != nil {
			return fmt.Errorf("complete sync %d: inspect update: %w", syncID, err)
		}
		if rows != 1 {
			return fmt.Errorf("complete sync %d: %w", syncID, ErrSyncRunSuperseded)
		}
		if err := completionStore.coalescePersonSweepChangesTx(ctx, tx, syncID, sourceID); err != nil {
			return fmt.Errorf("complete sync %d: %w", syncID, err)
		}
		return nil
	})
	if err == nil {
		completionStore.optimizeSQLiteBestEffort(ctx, "successful sync")
	}
	return completionStore.finalizeSyncExecution(syncID, err)
}

// finalizeSyncExecution releases source ownership after a terminal write
// attempt. Once a worker attempts its terminal transition, it must not retain
// execution ownership even when the database rejects that transition; the next
// owner can then recover the still-running row.
func (s *Store) finalizeSyncExecution(syncID int64, writeErr error) error {
	releaseErr := s.withoutSyncScope().releaseSyncExecutionLock(syncID)
	if writeErr == nil {
		return releaseErr
	}
	if releaseErr == nil {
		return writeErr
	}
	return errors.Join(writeErr, releaseErr)
}

func validateCurrentSyncGeneration(
	ctx context.Context,
	tx *loggedTx,
	sourceID int64,
	syncRunID int64,
	expectedStatus string,
) error {
	if err := lockSyncSourceTx(ctx, tx, sourceID); err != nil {
		return err
	}

	var latestID int64
	var latestStatus string
	err := tx.QueryRowContext(ctx, `
		SELECT id, status FROM sync_runs
		WHERE source_id = ?
		ORDER BY id DESC
		LIMIT 1
	`, sourceID).Scan(&latestID, &latestStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrSyncRunSuperseded
	}
	if err != nil {
		return fmt.Errorf("query current sync generation for source %d: %w", sourceID, err)
	}
	if latestID != syncRunID || latestStatus != expectedStatus {
		return fmt.Errorf(
			"latest sync %d is %s, want sync %d in %s: %w",
			latestID, latestStatus, syncRunID, expectedStatus, ErrSyncRunSuperseded,
		)
	}
	return nil
}

func lockSyncSourceTx(ctx context.Context, tx *loggedTx, sourceID int64) error {
	result, err := tx.ExecContext(
		ctx, `UPDATE sources SET updated_at = updated_at WHERE id = ?`, sourceID,
	)
	if err != nil {
		return fmt.Errorf("lock source %d for sync generation: %w", sourceID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("lock source %d for sync generation: inspect update: %w", sourceID, err)
	}
	if rows != 1 {
		return fmt.Errorf("lock source %d for sync generation: source not found", sourceID)
	}
	return nil
}

// FailSync marks a sync as failed with an error message.
func (s *Store) FailSync(syncID int64, errMsg string) error {
	_, err := s.db.Exec(fmt.Sprintf(`
		UPDATE sync_runs
		SET status = 'failed',
		    completed_at = %s,
		    error_message = ?
		WHERE id = ?
	`, s.dialect.Now()), errMsg, syncID)
	return s.finalizeSyncExecution(syncID, err)
}

// FailSyncAndClearSourceCursorContext atomically rejects one expired cursor
// and fails the still-current sync generation that observed it.
func (s *Store) FailSyncAndClearSourceCursorContext(
	ctx context.Context, syncID, sourceID int64, errMsg string,
) error {
	transitionStore := s.withoutSyncScope()
	err := transitionStore.withTxContext(ctx, func(tx *loggedTx) error {
		if err := validateCurrentSyncGeneration(
			ctx, tx, sourceID, syncID, SyncStatusRunning,
		); err != nil {
			return fmt.Errorf("fail sync %d and clear source cursor: %w", syncID, err)
		}

		now := s.dialect.Now()
		result, err := tx.ExecContext(ctx, fmt.Sprintf(`
			UPDATE sources
			SET sync_cursor = '', last_sync_at = %s, updated_at = %s
			WHERE id = ?
		`, now, now), sourceID)
		if err != nil {
			return fmt.Errorf("fail sync %d and clear source cursor: %w", syncID, err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("fail sync %d and clear source cursor: inspect source update: %w", syncID, err)
		}
		if rows != 1 {
			return fmt.Errorf("fail sync %d and clear source cursor: source %d not found", syncID, sourceID)
		}

		result, err = tx.ExecContext(ctx, fmt.Sprintf(`
			UPDATE sync_runs
			SET status = 'failed', completed_at = %s, error_message = ?
			WHERE id = ? AND source_id = ? AND status = 'running'
		`, now), errMsg, syncID, sourceID)
		if err != nil {
			return fmt.Errorf("fail sync %d and clear source cursor: %w", syncID, err)
		}
		rows, err = result.RowsAffected()
		if err != nil {
			return fmt.Errorf("fail sync %d and clear source cursor: inspect sync update: %w", syncID, err)
		}
		if rows != 1 {
			return fmt.Errorf("fail sync %d and clear source cursor: %w", syncID, ErrSyncRunSuperseded)
		}
		return nil
	})
	return transitionStore.finalizeSyncExecution(syncID, err)
}

// FailSyncWithCheckpoint marks a sync failed while preserving its last
// in-memory progress counters in the same statement. Importers use this when a
// prior checkpoint UPDATE itself failed, so the terminal row still records how
// much work and how many item errors occurred when failure finalization remains
// reachable.
func (s *Store) FailSyncWithCheckpoint(syncID int64, errMsg string, cp *Checkpoint) error {
	if cp == nil {
		return s.FailSync(syncID, errMsg)
	}
	_, err := s.db.Exec(fmt.Sprintf(`
		UPDATE sync_runs
		SET status = 'failed',
		    completed_at = %s,
		    error_message = ?,
		    cursor_before = ?,
		    messages_processed = ?,
		    messages_added = ?,
		    messages_updated = ?,
		    errors_count = ?
		WHERE id = ?
	`, s.dialect.Now()), errMsg, cp.PageToken, cp.MessagesProcessed,
		cp.MessagesAdded, cp.MessagesUpdated, cp.ErrorsCount, syncID)
	return s.finalizeSyncExecution(syncID, err)
}

// GetActiveSync returns the most recent running sync for a source, if any.
func (s *Store) GetActiveSync(sourceID int64) (*SyncRun, error) {
	run, err := s.getActiveSync(sourceID)
	if err != nil {
		return nil, err
	}
	if s.readOnly {
		return run, nil
	}
	recovered, err := s.recoverSyncSourceIfUnowned(context.Background(), sourceID)
	if err != nil {
		return nil, fmt.Errorf("recover active sync for source %d: %w", sourceID, err)
	}
	if !recovered {
		return run, nil
	}
	return s.getActiveSync(sourceID)
}

func (s *Store) getActiveSync(sourceID int64) (*SyncRun, error) {
	row := s.db.QueryRow(`
		SELECT id, source_id, started_at, completed_at, status,
		       messages_processed, messages_added, messages_updated, errors_count,
		       error_message, cursor_before, cursor_after, request_fingerprint
		FROM sync_runs
		WHERE source_id = ? AND status = 'running'
		ORDER BY started_at DESC, id DESC
		LIMIT 1
	`, sourceID)

	run, err := scanSyncRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("active sync for source %d: %w", sourceID, ErrSyncRunNotFound)
	}
	return run, err
}

// GetLatestSync returns the most recent sync run for a source, if any.
func (s *Store) GetLatestSync(sourceID int64) (*SyncRun, error) {
	row := s.db.QueryRow(`
		SELECT id, source_id, started_at, completed_at, status,
		       messages_processed, messages_added, messages_updated, errors_count,
		       error_message, cursor_before, cursor_after, request_fingerprint
		FROM sync_runs
		WHERE source_id = ?
		ORDER BY started_at DESC, id DESC
		LIMIT 1
	`, sourceID)

	run, err := scanSyncRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("latest sync for source %d: %w", sourceID, ErrSyncRunNotFound)
	}
	return run, err
}

// GetLatestCheckpointedSync returns the newest running or failed checkpoint
// since the source's last completed run. A newer uncheckpointed interruption
// does not hide recoverable state, while completion remains authoritative and
// makes every preceding checkpoint stale.
func (s *Store) GetLatestCheckpointedSync(sourceID int64) (*SyncRun, error) {
	row := s.db.QueryRow(`
		SELECT id, source_id, started_at, completed_at, status,
		       messages_processed, messages_added, messages_updated, errors_count,
		       error_message, cursor_before, cursor_after, request_fingerprint
		FROM sync_runs sr
		WHERE sr.source_id = ?
		  AND status IN ('running', 'failed')
		  AND cursor_before IS NOT NULL AND cursor_before != ''
		  AND id > COALESCE((
		    SELECT MAX(completed.id)
		    FROM sync_runs completed
		    WHERE completed.source_id = ? AND completed.status = 'completed'
		  ), 0)
		ORDER BY id DESC
		LIMIT 1
	`, sourceID, sourceID)

	run, err := scanSyncRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("latest checkpointed sync for source %d: %w", sourceID, ErrSyncRunNotFound)
	}
	return run, err
}

// GetLatestCheckpointedSyncByType returns the newest recoverable checkpoint
// for one sync type since that type's last completion. Checkpoints created by
// another importer for the same source are never considered resumable state.
func (s *Store) GetLatestCheckpointedSyncByType(sourceID int64, syncType string) (*SyncRun, error) {
	row := s.db.QueryRow(`
		SELECT id, source_id, started_at, completed_at, status,
		       messages_processed, messages_added, messages_updated, errors_count,
		       error_message, cursor_before, cursor_after, request_fingerprint
		FROM sync_runs sr
		WHERE sr.source_id = ?
		  AND sr.sync_type = ?
		  AND status IN ('running', 'failed')
		  AND cursor_before IS NOT NULL AND cursor_before != ''
		  AND id > COALESCE((
		    SELECT MAX(completed.id)
		    FROM sync_runs completed
		    WHERE completed.source_id = ?
		      AND completed.sync_type = ?
		      AND completed.status = 'completed'
		  ), 0)
		ORDER BY id DESC
		LIMIT 1
	`, sourceID, syncType, sourceID, syncType)

	run, err := scanSyncRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf(
			"latest checkpointed %s sync for source %d: %w",
			syncType, sourceID, ErrSyncRunNotFound,
		)
	}
	return run, err
}

func (s *Store) UpsertSourceImportItem(item SourceImportItem) error {
	err := s.withSyncSourceWriteContext(context.Background(), item.SourceID, func(q querier) error {
		_, err := q.Exec(fmt.Sprintf(`
		INSERT INTO source_import_items (
			source_id, provider, provider_id, name, checksum, size, modified_at,
			imported_at, status, records_imported, error_message, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, %s, %s)
		ON CONFLICT(source_id, provider, provider_id) DO UPDATE SET
			name = excluded.name,
			checksum = excluded.checksum,
			size = excluded.size,
			modified_at = excluded.modified_at,
			imported_at = excluded.imported_at,
			status = excluded.status,
			records_imported = excluded.records_imported,
			error_message = excluded.error_message,
			updated_at = %s
		`, s.dialect.Now(), s.dialect.Now(), s.dialect.Now()),
			item.SourceID, item.Provider, item.ProviderID, item.Name, item.Checksum,
			item.Size, item.ModifiedAt, item.ImportedAt, item.Status,
			item.RecordsImported, item.ErrorMessage)
		return err
	})
	if err != nil {
		return fmt.Errorf("upsert source import item: %w", err)
	}
	return nil
}

func (s *Store) GetSourceImportItem(sourceID int64, provider, providerID string) (*SourceImportItem, error) {
	var item SourceImportItem
	var checksum sql.NullString
	err := s.db.QueryRow(`
		SELECT id, source_id, provider, provider_id, name, checksum, size,
		       modified_at, imported_at, status, records_imported, error_message
		FROM source_import_items
		WHERE source_id = ? AND provider = ? AND provider_id = ?
	`, sourceID, provider, providerID).Scan(
		&item.ID, &item.SourceID, &item.Provider, &item.ProviderID, &item.Name,
		&checksum, &item.Size, &item.ModifiedAt, &item.ImportedAt,
		&item.Status, &item.RecordsImported, &item.ErrorMessage,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("source import item %s/%s for source %d: %w", provider, providerID, sourceID, ErrSourceImportItemNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get source import item: %w", err)
	}
	item.Checksum = checksum.String
	return &item, nil
}

func (s *Store) ListImportedSourceItemChecksums(sourceID int64, provider string) (map[string]string, error) {
	rows, err := s.db.Query(`
		SELECT provider_id, checksum
		FROM source_import_items
		WHERE source_id = ? AND provider = ? AND status = 'imported'
	`, sourceID, provider)
	if err != nil {
		return nil, fmt.Errorf("list imported source import items: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var providerID string
		var checksum sql.NullString
		if err := rows.Scan(&providerID, &checksum); err != nil {
			return nil, fmt.Errorf("scan source import item checksum: %w", err)
		}
		out[providerID] = checksum.String
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate source import item checksums: %w", err)
	}
	return out, nil
}

// HasAnyActiveSync returns true if any source currently has a running sync.
// Use this as a safety gate before performing destructive file operations that
// could race with concurrent attachment ingestion.
func (s *Store) HasAnyActiveSync() (bool, error) {
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM sync_runs WHERE status = 'running'`,
	).Scan(&count)
	if err != nil {
		return true, err // fail safe
	}
	return count > 0, nil
}

// GetLastSuccessfulSync returns the most recent successful sync for a source.
func (s *Store) GetLastSuccessfulSync(sourceID int64) (*SyncRun, error) {
	row := s.db.QueryRow(`
		SELECT id, source_id, started_at, completed_at, status,
		       messages_processed, messages_added, messages_updated, errors_count,
		       error_message, cursor_before, cursor_after, request_fingerprint
		FROM sync_runs
		WHERE source_id = ? AND status = 'completed'
		ORDER BY completed_at DESC, id DESC
		LIMIT 1
	`, sourceID)

	run, err := scanSyncRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("last successful sync for source %d: %w", sourceID, ErrSyncRunNotFound)
	}
	return run, err
}

// Source represents a Gmail account or other message source.
type Source struct {
	ID           int64
	SourceType   string // "gmail" or "imap"
	Identifier   string // email address or IMAP identifier URL
	DisplayName  sql.NullString
	GoogleUserID sql.NullString
	LastSyncAt   sql.NullTime
	SyncCursor   sql.NullString // historyId for Gmail
	SyncConfig   sql.NullString // JSON config for IMAP sources
	OAuthApp     sql.NullString // named OAuth app binding (NULL = default)
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// GetOrCreateSource gets or creates a source by type and identifier.
// Concurrent first-inserts converge via INSERT ... ON CONFLICT DO UPDATE
// RETURNING: the no-op SET fires RETURNING on both the insert and
// conflict path so the second caller receives the existing row's
// fields instead of a unique-violation error.
func (s *Store) GetOrCreateSource(sourceType, identifier string) (*Source, error) {
	now := s.dialect.Now()
	row := s.db.QueryRow(fmt.Sprintf(`
		INSERT INTO sources (source_type, identifier, created_at, updated_at)
		VALUES (?, ?, %s, %s)
		ON CONFLICT (source_type, identifier) DO UPDATE
		SET identifier = sources.identifier
		RETURNING id, source_type, identifier, display_name, google_user_id,
		          last_sync_at, sync_cursor, sync_config, oauth_app,
		          created_at, updated_at
	`, now, now), sourceType, identifier)

	source, err := scanSource(row)
	if err != nil {
		return nil, fmt.Errorf("upsert source: %w", err)
	}

	// Add to the default "All" collection if it exists.
	//
	// This runs as a separate Exec rather than inside a transaction
	// with the source insert. If this Exec fails, the source row is
	// committed but the All membership is missing — and the next
	// EnsureDefaultCollection call (which runs in InitSchema on every
	// process launch) re-adds every source not yet linked. Self-heals
	// on next CLI invocation; until then collection-scoped reads of
	// All would miss this source. Acceptable for a single-user tool;
	// a future refactor can fold this into a withTx.
	if _, err := s.db.Exec(
		s.dialect.InsertOrIgnore(
			`INSERT OR IGNORE INTO collection_sources (collection_id, source_id)
			 SELECT id, ? FROM collections WHERE name = ?`,
		),
		source.ID, DefaultCollectionName,
	); err != nil {
		slog.Warn("failed to add source to default collection (self-heals on next InitSchema)",
			sourceIDColumnName, source.ID,
			"identifier", identifier,
			"error", err,
		)
	}

	return source, nil
}

// UpdateSourceSyncCursor updates the sync cursor (historyId) for a source.
func (s *Store) UpdateSourceSyncCursor(sourceID int64, cursor string) error {
	now := s.dialect.Now()
	_, err := s.db.Exec(fmt.Sprintf(`
		UPDATE sources
		SET sync_cursor = ?, last_sync_at = %s, updated_at = %s
		WHERE id = ?
	`, now, now), cursor, sourceID)
	return err
}

// TouchSourceLastSyncAt records that a source-level sync completed even when
// the adapter does not maintain a cursor.
func (s *Store) TouchSourceLastSyncAt(sourceID int64) error {
	now := s.dialect.Now()
	return s.withSyncSourceWriteContext(context.Background(), sourceID, func(q querier) error {
		_, err := q.Exec(fmt.Sprintf(`
			UPDATE sources
			SET last_sync_at = %s, updated_at = %s
			WHERE id = ?
		`, now, now), sourceID)
		return err
	})
}

// ListSources returns all sources, optionally filtered by source type.
// Pass an empty string to return all sources.
func (s *Store) ListSources(sourceType string) ([]*Source, error) {
	return s.ListSourcesContext(context.Background(), sourceType)
}

// ListSourcesContext is the request-aware form of ListSources.
func (s *Store) ListSourcesContext(ctx context.Context, sourceType string) ([]*Source, error) {
	var rows *loggedRows
	var err error

	if sourceType != "" {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, source_type, identifier, display_name, google_user_id,
			       last_sync_at, sync_cursor, sync_config, oauth_app,
			       created_at, updated_at
			FROM sources
			WHERE source_type = ?
			ORDER BY identifier
		`, sourceType)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, source_type, identifier, display_name, google_user_id,
			       last_sync_at, sync_cursor, sync_config, oauth_app,
			       created_at, updated_at
			FROM sources
			ORDER BY identifier
		`)
	}
	if err != nil {
		return nil, fmt.Errorf("list sources: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var sources []*Source
	for rows.Next() {
		src, err := scanSource(rows)
		if err != nil {
			return nil, fmt.Errorf("scan source: %w", err)
		}
		sources = append(sources, src)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sources: %w", err)
	}

	return sources, nil
}

// UpdateSourceDisplayName updates the display name for a source.
func (s *Store) UpdateSourceDisplayName(sourceID int64, displayName string) error {
	return s.UpdateSourceDisplayNameContext(context.Background(), sourceID, displayName)
}

// UpdateSourceDisplayNameContext is the request-aware form of
// UpdateSourceDisplayName.
func (s *Store) UpdateSourceDisplayNameContext(
	ctx context.Context,
	sourceID int64,
	displayName string,
) error {
	return s.withSyncSourceWriteContext(ctx, sourceID, func(q querier) error {
		_, err := q.Exec(fmt.Sprintf(`
			UPDATE sources
			SET display_name = ?, updated_at = %s
			WHERE id = ?
		`, s.dialect.Now()), displayName, sourceID)
		return err
	})
}

// UpdateSourceSyncConfig updates the JSON sync configuration for an IMAP source.
// The sync_config column is JSONB on PG; the dialect supplies the
// appropriate placeholder cast (?::JSONB on PG, bare ? on SQLite).
func (s *Store) UpdateSourceSyncConfig(sourceID int64, configJSON string) error {
	_, err := s.db.Exec(fmt.Sprintf(`
		UPDATE sources
		SET sync_config = %s, updated_at = %s
		WHERE id = ?
	`, s.dialect.JSONBindExpr(), s.dialect.Now()), configJSON, sourceID)
	return err
}

// UpdateSourceIdentifier updates the identifier column for an existing source.
// Used by add-o365 to fix up the IMAP host when re-authorizing an account
// whose host classification changed (e.g. personal vs org scope correction).
func (s *Store) UpdateSourceIdentifier(sourceID int64, identifier string) error {
	_, err := s.db.Exec(fmt.Sprintf(`
		UPDATE sources
		SET identifier = ?, updated_at = %s
		WHERE id = ?
	`, s.dialect.Now()), identifier, sourceID)
	return err
}

// GetSourceByIdentifier returns a source by its identifier (email address).
func (s *Store) GetSourceByIdentifier(identifier string) (*Source, error) {
	row := s.db.QueryRow(`
		SELECT id, source_type, identifier, display_name, google_user_id,
		       last_sync_at, sync_cursor, sync_config, oauth_app,
		       created_at, updated_at
		FROM sources
		WHERE identifier = ?
	`, identifier)

	source, err := scanSource(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("source %q: %w", identifier, ErrSourceNotFound)
	}
	if err != nil {
		return nil, err
	}

	return source, nil
}
