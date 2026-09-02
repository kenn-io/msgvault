package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/peoplesweep"
)

const maxPersonSweepGapScan = 1_000

var personSweepSourceLanes = map[peoplesweep.SourceClass]struct{}{
	peoplesweep.SourceConversationText:  {},
	peoplesweep.SourceMeetingText:       {},
	peoplesweep.SourceAttachmentCaption: {},
	peoplesweep.SourceAttachmentOCR:     {},
	peoplesweep.SourceDocumentText:      {},
}

// ClaimPersonSweep atomically transfers one available work row to a worker.
// Retry availability and lease expiry both use the database clock.
func (s *Store) ClaimPersonSweep(
	ctx context.Context, request peoplesweep.ClaimRequest,
) (*peoplesweep.Lease, error) {
	if strings.TrimSpace(request.WorkerID) == "" {
		return nil, errors.New("claim person sweep: worker ID is required")
	}
	if request.LeaseDuration <= 0 {
		return nil, errors.New("claim person sweep: lease duration must be positive")
	}
	if request.PersonID < 0 {
		return nil, errors.New("claim person sweep: person ID must not be negative")
	}

	now := s.dialect.Now()
	leaseUntil, leaseDurationArg := personSweepLeaseExpiration(
		s.IsPostgreSQL(), request.LeaseDuration)
	candidateSuffix := ""
	if s.IsPostgreSQL() {
		candidateSuffix = " FOR UPDATE OF w SKIP LOCKED"
	}
	query := fmt.Sprintf(`
		UPDATE person_sweep_work
		SET lease_owner = ?, lease_until = %s, lease_fence = lease_fence + 1,
		    updated_at = %s
		WHERE person_id = (
			SELECT w.person_id
			FROM person_sweep_work w
			JOIN person_tracking pt ON pt.person_id = w.person_id
			WHERE available_at <= %s
			  AND (w.lease_until IS NULL OR w.lease_until <= %s)
			  AND (? = 0 OR w.person_id = ?)
			ORDER BY w.available_at, w.person_id
			LIMIT 1%s
		)
		RETURNING person_id, lease_owner, lease_fence, lease_until, attempt_count`,
		leaseUntil, now, now, now, candidateSuffix)
	var (
		lease     peoplesweep.Lease
		expiresAt requiredTimestamp
	)
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		err := tx.QueryRowContext(ctx, query, request.WorkerID,
			leaseDurationArg, request.PersonID, request.PersonID).Scan(
			&lease.PersonID, &lease.WorkerID, &lease.Fence, &expiresAt, &lease.AttemptCount)
		if err != nil {
			return err
		}
		return s.finalizeReclaimedPersonSweepAttempts(ctx, tx, lease.PersonID, lease.Fence)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // No due work is a successful empty claim.
	}
	if err != nil {
		return nil, fmt.Errorf("claim person sweep: %w", err)
	}
	lease.ExpiresAt = expiresAt.Time.UTC()
	return &lease, nil
}

