package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/personenrichment"
)

// personEnrichmentScheduledAuthoritySQL reads applied authority only. Provider
// consent remains the responsibility of the publication or request boundary.
func personEnrichmentScheduledAuthoritySQL(personExpr string) string {
	return `EXISTS (
  SELECT 1 FROM person_tracking tier_tracking
  WHERE tier_tracking.person_id = ` + personExpr + `
    AND (tier_tracking.provenance = 'manual' OR EXISTS (
      SELECT 1 FROM person_tier_state tier_state
      WHERE tier_state.person_id = tier_tracking.person_id
        AND tier_state.enrichment_eligible))
    AND NOT EXISTS (
      SELECT 1 FROM person_tier_opt_outs tier_opt_out
      WHERE tier_opt_out.person_id = tier_tracking.person_id
        AND tier_opt_out.lane = 'enrichment'))`
}

// Scheduled work needs current tier authority. An exact open manual target is
// the only exception for a person whose scheduled authority is absent.
func personEnrichmentWorkAuthoritySQL() string {
	return `(` + personEnrichmentScheduledAuthoritySQL("w.person_id") + ` OR ` +
		personEnrichmentExactManualTargetSQL() + `)`
}

func personEnrichmentManualTargetForPersonSQL(personExpr, fingerprintExpr string) string {
	return `EXISTS (
  SELECT 1 FROM person_enrichment_manual_run_targets manual_target
  JOIN person_enrichment_runs manual_run ON manual_run.id = manual_target.run_id
  WHERE manual_target.person_id = ` + personExpr + `
    AND manual_target.profile_fingerprint = ` + fingerprintExpr + `
    AND manual_run.state IN ('queued', 'running'))`
}

func personEnrichmentManualTargetForRunSQL(personExpr, fingerprintExpr, runExpr string) string {
	return `EXISTS (
  SELECT 1 FROM person_enrichment_manual_run_targets manual_target
  JOIN person_enrichment_runs manual_run ON manual_run.id = manual_target.run_id
  WHERE manual_target.person_id = ` + personExpr + `
    AND manual_target.profile_fingerprint = ` + fingerprintExpr + `
    AND manual_target.run_id = ` + runExpr + `
    AND manual_run.state IN ('queued', 'running'))`
}

// A manual run's kind alone cannot grant authority to coalesced scheduled work.
// Only the immutable target matching this exact work row grants the exception.
func personEnrichmentExactManualTargetSQL() string {
	return personEnrichmentManualTargetForRunSQL("w.person_id", "w.profile_fingerprint", "w.run_id")
}

// An open manual target owns detached work until its requested run claims it.
// Scheduled authority applies only when no open target owns that person/profile.
func personEnrichmentClaimAuthoritySQL(personExpr, fingerprintExpr, runExpr string) string {
	return `(` + personEnrichmentManualTargetForRunSQL(personExpr, fingerprintExpr, runExpr) + ` OR (` +
		personEnrichmentScheduledAuthoritySQL(personExpr) + ` AND NOT ` +
		personEnrichmentManualTargetForPersonSQL(personExpr, fingerprintExpr) + `))`
}

func (s *Store) personEnrichmentScheduledEligibleTx(
	ctx context.Context, tx *loggedTx, personID int64,
) (bool, error) {
	var eligible bool
	if err := tx.QueryRowContext(ctx, `SELECT `+personEnrichmentScheduledAuthoritySQL("?"), personID).Scan(&eligible); err != nil {
		return false, fmt.Errorf("read scheduled person enrichment eligibility: %w", err)
	}
	return eligible, nil
}

func (s *Store) openPersonManualEnrichmentRunTx(
	ctx context.Context, tx *loggedTx, personID int64,
) (string, error) {
	var runID string
	err := tx.QueryRowContext(ctx, `SELECT CAST(manual_target.run_id AS TEXT)
  FROM person_enrichment_manual_run_targets manual_target
  JOIN person_enrichment_runs manual_run ON manual_run.id = manual_target.run_id
  WHERE manual_target.person_id = ? AND manual_run.state IN ('queued', 'running')
  ORDER BY manual_target.run_id LIMIT 1`, personID).Scan(&runID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read open manual person enrichment target: %w", err)
	}
	return runID, nil
}

