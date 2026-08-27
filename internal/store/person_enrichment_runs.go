package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/personenrichment"
)

var (
	ErrRunNotTerminal               = errors.New("person enrichment run still owns nonterminal work")
	ErrManualRunIdempotencyConflict = errors.New("manual person enrichment idempotency key belongs to another target")
)

// PersonEnrichmentRun is one bounded, idempotent execution scope with counts
// derived from its durable attempts.
type PersonEnrichmentRun struct {
	personenrichment.DurableRun

	StartedAt             *time.Time `json:"started_at,omitempty"`
	CompletedAt           *time.Time `json:"completed_at,omitempty"`
	RequestedCount        int64      `json:"requested_count"`
	StartedCount          int64      `json:"started_count"`
	SucceededCount        int64      `json:"succeeded_count"`
	FailedCount           int64      `json:"failed_count"`
	SuppressedCount       int64      `json:"suppressed_count"`
	IdentityRejectedCount int64      `json:"identity_rejected_count"`
	FailureClass          *string    `json:"failure_class,omitempty"`
	SafeError             *string    `json:"safe_error,omitempty"`
}

func (s *Store) StartRun(
	ctx context.Context, input personenrichment.RunStart,
) (*personenrichment.DurableRun, bool, error) {
	if input.Kind != "scheduled" && input.Kind != "manual" {
		return nil, false, fmt.Errorf("invalid person enrichment run kind %q", input.Kind)
	}
	input.RequestedBy = strings.TrimSpace(input.RequestedBy)
	if input.RequestedBy == "" || input.RequestedAt.IsZero() {
		return nil, false, errors.New("person enrichment run requires requested_by and requested_at")
	}
	input.RequestedAt = input.RequestedAt.UTC()
	var run *personenrichment.DurableRun
	var created bool
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		var err error
		run, created, err = s.startPersonEnrichmentRunTx(ctx, tx, input)
		return err
	})
	return run, created, err
}