func (s *Store) finalizeReclaimedPersonSweepAttempts(
	ctx context.Context, tx *loggedTx, personID, currentFence int64,
) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, run_id FROM person_sweep_attempts
		WHERE person_id = ? AND status = 'running' AND lease_fence < ?
		ORDER BY id`, personID, currentFence)
	if err != nil {
		return fmt.Errorf("list abandoned person sweep attempts: %w", err)
	}
	type abandonedAttempt struct{ id, runID string }
	attempts := make([]abandonedAttempt, 0)
	for rows.Next() {
		var attempt abandonedAttempt
		if err := rows.Scan(&attempt.id, &attempt.runID); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan abandoned person sweep attempt: %w", err)
		}
		attempts = append(attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate abandoned person sweep attempts: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close abandoned person sweep attempts: %w", err)
	}
	for _, attempt := range attempts {
		if err := s.finalizeReclaimedPersonSweepAttempt(ctx, tx, attempt.id, attempt.runID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) finalizeReclaimedPersonSweepAttempt(
	ctx context.Context, tx *loggedTx, attemptID, runID string,
) error {
	rows, err := tx.QueryContext(ctx, `SELECT batch_ordinal, call_ordinal, purpose, utc_day, status,
		reserved_requests, reserved_input_tokens, reserved_output_tokens, reserved_cost_micro_usd
		FROM person_sweep_batches
		WHERE attempt_id = ?
		ORDER BY batch_ordinal, call_ordinal`, attemptID)
	if err != nil {
		return fmt.Errorf("list abandoned person sweep batches: %w", err)
	}
	type abandonedBatch struct {
		ordinal, callOrdinal int
		purpose              string
		day, status          string
		reserved             peoplesweep.Usage
	}
	batches := make([]abandonedBatch, 0)
	for rows.Next() {
		var batch abandonedBatch
		if err := rows.Scan(&batch.ordinal, &batch.callOrdinal, &batch.purpose,
			&batch.day, &batch.status,
			&batch.reserved.Requests, &batch.reserved.InputTokens,
			&batch.reserved.OutputTokens, &batch.reserved.EstimatedCostMicroUSD); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan abandoned person sweep batch: %w", err)
		}
		batches = append(batches, batch)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate abandoned person sweep batches: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close abandoned person sweep batches: %w", err)
	}
	coordinates := make([]personSweepBatchCoordinate, 0, len(batches))
	for _, batch := range batches {
		coordinates = append(coordinates, personSweepBatchCoordinate{ordinal: batch.ordinal,
			callOrdinal: batch.callOrdinal, purpose: batch.purpose, day: batch.day})
	}
	if err := validatePersonSweepCallCoordinateSet(coordinates); err != nil {
		return fmt.Errorf("finalize abandoned person sweep attempt: %w", err)
	}
	for _, batch := range batches {
		if batch.status != "reserved" && batch.status != "running" {
			continue
		}
		actual := peoplesweep.Usage{}
		status := "cancelled"
		if batch.status == "running" {
			status = "failed"
			actual = batch.reserved
		}
		_, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE person_sweep_batches SET
			status = ?, actual_requests = ?, actual_input_tokens = ?, actual_output_tokens = ?,
			actual_cost_micro_usd = ?, failure_class = ?, completed_at = %s
			WHERE attempt_id = ? AND batch_ordinal = ? AND call_ordinal = ? AND status = ?`,
			s.dialect.Now()),
			status, actual.Requests, actual.InputTokens, actual.OutputTokens,
			actual.EstimatedCostMicroUSD, peoplesweep.FailureLeaseLost,
			attemptID, batch.ordinal, batch.callOrdinal, batch.status)
		if err != nil {
			return fmt.Errorf("finalize abandoned person sweep batch: %w", err)
		}
		if err := s.adjustPersonSweepDailyUsage(ctx, tx, batch.day,
			negatePersonSweepUsage(batch.reserved), actual); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE person_sweep_attempts SET
		status = 'failed', failure_class = ?, completed_at = %s
		WHERE id = ? AND status = 'running'`, s.dialect.Now()),
		peoplesweep.FailureLeaseLost, attemptID); err != nil {
		return fmt.Errorf("finalize abandoned person sweep attempt: %w", err)
	}
	if err := s.refreshPersonSweepAttemptAndRunUsage(ctx, tx, attemptID, runID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE person_sweep_runs SET
		status = CASE
			WHEN success_count > 0 AND failure_count > 0 THEN 'partial'
			WHEN success_count > 0 THEN 'succeeded'
			ELSE 'failed' END,
		completed_at = %s
		WHERE id = ? AND status = 'running'
		  AND NOT EXISTS (SELECT 1 FROM person_sweep_attempts
		                  WHERE run_id = ? AND status = 'running')`, s.dialect.Now()), runID, runID)
	if err != nil {
		return fmt.Errorf("finalize abandoned person sweep run: %w", err)
	}
	return nil
}