// cancelScheduledPersonEnrichmentTx fences scheduled attempts while retaining
// the exact target a human requested. Callers hold the enrichment authority gate.
func (s *Store) cancelScheduledPersonEnrichmentTx(
	ctx context.Context, tx *loggedTx, personID int64,
) error {
	// Claim takes the work row lock before touching its attempt. Keep that order
	// so a concurrent claim cannot bind work after this cancellation's snapshot.
	rows, err := tx.QueryContext(ctx, `SELECT profile_fingerprint FROM person_enrichment_work
  WHERE person_id = ? ORDER BY profile_fingerprint`+s.dialect.SelectForUpdate(), personID)
	if err != nil {
		return fmt.Errorf("lock scheduled person enrichment work: %w", err)
	}
	for rows.Next() {
		var fingerprint string
		if err := rows.Scan(&fingerprint); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan scheduled person enrichment work: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate scheduled person enrichment work: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close scheduled person enrichment work: %w", err)
	}

	preservedAttempt := personEnrichmentManualTargetForRunSQL(
		"a.person_id", "a.profile_fingerprint", "a.run_id",
	)
	now := s.personEnrichmentTime()
	var dispatching bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
  SELECT 1 FROM person_enrichment_attempts a
  WHERE a.person_id = ? AND NOT `+preservedAttempt+`
    AND a.state IN ('starting','pending') AND a.dispatch_authorized_at IS NOT NULL
    AND a.lease_owner IS NOT NULL AND a.lease_until > ?)`, personID, now).Scan(&dispatching); err != nil {
		return fmt.Errorf("check scheduled person enrichment dispatch in progress: %w", err)
	}
	if dispatching {
		return ErrPersonEnrichmentDispatchInProgress
	}
	rows, err = tx.QueryContext(ctx, `SELECT a.id FROM person_enrichment_attempts a
	  WHERE a.person_id = ?
	    AND (a.state IN ('queued','starting','pending','retry_wait')
	         OR (a.state = 'uncertain_start' AND a.completed_at IS NULL))
    AND NOT `+preservedAttempt+` ORDER BY a.id`+s.dialect.SelectForUpdate(), personID)
	if err != nil {
		return fmt.Errorf("list canceled scheduled person enrichment attempts: %w", err)
	}
	var attemptIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan canceled scheduled person enrichment attempt: %w", err)
		}
		attemptIDs = append(attemptIDs, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate canceled scheduled person enrichment attempts: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close canceled scheduled person enrichment attempts: %w", err)
	}
	for _, id := range attemptIDs {
		// An unknown provider outcome still consumes its guaranteed reservation.
		if _, err := reconcilePersonEnrichmentCostTx(ctx, tx, s.dialect, id, personenrichment.Cost{}, true, now); err != nil {
			return fmt.Errorf("reconcile canceled scheduled person enrichment attempt: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE person_enrichment_attempts
   SET state = 'terminal', completed_at = ?, failure_class = ?,
       next_action_at = NULL, lease_owner = NULL, lease_until = NULL
   WHERE id = ?`, now, personenrichment.FailurePolicy, id); err != nil {
			return fmt.Errorf("cancel scheduled person enrichment attempt: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM person_enrichment_work AS w
  WHERE w.person_id = ? AND NOT `+personEnrichmentManualTargetForPersonSQL(
		"w.person_id", "w.profile_fingerprint",
	), personID); err != nil {
		return fmt.Errorf("delete scheduled person enrichment work: %w", err)
	}
	// A deferred manual target has no run ID on its work row. Reattach the exact
	// open target and restore its generation before the next worker claim.
	if _, err := tx.ExecContext(ctx, `UPDATE person_enrichment_work AS w
	  SET run_id = (
	        SELECT manual_target.run_id
	        FROM person_enrichment_manual_run_targets manual_target
	        JOIN person_enrichment_runs manual_run ON manual_run.id = manual_target.run_id
	        WHERE manual_target.person_id = w.person_id
	          AND manual_target.profile_fingerprint = w.profile_fingerprint
	          AND manual_run.state IN ('queued', 'running')
	        ORDER BY CASE WHEN manual_target.run_id = w.run_id THEN 0 ELSE 1 END,
	                 CASE WHEN 'manual:' || manual_run.requested_by = w.trigger_generation THEN 0 ELSE 1 END,
	                 manual_target.run_id
	        LIMIT 1),
	      trigger_mask = 16,
      trigger_generation = 'manual:' || (
        SELECT manual_run.requested_by
        FROM person_enrichment_manual_run_targets manual_target
        JOIN person_enrichment_runs manual_run ON manual_run.id = manual_target.run_id
        WHERE manual_target.person_id = w.person_id
          AND manual_target.profile_fingerprint = w.profile_fingerprint
          AND manual_run.state IN ('queued', 'running')
        ORDER BY CASE WHEN manual_target.run_id = w.run_id THEN 0 ELSE 1 END,
                 CASE WHEN 'manual:' || manual_run.requested_by = w.trigger_generation THEN 0 ELSE 1 END,
                 manual_target.run_id
        LIMIT 1),
      has_fresh_trigger = FALSE
  WHERE w.person_id = ? AND `+personEnrichmentManualTargetForPersonSQL(
		"w.person_id", "w.profile_fingerprint",
	), personID); err != nil {
		return fmt.Errorf("retain manual person enrichment trigger: %w", err)
	}
	return nil
}