// StartManualPersonEnrichmentRunContext atomically validates exact consent
// and tracked-person eligibility, persists/reuses the manual run, then binds
// the requested work to that RunID before any worker can claim it.
func (s *Store) StartManualPersonEnrichmentRunContext(
	ctx context.Context,
	personID int64,
	profileFingerprint string,
	idempotencyKey string,
	requestedAt time.Time,
) (*personenrichment.DurableRun, bool, error) {
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if personID <= 0 || !validLowerSHA256(profileFingerprint) || idempotencyKey == "" || requestedAt.IsZero() {
		return nil, false, errors.New("manual person enrichment run input is invalid")
	}
	requestedAt = requestedAt.UTC()
	var run *personenrichment.DurableRun
	var created bool
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if s.personEnrichmentTxBarrier != nil {
			s.personEnrichmentTxBarrier("manual_before_person_lock")
		}
		if err := s.lockPersonEnrichmentAuthorityMutationTx(ctx, tx); err != nil {
			return err
		}
		if _, err := lockPersonEnrichmentPersonTx(ctx, tx, s.dialect, personID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrPersonNotFound
			}
			return err
		}
		if s.personEnrichmentTxBarrier != nil {
			s.personEnrichmentTxBarrier("manual_person_locked")
		}
		var authorized bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM person_tracking pt
			JOIN person_enrichment_consents c
			  ON c.profile_fingerprint = ? AND c.revoked_at IS NULL
			WHERE pt.person_id = ?)`, profileFingerprint, personID).Scan(&authorized); err != nil {
			return fmt.Errorf("authorize manual person enrichment run: %w", err)
		}
		if !authorized {
			return errors.New("manual person enrichment run requires a tracked person and active exact consent")
		}
		var err error
		run, created, err = s.startPersonEnrichmentRunTx(ctx, tx, personenrichment.RunStart{
			Kind: "manual", RequestedBy: idempotencyKey, RequestedAt: requestedAt,
		})
		if err != nil {
			return err
		}
		if err := s.bindManualPersonEnrichmentRunTargetTx(
			ctx, tx, run.ID, personID, profileFingerprint, created); err != nil {
			run = nil
			created = false
			return err
		}
		if run.State != "running" {
			return nil
		}
		var existingRun sql.NullInt64
		var leaseOwner sql.NullString
		err = tx.QueryRowContext(ctx, `SELECT run_id, lease_owner
			FROM person_enrichment_work WHERE person_id = ? AND profile_fingerprint = ?`+
			s.dialect.SelectForUpdate(), personID, profileFingerprint).Scan(&existingRun, &leaseOwner)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			mask, maskErr := personEnrichmentTriggerMask(personenrichment.TriggerManual)
			if maskErr != nil {
				return maskErr
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO person_enrichment_work
				(person_id, profile_fingerprint, trigger_mask, trigger_generation, due_at, run_id)
				VALUES (?, ?, ?, ?, ?, ?)`, personID, profileFingerprint, mask,
				"manual:"+idempotencyKey, requestedAt, run.ID)
			return err
		case err != nil:
			return fmt.Errorf("lock manual person enrichment work: %w", err)
		case leaseOwner.Valid && (!existingRun.Valid || existingRun.Int64 != run.ID):
			return errors.New("person enrichment work is already leased by another run")
		case existingRun.Valid && existingRun.Int64 != run.ID:
			return errors.New("person enrichment work belongs to another run")
		}
		if existingRun.Valid && existingRun.Int64 == run.ID {
			return nil
		}
		if err := putPersonEnrichmentWorkWithExecer(ctx, tx, EnrichmentTriggerInput{
			PersonID: personID, ProfileFingerprint: profileFingerprint,
			Kind: personenrichment.TriggerManual, Generation: "manual:" + idempotencyKey,
			DueAt: requestedAt,
		}); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE person_enrichment_work SET run_id = ?
			WHERE person_id = ? AND profile_fingerprint = ?
			  AND run_id IS NULL AND lease_owner IS NULL`, run.ID, personID, profileFingerprint)
		if err != nil {
			return fmt.Errorf("bind manual person enrichment work to run: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil || rows != 1 {
			return errors.New("manual person enrichment work could not be bound to its run")
		}
		return nil
	})
	return run, created, err
}

func (s *Store) bindManualPersonEnrichmentRunTargetTx(
	ctx context.Context,
	tx *loggedTx,
	runID, personID int64,
	profileFingerprint string,
	created bool,
) error {
	if created {
		if _, err := tx.ExecContext(ctx, `INSERT INTO person_enrichment_manual_run_targets
			(run_id, person_id, profile_fingerprint) VALUES (?, ?, ?)`,
			runID, personID, profileFingerprint); err != nil {
			return fmt.Errorf("bind manual person enrichment run target: %w", err)
		}
		return nil
	}
	var boundPersonID int64
	var boundFingerprint string
	err := tx.QueryRowContext(ctx, `SELECT person_id, profile_fingerprint
		FROM person_enrichment_manual_run_targets WHERE run_id = ?`+
		s.dialect.SelectForUpdate(), runID).Scan(&boundPersonID, &boundFingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrManualRunIdempotencyConflict
	}
	if err != nil {
		return fmt.Errorf("load manual person enrichment run target: %w", err)
	}
	if boundPersonID != personID || boundFingerprint != profileFingerprint {
		return ErrManualRunIdempotencyConflict
	}
	return nil
}

func (s *Store) startPersonEnrichmentRunTx(
	ctx context.Context, tx *loggedTx, input personenrichment.RunStart,
) (*personenrichment.DurableRun, bool, error) {
	result, err := tx.ExecContext(ctx, `
			INSERT INTO person_enrichment_runs
				(kind, requested_by, requested_at, started_at, state)
			VALUES (?, ?, ?, ?, 'running')
			ON CONFLICT (kind, requested_by) DO NOTHING`,
		input.Kind, input.RequestedBy, input.RequestedAt, input.RequestedAt)
	if err != nil {
		return nil, false, fmt.Errorf("start person enrichment run: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return nil, false, fmt.Errorf("read person enrichment run insert result: %w", err)
	}
	created := rows == 1
	if !created {
		if _, err := tx.ExecContext(ctx, `
				UPDATE person_enrichment_runs
				SET state = 'running', started_at = COALESCE(started_at, ?)
				WHERE kind = ? AND requested_by = ? AND state = 'queued'`,
			input.RequestedAt, input.Kind, input.RequestedBy); err != nil {
			return nil, false, fmt.Errorf("transition queued person enrichment run: %w", err)
		}
	}
	run, err := scanDurableRun(tx.QueryRowContext(ctx, `
			SELECT id, kind, requested_by, state, requested_at
			FROM person_enrichment_runs WHERE kind = ? AND requested_by = ?`,
		input.Kind, input.RequestedBy))
	if err != nil {
		return nil, false, fmt.Errorf("read person enrichment run: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
			INSERT INTO person_enrichment_run_counters (run_id) VALUES (?)
			ON CONFLICT (run_id) DO NOTHING`, run.ID)
	if err != nil {
		return nil, false, fmt.Errorf("initialize person enrichment run counter: %w", err)
	}
	return run, created, nil
}