func personSweepLeaseExpiration(postgres bool, duration time.Duration) (string, any) {
	if postgres {
		return "NOW() + (? * INTERVAL '1 microsecond')", duration.Microseconds()
	}
	return "strftime('%Y-%m-%d %H:%M:%f', 'now', '+' || ? || ' seconds')", duration.Seconds()
}

// RenewPersonSweep extends an unexpired lease only when owner and fence both
// still match the current work row.
func (s *Store) RenewPersonSweep(
	ctx context.Context, lease peoplesweep.Lease, duration time.Duration,
) (*peoplesweep.Lease, error) {
	if duration <= 0 {
		return nil, errors.New("renew person sweep: lease duration must be positive")
	}
	leaseUntil, leaseDurationArg := personSweepLeaseExpiration(s.IsPostgreSQL(), duration)
	query := fmt.Sprintf(`
		UPDATE person_sweep_work
		SET lease_until = %s, updated_at = %s
		WHERE person_id = ? AND lease_owner = ? AND lease_fence = ?
		  AND lease_until > %s
		RETURNING person_id, lease_owner, lease_fence, lease_until, attempt_count`,
		leaseUntil, s.dialect.Now(), s.dialect.Now())
	var (
		renewed   peoplesweep.Lease
		expiresAt requiredTimestamp
	)
	err := s.db.QueryRowContext(ctx, s.Rebind(query), leaseDurationArg,
		lease.PersonID, lease.WorkerID, lease.Fence).Scan(
		&renewed.PersonID, &renewed.WorkerID, &renewed.Fence, &expiresAt, &renewed.AttemptCount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, peoplesweep.ErrLeaseLost
	}
	if err != nil {
		return nil, fmt.Errorf("renew person sweep: %w", err)
	}
	renewed.ExpiresAt = expiresAt.Time.UTC()
	return &renewed, nil
}

// FailPersonSweepWork releases exactly one fenced lease and schedules its
// retry. Inference usage is intentionally not recorded by this operation.
func (s *Store) FailPersonSweepWork(
	ctx context.Context, failure peoplesweep.WorkFailure,
) error {
	if failure.RetryAt.IsZero() {
		return errors.New("fail person sweep work: retry time is required")
	}
	result, err := s.db.ExecContext(ctx, s.Rebind(fmt.Sprintf(`
		UPDATE person_sweep_work
		SET available_at = ?, attempt_count = attempt_count + 1,
		    last_failure_class = ?, lease_owner = '', lease_until = NULL,
		    updated_at = %s
		WHERE person_id = ? AND lease_owner = ? AND lease_fence = ?
		  AND lease_until > %s`, s.dialect.Now(), s.dialect.Now())),
		s.dialect.TimestampParam(failure.RetryAt), failure.Class,
		failure.Lease.PersonID, failure.Lease.WorkerID, failure.Lease.Fence)
	if err != nil {
		return fmt.Errorf("fail person sweep work: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("fail person sweep work rows affected: %w", err)
	}
	if changed != 1 {
		return peoplesweep.ErrLeaseLost
	}
	return nil
}

// EnsurePersonSweepCursors creates missing fingerprinted lanes while holding
// the journal clock. The initial optimistic high water and deterministic
// source upper key therefore describe one transactionally consistent cut.
func (s *Store) EnsurePersonSweepCursors(
	ctx context.Context, keys []peoplesweep.CursorKey,
) ([]peoplesweep.Cursor, error) {
	if len(keys) == 0 {
		return []peoplesweep.Cursor{}, nil
	}
	var cursors []peoplesweep.Cursor
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		var err error
		cursors, _, err = s.ensurePersonSweepCursorsTx(ctx, tx, keys)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("ensure person sweep cursors: %w", err)
	}
	return cursors, nil
}

