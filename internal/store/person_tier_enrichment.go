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

// A manual run's kind alone cannot grant authority to coalesced scheduled work.
// Only the immutable target matching this exact work row grants the exception.
func personEnrichmentExactManualTargetSQL(workAlias string) string {
	return `EXISTS (
  SELECT 1 FROM person_enrichment_manual_run_targets manual_target
  JOIN person_enrichment_runs manual_run ON manual_run.id = manual_target.run_id
  WHERE manual_target.run_id = ` + workAlias + `.run_id
    AND manual_target.person_id = ` + workAlias + `.person_id
    AND manual_target.profile_fingerprint = ` + workAlias + `.profile_fingerprint
    AND manual_run.state IN ('queued', 'running'))`
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
	err := tx.QueryRowContext(ctx, `SELECT CAST(w.run_id AS TEXT)
  FROM person_enrichment_work w WHERE w.person_id = ? AND `+
		personEnrichmentExactManualTargetSQL("w")+` ORDER BY w.run_id LIMIT 1`, personID).Scan(&runID)
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

	preservedAttempt := `EXISTS (SELECT 1 FROM person_enrichment_work w
  WHERE w.person_id = a.person_id AND w.profile_fingerprint = a.profile_fingerprint
    AND w.run_id = a.run_id AND ` + personEnrichmentExactManualTargetSQL("w") + `)`
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
  WHERE a.person_id = ? AND a.state IN ('queued','starting','pending','retry_wait','uncertain_start')
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
  WHERE w.person_id = ? AND NOT `+personEnrichmentExactManualTargetSQL("w"), personID); err != nil {
		return fmt.Errorf("delete scheduled person enrichment work: %w", err)
	}
	// A fresh scheduled publication can replace a leased manual mask and
	// generation. The immutable exact target and its active attempt remain
	// authoritative for the manual bit and generation.
	if _, err := tx.ExecContext(ctx, `UPDATE person_enrichment_work AS w
  SET trigger_mask = 16,
      trigger_generation = COALESCE(
        (SELECT a.trigger_generation FROM person_enrichment_attempts a
         WHERE a.id = w.active_attempt_id AND a.run_id = w.run_id
           AND a.trigger_kind = 'manual'),
        w.trigger_generation),
      has_fresh_trigger = FALSE
  WHERE w.person_id = ? AND `+personEnrichmentExactManualTargetSQL("w"), personID); err != nil {
		return fmt.Errorf("retain manual person enrichment trigger: %w", err)
	}
	return nil
}
