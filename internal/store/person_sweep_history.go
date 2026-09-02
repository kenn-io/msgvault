package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/peoplesweep"
)

func (s *Store) ListPersonSweepRuns(
	ctx context.Context, filter peoplesweep.RunFilter,
) ([]peoplesweep.RunSummary, error) {
	if filter.Limit < 1 || filter.Limit > 200 || filter.PersonID < 0 ||
		(filter.ProviderFingerprint != "" && !validLowerSHA256(filter.ProviderFingerprint)) {
		return nil, errors.New("list person sweep runs: limit must be 1-200 and person ID nonnegative")
	}
	query := `SELECT r.id, r.kind, r.mode, r.status, r.program_fingerprint,
	                 r.catalog_fingerprint, r.provider_fingerprint, r.attempt_count,
	                 r.success_count, r.failure_count, r.projected_write_count,
	                 r.actual_requests, r.actual_input_tokens, r.actual_output_tokens,
	                 r.actual_cost_micro_usd, r.started_at, r.completed_at
	          FROM person_sweep_runs r`
	conditions := make([]string, 0, 2)
	args := make([]any, 0, 3)
	if filter.PersonID > 0 {
		conditions = append(conditions, `EXISTS (SELECT 1 FROM person_sweep_attempts a
		                          WHERE a.run_id = r.id AND a.person_id = ?)`)
		args = append(args, filter.PersonID)
	}
	if filter.ProviderFingerprint != "" {
		conditions = append(conditions, "r.provider_fingerprint = ?")
		args = append(args, filter.ProviderFingerprint)
	}
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += ` ORDER BY r.started_at DESC, r.id DESC LIMIT ?`
	args = append(args, filter.Limit)
	rows, err := s.db.QueryContext(ctx, s.Rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("list person sweep runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	runs := make([]peoplesweep.RunSummary, 0)
	for rows.Next() {
		var run peoplesweep.RunSummary
		var started requiredTimestamp
		var completed nullableTimestamp
		if err := rows.Scan(&run.ID, &run.Kind, &run.Mode, &run.Status,
			&run.ProgramFingerprint, &run.CatalogFingerprint, &run.ProviderFingerprint,
			&run.Attempts, &run.Successes, &run.Failures, &run.ProjectedWrites,
			&run.Usage.Requests, &run.Usage.InputTokens, &run.Usage.OutputTokens,
			&run.Usage.EstimatedCostMicroUSD, &started, &completed); err != nil {
			return nil, fmt.Errorf("scan person sweep run: %w", err)
		}
		run.StartedAt = started.Time.UTC()
		if completed.Valid {
			value := completed.Time.UTC()
			run.CompletedAt = &value
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person sweep runs: %w", err)
	}
	return runs, nil
}

func (s *Store) ListPersonSweepAttempts(
	ctx context.Context, filter peoplesweep.AttemptFilter,
) ([]peoplesweep.AttemptSummary, error) {
	if filter.Limit < 1 || filter.Limit > 200 || filter.PersonID < 0 ||
		(filter.ProviderFingerprint != "" && !validLowerSHA256(filter.ProviderFingerprint)) {
		return nil, errors.New("list person sweep attempts: limit must be 1-200 and person ID nonnegative")
	}
	query := `SELECT id, run_id, person_id, status, failure_class,
	                 cursor_envelope_json, envelope_hash, program_fingerprint,
	                 catalog_fingerprint, provider_fingerprint, generation_id,
	                 generation_key, seed_count, context_count, claim_count,
	                 decision_count, projected_write_count, provider_request_id,
	                 request_count, input_tokens,
	                 output_tokens, estimated_cost_micro_usd, latency_milliseconds
	          FROM person_sweep_attempts`
	conditions := make([]string, 0, 3)
	args := make([]any, 0, 4)
	if strings.TrimSpace(filter.RunID) != "" {
		conditions = append(conditions, "run_id = ?")
		args = append(args, filter.RunID)
	}
	if filter.PersonID > 0 {
		conditions = append(conditions, "person_id = ?")
		args = append(args, filter.PersonID)
	}
	if filter.ProviderFingerprint != "" {
		conditions = append(conditions, "provider_fingerprint = ?")
		args = append(args, filter.ProviderFingerprint)
	}
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += ` ORDER BY started_at DESC, id DESC LIMIT ?`
	args = append(args, filter.Limit)
	rows, err := s.db.QueryContext(ctx, s.Rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("list person sweep attempts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	attempts := make([]peoplesweep.AttemptSummary, 0)
	for rows.Next() {
		var attempt peoplesweep.AttemptSummary
		var envelope string
		var generationID sql.NullInt64
		var latencyMS int64
		if err := rows.Scan(&attempt.ID, &attempt.RunID, &attempt.PersonID,
			&attempt.Status, &attempt.FailureClass, &envelope, &attempt.EnvelopeHash,
			&attempt.ProgramFingerprint, &attempt.CatalogFingerprint,
			&attempt.ProviderFingerprint, &generationID, &attempt.GenerationKey,
			&attempt.SeedCount, &attempt.ContextCount, &attempt.ClaimCount,
			&attempt.DecisionCount, &attempt.ProjectedWrites, &attempt.ProviderRequestID,
			&attempt.Usage.Requests,
			&attempt.Usage.InputTokens, &attempt.Usage.OutputTokens,
			&attempt.Usage.EstimatedCostMicroUSD, &latencyMS); err != nil {
			return nil, fmt.Errorf("scan person sweep attempt: %w", err)
		}
		if err := json.Unmarshal([]byte(envelope), &attempt.CursorEnvelope); err != nil {
			return nil, fmt.Errorf("decode person sweep cursor envelope: %w", err)
		}
		if generationID.Valid {
			value := generationID.Int64
			attempt.GenerationID = &value
		}
		if latencyMS < 0 || latencyMS > math.MaxInt64/int64(time.Millisecond) {
			return nil, errors.New("list person sweep attempts: invalid latency")
		}
		attempt.Latency = time.Duration(latencyMS) * time.Millisecond
		attempts = append(attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person sweep attempts: %w", err)
	}
	return attempts, nil
}

// PersonSweepOperationalStatus returns aggregate, redacted work state for the
// CLI. Raw evidence and provider payloads are not selected by these queries.
func (s *Store) PersonSweepOperationalStatus(
	ctx context.Context,
) (peoplesweep.OperationalStatus, error) {
	var status peoplesweep.OperationalStatus
	now := s.dialect.Now()
	query := fmt.Sprintf(`
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN lease_until > %s THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN available_at > %s
		                            AND (lease_until IS NULL OR lease_until <= %s)
		                         THEN 1 ELSE 0 END), 0),
		       MIN(created_at)
		FROM person_sweep_work`, now, now, now)
	var oldest nullableTimestamp
	if err := s.db.QueryRowContext(ctx, query).Scan(
		&status.DirtyCount, &status.LeasedCount, &status.RetryCount, &oldest,
	); err != nil {
		return peoplesweep.OperationalStatus{}, fmt.Errorf("read person sweep work status: %w", err)
	}
	if oldest.Valid {
		value := oldest.Time.UTC()
		status.OldestDirtyAt = &value
	}
	if err := s.db.QueryRowContext(ctx, `
		SELECT sequence FROM person_sweep_change_clock WHERE singleton = TRUE`,
	).Scan(&status.JournalHighWater); err != nil {
		return peoplesweep.OperationalStatus{}, fmt.Errorf("read person sweep journal high water: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(optimistic_sequence), 0) FROM person_sweep_cursors`,
	).Scan(&status.CursorHighWater); err != nil {
		return peoplesweep.OperationalStatus{}, fmt.Errorf("read person sweep cursor high water: %w", err)
	}
	var attemptFailure, workFailure string
	var attemptFailedAt, workFailedAt requiredTimestamp
	attemptErr := s.db.QueryRowContext(ctx, `
		SELECT failure_class, COALESCE(completed_at, started_at)
		FROM person_sweep_attempts
		WHERE failure_class <> ''
		ORDER BY COALESCE(completed_at, started_at) DESC, id DESC LIMIT 1`,
	).Scan(&attemptFailure, &attemptFailedAt)
	if attemptErr != nil && !errors.Is(attemptErr, sql.ErrNoRows) {
		return peoplesweep.OperationalStatus{}, fmt.Errorf(
			"read person sweep last attempt failure: %w", attemptErr)
	}
	workErr := s.db.QueryRowContext(ctx, `
		SELECT last_failure_class, updated_at FROM person_sweep_work
		WHERE last_failure_class <> ''
		ORDER BY updated_at DESC, person_id DESC LIMIT 1`,
	).Scan(&workFailure, &workFailedAt)
	if workErr != nil && !errors.Is(workErr, sql.ErrNoRows) {
		return peoplesweep.OperationalStatus{}, fmt.Errorf(
			"read person sweep last work failure: %w", workErr)
	}
	switch {
	case errors.Is(attemptErr, sql.ErrNoRows):
		status.LastFailure = peoplesweep.FailureClass(workFailure)
	case errors.Is(workErr, sql.ErrNoRows):
		status.LastFailure = peoplesweep.FailureClass(attemptFailure)
	case workFailedAt.Time.Before(attemptFailedAt.Time):
		status.LastFailure = peoplesweep.FailureClass(attemptFailure)
	default:
		status.LastFailure = peoplesweep.FailureClass(workFailure)
	}
	return status, nil
}