func (s *Store) ensurePersonSweepCursorsTx(
	ctx context.Context, tx *loggedTx, keys []peoplesweep.CursorKey,
) ([]peoplesweep.Cursor, map[peoplesweep.CursorKey]bool, error) {
	for _, key := range keys {
		if err := validatePersonSweepCursorKey(key); err != nil {
			return nil, nil, err
		}
	}
	// The no-op update is the portable lock: SQLite takes its singleton writer
	// slot and PostgreSQL takes the clock row lock used by every journal append.
	if _, err := tx.ExecContext(ctx, `
		UPDATE person_sweep_change_clock SET sequence = sequence WHERE singleton = TRUE`); err != nil {
		return nil, nil, fmt.Errorf("lock person sweep change clock: %w", err)
	}
	var highWater int64
	if err := tx.QueryRowContext(ctx, `
		SELECT sequence FROM person_sweep_change_clock WHERE singleton = TRUE`).Scan(&highWater); err != nil {
		return nil, nil, fmt.Errorf("capture person sweep high water: %w", err)
	}

	cursors := make([]peoplesweep.Cursor, 0, len(keys))
	created := make(map[peoplesweep.CursorKey]bool, len(keys))
	for _, key := range keys {
		upper, err := s.personSweepSourceUpperKeyTx(ctx, tx, key.SourceLane)
		if err != nil {
			return nil, nil, err
		}
		result, err := tx.ExecContext(ctx, s.Rebind(fmt.Sprintf(`
			INSERT INTO person_sweep_cursors
				(person_id, source_lane, program_fingerprint, catalog_fingerprint,
				 optimistic_sequence, reconcile_upper_key, reconcile_after_key,
				 reconciliation_complete, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, '', ?, %s, %s)
			ON CONFLICT (person_id, source_lane, program_fingerprint, catalog_fingerprint)
			DO NOTHING`, s.dialect.Now(), s.dialect.Now())),
			key.PersonID, key.SourceLane, key.ProgramFingerprint,
			key.CatalogFingerprint, highWater, upper, upper == "")
		if err != nil {
			return nil, nil, fmt.Errorf("insert person sweep cursor: %w", err)
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return nil, nil, fmt.Errorf("person sweep cursor rows affected: %w", err)
		}
		created[key] = inserted == 1
		if _, err := tx.ExecContext(ctx, s.Rebind(fmt.Sprintf(`
			UPDATE person_sweep_cursors
			SET reconciliation_complete = TRUE, updated_at = %s
			WHERE person_id = ? AND source_lane = ? AND program_fingerprint = ?
			  AND catalog_fingerprint = ? AND reconciliation_complete = FALSE
			  AND reconcile_after_key = reconcile_upper_key`, s.dialect.Now())),
			key.PersonID, key.SourceLane, key.ProgramFingerprint,
			key.CatalogFingerprint); err != nil {
			return nil, nil, fmt.Errorf("complete empty person sweep cursor: %w", err)
		}
		cursor, err := s.loadPersonSweepCursorTx(ctx, tx, key)
		if err != nil {
			return nil, nil, err
		}
		cursors = append(cursors, cursor)
	}
	return cursors, created, nil
}

func validatePersonSweepCursorKey(key peoplesweep.CursorKey) error {
	if key.PersonID <= 0 || strings.TrimSpace(key.ProgramFingerprint) == "" ||
		strings.TrimSpace(key.CatalogFingerprint) == "" {
		return errors.New("person sweep cursor requires person and fingerprints")
	}
	if _, ok := personSweepSourceLanes[key.SourceLane]; !ok {
		return fmt.Errorf("person sweep cursor has unknown source lane %q", key.SourceLane)
	}
	return nil
}