func (s *Store) ListRunningRuns(
	ctx context.Context, filter personenrichment.RunningRunFilter,
) ([]personenrichment.DurableRun, error) {
	hasTimeCursor := !filter.AfterRequestedAt.IsZero()
	hasIDCursor := filter.AfterID != 0
	if filter.AfterID < 0 || hasTimeCursor != hasIDCursor ||
		filter.Limit < 1 || filter.Limit > 200 {
		return nil, errors.New("running run filter requires a complete chronological cursor and limit from 1 to 200")
	}
	after := filter.AfterRequestedAt.UTC()
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, kind, requested_by, state, requested_at
		FROM person_enrichment_runs
		WHERE state = 'running'
		  AND (? = 0 OR requested_at > ? OR (requested_at = ? AND id > ?))
		ORDER BY requested_at ASC, id ASC LIMIT ?`, filter.AfterID, after, after,
		filter.AfterID, filter.Limit)
	if err != nil {
		return nil, fmt.Errorf("list running person enrichment runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	runs := make([]personenrichment.DurableRun, 0)
	for rows.Next() {
		run, err := scanDurableRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scan running person enrichment run: %w", err)
		}
		runs = append(runs, *run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate running person enrichment runs: %w", err)
	}
	return runs, nil
}

func (s *Store) GetPersonEnrichmentRunContext(
	ctx context.Context, runID int64,
) (*PersonEnrichmentRun, error) {
	if runID <= 0 {
		return nil, errors.New("person enrichment run ID must be positive")
	}
	var (
		run                    PersonEnrichmentRun
		requestedAt, startedAt nullableTimestamp
		completedAt            nullableTimestamp
		failureClass, safeErr  sql.NullString
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT id, kind, requested_by, state, requested_at, started_at, completed_at,
		       requested_count, started_count, succeeded_count, failed_count,
		       suppressed_count, identity_rejected_count, failure_class, safe_error
		FROM person_enrichment_runs WHERE id = ?`, runID).Scan(
		&run.ID, &run.Kind, &run.RequestedBy, &run.State, &requestedAt, &startedAt, &completedAt,
		&run.RequestedCount, &run.StartedCount, &run.SucceededCount, &run.FailedCount,
		&run.SuppressedCount, &run.IdentityRejectedCount, &failureClass, &safeErr)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, fmt.Errorf("get person enrichment run: %w", err)
	}
	if !requestedAt.Valid {
		return nil, errors.New("person enrichment run has invalid requested_at")
	}
	run.RequestedAt = requestedAt.Time
	run.StartedAt = optionalTimestamp(startedAt)
	run.CompletedAt = optionalTimestamp(completedAt)
	if failureClass.Valid {
		value := failureClass.String
		run.FailureClass = &value
	}
	if safeErr.Valid {
		value := safeErr.String
		run.SafeError = &value
	}
	return &run, nil
}