func (s *Store) personSweepSourceUpperKeyTx(
	ctx context.Context, tx *loggedTx, lane peoplesweep.SourceClass,
) (string, error) {
	var query string
	switch lane {
	case peoplesweep.SourceConversationText:
		query = `SELECT COALESCE(MAX(id), 0) FROM messages
			WHERE deleted_at IS NULL AND deleted_from_source_at IS NULL
			  AND message_type <> 'meeting_transcript'`
	case peoplesweep.SourceMeetingText:
		query = `SELECT COALESCE(MAX(id), 0) FROM messages
			WHERE deleted_at IS NULL AND deleted_from_source_at IS NULL
			  AND message_type = 'meeting_transcript'`
	case peoplesweep.SourceAttachmentCaption, peoplesweep.SourceAttachmentOCR:
		query = `SELECT COALESCE(MAX(a.id), 0) FROM attachments a
			WHERE EXISTS (SELECT 1 FROM messages m WHERE m.id = a.message_id
			  AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL)`
	case peoplesweep.SourceDocumentText:
		query = `SELECT COALESCE(MAX(a.id), 0) FROM attachments a
			WHERE LOWER(COALESCE(a.media_type, '')) = 'document'
			  AND EXISTS (SELECT 1 FROM messages m WHERE m.id = a.message_id
			  AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL)`
	default:
		return "", fmt.Errorf("capture source upper key: unknown lane %q", lane)
	}
	var upper int64
	if err := tx.QueryRowContext(ctx, query).Scan(&upper); err != nil {
		return "", fmt.Errorf("capture %s upper key: %w", lane, err)
	}
	if upper == 0 {
		return "", nil
	}
	return fmt.Sprintf("%020d", upper), nil
}

func (s *Store) loadPersonSweepCursorTx(
	ctx context.Context, tx *loggedTx, key peoplesweep.CursorKey,
) (peoplesweep.Cursor, error) {
	cursor := peoplesweep.Cursor{Key: key}
	var lastBackstop nullableTimestamp
	err := tx.QueryRowContext(ctx, s.Rebind(`
		SELECT optimistic_sequence, optimistic_document_key,
		       reconcile_upper_key, reconcile_after_key, reconcile_document_key,
		       reconciliation_complete, backstop_upper_key, backstop_after_key, backstop_document_key,
		       last_backstop_at
		FROM person_sweep_cursors
		WHERE person_id = ? AND source_lane = ? AND program_fingerprint = ?
		  AND catalog_fingerprint = ?`), key.PersonID, key.SourceLane,
		key.ProgramFingerprint, key.CatalogFingerprint).Scan(
		&cursor.OptimisticSequence, &cursor.OptimisticDocumentKey, &cursor.ReconcileUpperKey,
		&cursor.ReconcileAfterKey, &cursor.ReconcileDocumentKey, &cursor.ReconciliationComplete,
		&cursor.BackstopUpperKey, &cursor.BackstopAfterKey, &cursor.BackstopDocumentKey, &lastBackstop)
	if err != nil {
		return peoplesweep.Cursor{}, fmt.Errorf("load person sweep cursor: %w", err)
	}
	if lastBackstop.Valid {
		value := lastBackstop.Time.UTC()
		cursor.LastBackstopAt = &value
	}
	return cursor, nil
}

// AdvancePersonSweepReconciliation compare-and-sets one optimistic or bounded
// reconciliation cursor under the exact active work lease.
func (s *Store) AdvancePersonSweepReconciliation(
	ctx context.Context, lease peoplesweep.Lease, cursor peoplesweep.GenerationCursor,
) error {
	if err := validatePersonSweepCursorKey(cursor.Key); err != nil {
		return err
	}
	if lease.PersonID != cursor.Key.PersonID {
		return peoplesweep.ErrLeaseLost
	}
	now := s.dialect.Now()
	var (
		setClause string
		modeArgs  []any
	)
	switch cursor.Mode {
	case peoplesweep.GenerationCursorOptimistic:
		if cursor.CursorFrom < 0 || (cursor.CursorThrough < cursor.CursorFrom ||
			(cursor.CursorThrough == cursor.CursorFrom && cursor.DocumentToKey <= cursor.DocumentFromKey)) {
			return errors.New("advance person sweep optimistic cursor: invalid sequence range")
		}
		setClause = "optimistic_sequence = ?, optimistic_document_key = ?"
		modeArgs = []any{cursor.CursorThrough, cursor.DocumentToKey}
	case peoplesweep.GenerationCursorReconciliation:
		if !personSweepDocumentCoordinateAdvanced(cursor.ReconcileFromKey, cursor.DocumentFromKey,
			cursor.ReconcileToKey, cursor.DocumentToKey) {
			return errors.New("advance person sweep reconciliation cursor: invalid key range")
		}
		setClause = `reconcile_after_key = ?, reconcile_document_key = ?,
			reconciliation_complete = CASE WHEN ? >= reconcile_upper_key AND ? = '' THEN TRUE ELSE FALSE END`
		modeArgs = []any{cursor.ReconcileToKey, cursor.DocumentToKey,
			cursor.ReconcileToKey, cursor.DocumentToKey}
	default:
		return fmt.Errorf("advance person sweep cursor: unsupported mode %q", cursor.Mode)
	}
	query := fmt.Sprintf(`
		UPDATE person_sweep_cursors
		SET %s, updated_at = %s
		WHERE person_id = ? AND source_lane = ? AND program_fingerprint = ?
		  AND catalog_fingerprint = ?
		  AND %s
		  AND EXISTS (
			SELECT 1 FROM person_sweep_work w
			WHERE w.person_id = person_sweep_cursors.person_id
			  AND w.lease_owner = ? AND w.lease_fence = ? AND w.lease_until > %s
		  )`, setClause, now, personSweepCursorCASPredicate(cursor.Mode), now)
	args := append(slices.Clone(modeArgs), cursor.Key.PersonID, cursor.Key.SourceLane,
		cursor.Key.ProgramFingerprint, cursor.Key.CatalogFingerprint)
	if cursor.Mode == peoplesweep.GenerationCursorOptimistic {
		args = append(args, cursor.CursorFrom, cursor.DocumentFromKey)
	} else {
		args = append(args, cursor.ReconcileFromKey, cursor.DocumentFromKey, cursor.ReconcileToKey)
	}
	args = append(args, lease.WorkerID, lease.Fence)
	result, err := s.db.ExecContext(ctx, s.Rebind(query), args...)
	if err != nil {
		return fmt.Errorf("advance person sweep cursor: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("advance person sweep cursor rows affected: %w", err)
	}
	if changed == 1 {
		return nil
	}
	current, err := s.personSweepLeaseCurrent(ctx, lease)
	if err != nil {
		return err
	}
	if !current {
		return peoplesweep.ErrLeaseLost
	}
	return nil
}

func personSweepCursorCASPredicate(mode peoplesweep.GenerationCursorMode) string {
	if mode == peoplesweep.GenerationCursorOptimistic {
		return "optimistic_sequence = ? AND optimistic_document_key = ?"
	}
	return "reconcile_after_key = ? AND reconcile_document_key = ? AND ? <= reconcile_upper_key AND reconciliation_complete = FALSE"
}

func (s *Store) personSweepLeaseCurrent(
	ctx context.Context, lease peoplesweep.Lease,
) (bool, error) {
	var current bool
	err := s.db.QueryRowContext(ctx, s.Rebind(fmt.Sprintf(`
		SELECT EXISTS (
			SELECT 1 FROM person_sweep_work
			WHERE person_id = ? AND lease_owner = ? AND lease_fence = ?
			  AND lease_until > %s
		)`, s.dialect.Now())), lease.PersonID, lease.WorkerID, lease.Fence).Scan(&current)
	if err != nil {
		return false, fmt.Errorf("validate person sweep lease: %w", err)
	}
	return current, nil
}

// ReconcilePersonSweepWorkContext scans a bounded ascending page of tracked
// people and coalesces cursor gaps, journal gaps, due retries, and backstops
// into one durable work row per person.
func (s *Store) ReconcilePersonSweepWorkContext(
	ctx context.Context, request peoplesweep.GapRequest,
) (peoplesweep.GapResult, error) {
	if strings.TrimSpace(request.ProgramFingerprint) == "" ||
		strings.TrimSpace(request.CatalogFingerprint) == "" {
		return peoplesweep.GapResult{}, errors.New("reconcile person sweep work: fingerprints are required")
	}
	if len(request.SourceLanes) == 0 || request.Limit <= 0 {
		return peoplesweep.GapResult{}, nil
	}
	if request.Limit > maxPersonSweepGapScan {
		request.Limit = maxPersonSweepGapScan
	}
	for _, lane := range request.SourceLanes {
		if _, ok := personSweepSourceLanes[lane]; !ok {
			return peoplesweep.GapResult{}, fmt.Errorf(
				"reconcile person sweep work: unknown source lane %q", lane)
		}
	}
	if request.Now.IsZero() {
		request.Now = time.Now().UTC()
	}

	var result peoplesweep.GapResult
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		people, err := scanTrackedPersonIDs(ctx, tx, s, request.AfterPersonID, request.Limit)
		if err != nil {
			return err
		}
		result.PeopleScanned = len(people)
		if len(people) == 0 {
			return nil
		}
		result.NextPersonID = people[len(people)-1]

		keys := make([]peoplesweep.CursorKey, 0, len(people)*len(request.SourceLanes))
		for _, personID := range people {
			for _, lane := range request.SourceLanes {
				keys = append(keys, peoplesweep.CursorKey{
					PersonID: personID, SourceLane: lane,
					ProgramFingerprint: request.ProgramFingerprint,
					CatalogFingerprint: request.CatalogFingerprint,
				})
			}
		}
		cursors, created, err := s.ensurePersonSweepCursorsTx(ctx, tx, keys)
		if err != nil {
			return err
		}
		byPerson := make(map[int64][]peoplesweep.Cursor, len(people))
		for _, cursor := range cursors {
			byPerson[cursor.Key.PersonID] = append(byPerson[cursor.Key.PersonID], cursor)
		}

		for _, personID := range people {
			dirtyThrough, journalGap, err := s.personSweepJournalGapTx(
				ctx, tx, personID, byPerson[personID])
			if err != nil {
				return err
			}
			need := journalGap
			for _, cursor := range byPerson[personID] {
				if cursor.BackstopUpperKey != "" || request.ForceBackstop || created[cursor.Key] || !cursor.ReconciliationComplete ||
					personSweepBackstopDue(cursor.LastBackstopAt, request.Now, request.BackstopInterval) {
					need = true
				}
			}
			dueRetry, err := s.personSweepRetryDueTx(ctx, tx, personID)
			if err != nil {
				return err
			}
			need = need || dueRetry
			if !need {
				continue
			}
			if err := s.upsertPersonSweepWorkTxMode(
				ctx, tx, personID, dirtyThrough, request.ForceBackstop); err != nil {
				return err
			}
			result.WorkCreated++
		}
		return nil
	})
	if err != nil {
		return peoplesweep.GapResult{}, fmt.Errorf("reconcile person sweep work: %w", err)
	}
	return result, nil
}