func (s *Store) CompleteRun(
	ctx context.Context, runID int64, completion personenrichment.RunCompletion,
) error {
	if completion.State != "" && completion.State != "succeeded" &&
		completion.State != "partial" && completion.State != "failed" {
		return errors.New("person enrichment run completion is invalid")
	}
	if completion.CompletedAt.IsZero() {
		completion.CompletedAt = s.personEnrichmentTime()
	}
	var failureClass, safeMessage any
	if completion.Failure != nil {
		if err := validateSafeFailure(*completion.Failure); err != nil {
			return err
		}
		failureClass = string(completion.Failure.Class)
		safeMessage = safeFailureMessage(completion.Failure.Message)
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if s.personEnrichmentRunBarrier != nil {
			s.personEnrichmentRunBarrier("complete_before_run_lock")
		}
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM person_enrichment_runs WHERE id = ?`+
			s.dialect.SelectForUpdate(), runID).Scan(&state); err != nil {
			return fmt.Errorf("lock person enrichment run: %w", err)
		}
		if state != "running" {
			return fmt.Errorf("person enrichment run is %q, not running", state)
		}
		var nonterminal int64
		if err := tx.QueryRowContext(ctx, `
			SELECT (SELECT COUNT(*) FROM person_enrichment_work WHERE run_id = ?) +
			       (SELECT COUNT(*) FROM person_enrichment_attempts
			        WHERE run_id = ? AND state IN ('queued','starting','pending','retry_wait'))`,
			runID, runID).Scan(&nonterminal); err != nil {
			return fmt.Errorf("check person enrichment run terminal state: %w", err)
		}
		if nonterminal != 0 {
			return ErrRunNotTerminal
		}
		var requested, started, succeeded, failed, suppressed, rejected int64
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*), COUNT(*),
			       COALESCE(SUM(CASE WHEN state = 'succeeded' THEN 1 ELSE 0 END), 0),
			       COALESCE(SUM(CASE WHEN state IN ('terminal','uncertain_start') THEN 1 ELSE 0 END), 0),
			       COALESCE(SUM(CASE WHEN state = 'suppressed' THEN 1 ELSE 0 END), 0),
			       COALESCE(SUM(CASE WHEN state = 'identity_rejected' THEN 1 ELSE 0 END), 0)
			FROM person_enrichment_attempts WHERE run_id = ?`, runID).Scan(
			&requested, &started, &succeeded, &failed, &suppressed, &rejected); err != nil {
			return fmt.Errorf("derive person enrichment run counts: %w", err)
		}
		if completion.State == "" {
			// Truthful terminal state: every started attempt failed -> failed;
			// failures mixed with any other terminal outcome -> partial;
			// otherwise (successes and/or policy outcomes only) -> succeeded.
			switch {
			case failed == 0:
				completion.State = "succeeded"
			case started > 0 && failed == started:
				completion.State = "failed"
			default:
				completion.State = "partial"
			}
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE person_enrichment_runs
			SET state = ?, completed_at = ?, requested_count = ?, started_count = ?,
			    succeeded_count = ?, failed_count = ?, suppressed_count = ?,
			    identity_rejected_count = ?, failure_class = ?, safe_error = ?
			WHERE id = ? AND state = 'running'`, completion.State, completion.CompletedAt.UTC(),
			requested, started, succeeded, failed, suppressed, rejected,
			failureClass, safeMessage, runID)
		if err != nil {
			return fmt.Errorf("complete person enrichment run: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil || rows != 1 {
			return ErrRunNotTerminal
		}
		return nil
	})
}

func scanDurableRun(row scanner) (*personenrichment.DurableRun, error) {
	var run personenrichment.DurableRun
	var requestedAt nullableTimestamp
	if err := row.Scan(&run.ID, &run.Kind, &run.RequestedBy, &run.State, &requestedAt); err != nil {
		return nil, err
	}
	if !requestedAt.Valid {
		return nil, errors.New("person enrichment run has invalid requested_at")
	}
	run.RequestedAt = requestedAt.Time
	return &run, nil
}