func scanTrackedPersonIDs(
	ctx context.Context, tx *loggedTx, s *Store, after int64, limit int,
) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, s.Rebind(`
		SELECT person_id FROM person_tracking
		WHERE person_id > ? ORDER BY person_id LIMIT ?`), after, limit)
	if err != nil {
		return nil, fmt.Errorf("scan tracked people for sweep: %w", err)
	}
	defer func() { _ = rows.Close() }()
	people := make([]int64, 0, limit)
	for rows.Next() {
		var personID int64
		if err := rows.Scan(&personID); err != nil {
			return nil, fmt.Errorf("scan tracked person for sweep: %w", err)
		}
		people = append(people, personID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tracked people for sweep: %w", err)
	}
	return people, nil
}

func (s *Store) personSweepJournalGapTx(
	ctx context.Context, tx *loggedTx, personID int64, cursors []peoplesweep.Cursor,
) (int64, bool, error) {
	var dirtyThrough int64
	gap := false
	for _, cursor := range cursors {
		var laneHighWater int64
		err := tx.QueryRowContext(ctx, s.Rebind(`
			SELECT COALESCE(MAX(sequence), 0) FROM person_sweep_changes
			WHERE person_id = ? AND source_lane = ?`), personID, cursor.Key.SourceLane).Scan(&laneHighWater)
		if err != nil {
			return 0, false, fmt.Errorf("read person sweep lane high water: %w", err)
		}
		if laneHighWater > dirtyThrough {
			dirtyThrough = laneHighWater
		}
		if laneHighWater > cursor.OptimisticSequence {
			gap = true
		}
	}
	return dirtyThrough, gap, nil
}

func (s *Store) personSweepRetryDueTx(
	ctx context.Context, tx *loggedTx, personID int64,
) (bool, error) {
	var due bool
	err := tx.QueryRowContext(ctx, s.Rebind(fmt.Sprintf(`
		SELECT EXISTS (
			SELECT 1 FROM person_sweep_work
			WHERE person_id = ? AND available_at <= %s
			  AND (lease_until IS NULL OR lease_until <= %s)
		)`, s.dialect.Now(), s.dialect.Now())), personID).Scan(&due)
	if err != nil {
		return false, fmt.Errorf("read person sweep retry availability: %w", err)
	}
	return due, nil
}

func personSweepBackstopDue(last *time.Time, now time.Time, interval time.Duration) bool {
	if interval <= 0 {
		return false
	}
	return last == nil || !last.Add(interval).After(now)
}

func (s *Store) upsertPersonSweepWorkTx(
	ctx context.Context, tx *loggedTx, personID, dirtyThrough int64,
) error {
	return s.upsertPersonSweepWorkTxMode(ctx, tx, personID, dirtyThrough, false)
}

func (s *Store) upsertPersonSweepWorkTxMode(
	ctx context.Context, tx *loggedTx, personID, dirtyThrough int64, forceAvailable bool,
) error {
	lockSuffix := ""
	if s.IsPostgreSQL() {
		lockSuffix = " FOR KEY SHARE"
	}
	var trackedPersonID int64
	err := tx.QueryRowContext(ctx, s.Rebind(`
		SELECT person_id FROM person_tracking WHERE person_id = ?`+lockSuffix),
		personID).Scan(&trackedPersonID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lock person %d tracking for sweep publication: %w", personID, err)
	}
	maxExpr := "MAX(person_sweep_work.dirty_through_sequence, excluded.dirty_through_sequence)"
	if s.IsPostgreSQL() {
		maxExpr = "GREATEST(person_sweep_work.dirty_through_sequence, excluded.dirty_through_sequence)"
	}
	now := s.dialect.Now()
	_, err = tx.ExecContext(ctx, s.Rebind(fmt.Sprintf(`
		INSERT INTO person_sweep_work
			(person_id, dirty_through_sequence, available_at, attempt_count,
			 last_failure_class, lease_owner, lease_until, lease_fence,
			 created_at, updated_at)
		VALUES (?, ?, %s, 0, '', '', NULL, 0, %s, %s)
		ON CONFLICT (person_id) DO UPDATE SET
			dirty_through_sequence = %s,
			available_at = CASE
				WHEN ? THEN %s
				WHEN excluded.dirty_through_sequence > person_sweep_work.dirty_through_sequence
				THEN %s ELSE person_sweep_work.available_at END,
			updated_at = %s`, now, now, now, maxExpr, now, now, now)), personID, dirtyThrough, forceAvailable)
	if err != nil {
		return fmt.Errorf("upsert person sweep work: %w", err)
	}
	return nil
}
