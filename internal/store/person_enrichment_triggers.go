package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/personenrichment"
)

// EnrichmentTriggerInput is one durable, coalescing publication for an exact
// person and immutable provider policy. Callers supply the generation owned by
// the mutation that made the work necessary.
type EnrichmentTriggerInput struct {
	PersonID           int64
	ProfileFingerprint string
	Kind               personenrichment.TriggerKind
	Generation         string
	DueAt              time.Time
}

// EnqueuePersonEnrichmentContext publishes work only while the person remains
// tracked and the exact provider policy still has active consent.
func (s *Store) EnqueuePersonEnrichmentContext(
	ctx context.Context, input EnrichmentTriggerInput,
) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		return s.enqueuePersonEnrichmentTx(ctx, tx, input)
	})
}

func (s *Store) enqueuePersonEnrichmentTx(
	ctx context.Context, tx *loggedTx, input EnrichmentTriggerInput,
) error {
	if err := validateEnrichmentTriggerInput(input); err != nil {
		return err
	}
	var authorized bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM person_tracking pt
			JOIN person_enrichment_consents c
			  ON c.profile_fingerprint = ? AND c.revoked_at IS NULL
			WHERE pt.person_id = ?
		)`, input.ProfileFingerprint, input.PersonID).Scan(&authorized); err != nil {
		return fmt.Errorf("authorize person enrichment trigger: %w", err)
	}
	if !authorized {
		return nil
	}
	return s.putPersonEnrichmentWorkTx(ctx, tx, input)
}

func validateEnrichmentTriggerInput(input EnrichmentTriggerInput) error {
	if input.PersonID <= 0 || !validLowerSHA256(input.ProfileFingerprint) ||
		strings.TrimSpace(input.Generation) == "" || input.DueAt.IsZero() {
		return errors.New("person enrichment trigger input is invalid")
	}
	if _, err := personEnrichmentTriggerMask(input.Kind); err != nil {
		return err
	}
	return nil
}

func (s *Store) putPersonEnrichmentWorkTx(
	ctx context.Context, tx *loggedTx, input EnrichmentTriggerInput,
) error {
	return putPersonEnrichmentWorkWithExecer(ctx, tx, input)
}

func putPersonEnrichmentWorkWithExecer(
	ctx context.Context, execer contextQuerier, input EnrichmentTriggerInput,
) error {
	mask, err := personEnrichmentTriggerMask(input.Kind)
	if err != nil {
		return err
	}
	_, err = execer.ExecContext(ctx, `
		INSERT INTO person_enrichment_work
			(person_id, profile_fingerprint, trigger_mask, trigger_generation, due_at)
		VALUES (?, ?, ?, ?, ?)`+personEnrichmentWorkConflictClause,
		input.PersonID, input.ProfileFingerprint, mask,
		strings.TrimSpace(input.Generation), input.DueAt.UTC())
	if err != nil {
		return fmt.Errorf("put person enrichment trigger: %w", err)
	}
	return nil
}

func (s *Store) publishPersonEnrichmentTx(
	ctx context.Context, tx *loggedTx, personID int64,
	kind personenrichment.TriggerKind, generation string, dueAt time.Time,
) error {
	mask, err := personEnrichmentTriggerMask(kind)
	if err != nil {
		return err
	}
	if personID <= 0 || strings.TrimSpace(generation) == "" || dueAt.IsZero() {
		return errors.New("person enrichment publication is invalid")
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO person_enrichment_work
			(person_id, profile_fingerprint, trigger_mask, trigger_generation, due_at)
		SELECT ?, c.profile_fingerprint, ?, ?, ?
		FROM person_enrichment_consents c
		JOIN person_tracking pt ON pt.person_id = ?
		WHERE c.revoked_at IS NULL`+personEnrichmentWorkConflictClause,
		personID, mask, strings.TrimSpace(generation), dueAt.UTC(), personID)
	if err != nil {
		return fmt.Errorf("publish authorized person enrichment work: %w", err)
	}
	return nil
}

func (s *Store) publishPersonIdentityEnrichmentTx(
	ctx context.Context, tx *loggedTx, personIDs ...int64,
) error {
	for _, personID := range sortedUniqueInt64s(personIDs...) {
		var revision int64
		if err := tx.QueryRowContext(ctx,
			`SELECT revision FROM persons WHERE id = ?`, personID).Scan(&revision); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return fmt.Errorf("read person enrichment identity generation: %w", err)
		}
		if err := s.publishPersonEnrichmentTx(ctx, tx, personID,
			personenrichment.TriggerIdentity, "revision:"+strconv.FormatInt(revision, 10),
			s.personEnrichmentTime()); err != nil {
			return err
		}
	}
	return nil
}

// EnqueueDuePersonEnrichmentContext repairs bounded missing work. Existing
// refresh rows keep their database-owned due times; the cron schedule only
// wakes this scan.
func (s *Store) EnqueueDuePersonEnrichmentContext(
	ctx context.Context, now time.Time, limit int,
) (int, error) {
	if now.IsZero() || limit < 1 || limit > 200 {
		return 0, errors.New("person enrichment catch-up requires a time and limit from 1 to 200")
	}
	now = now.UTC()
	count := 0
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		expiryMask, err := personEnrichmentTriggerMask(personenrichment.TriggerExpiry)
		if err != nil {
			return err
		}
		expiredRows, err := tx.QueryContext(ctx, `
			SELECT c.person_id, g.provider_policy_fingerprint, MAX(c.id)
			FROM person_fact_claims c
			JOIN person_fact_generations g ON g.id = c.generation_id
			JOIN person_tracking pt ON pt.person_id = c.person_id
			JOIN person_enrichment_consents consent
			  ON consent.profile_fingerprint = g.provider_policy_fingerprint
			 AND consent.revoked_at IS NULL
			LEFT JOIN person_enrichment_work w
			  ON w.person_id = c.person_id
			 AND w.profile_fingerprint = g.provider_policy_fingerprint
			WHERE c.origin = 'enrichment' AND c.valid_until IS NOT NULL
			  AND c.valid_until <= ?
			  AND NOT EXISTS (
				SELECT 1 FROM person_fact_generations newer
				WHERE newer.person_id = g.person_id
				  AND newer.provider_policy_fingerprint = g.provider_policy_fingerprint
				  AND newer.id > g.id
			  )
			  AND (w.person_id IS NULL OR (w.trigger_mask & ?) = 0 OR w.due_at > ?)
			GROUP BY c.person_id, g.provider_policy_fingerprint
			ORDER BY c.person_id, g.provider_policy_fingerprint
			LIMIT ?`, now, expiryMask, now, limit)
		if err != nil {
			return fmt.Errorf("list expired person enrichment claims: %w", err)
		}
		type expiredWork struct {
			personID    int64
			fingerprint string
			claimID     int64
		}
		expired := make([]expiredWork, 0, limit)
		for expiredRows.Next() {
			var item expiredWork
			if err := expiredRows.Scan(&item.personID, &item.fingerprint, &item.claimID); err != nil {
				_ = expiredRows.Close()
				return fmt.Errorf("scan expired person enrichment claim: %w", err)
			}
			expired = append(expired, item)
		}
		if err := expiredRows.Err(); err != nil {
			_ = expiredRows.Close()
			return fmt.Errorf("iterate expired person enrichment claims: %w", err)
		}
		if err := expiredRows.Close(); err != nil {
			return fmt.Errorf("close expired person enrichment claims: %w", err)
		}
		for _, item := range expired {
			if err := s.putPersonEnrichmentWorkTx(ctx, tx, EnrichmentTriggerInput{
				PersonID: item.personID, ProfileFingerprint: item.fingerprint,
				Kind:       personenrichment.TriggerExpiry,
				Generation: "claim:" + strconv.FormatInt(item.claimID, 10), DueAt: now,
			}); err != nil {
				return err
			}
			count++
		}
		if count == limit {
			return nil
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT pt.person_id, c.profile_fingerprint, p.revision
			FROM person_tracking pt
			JOIN persons p ON p.id = pt.person_id
			CROSS JOIN person_enrichment_consents c
			LEFT JOIN person_enrichment_work w
			  ON w.person_id = pt.person_id
			 AND w.profile_fingerprint = c.profile_fingerprint
			WHERE c.revoked_at IS NULL AND w.person_id IS NULL
			ORDER BY pt.person_id, c.profile_fingerprint
			LIMIT ?`, limit-count)
		if err != nil {
			return fmt.Errorf("list missing person enrichment work: %w", err)
		}
		type missingWork struct {
			personID    int64
			fingerprint string
			revision    int64
		}
		missing := make([]missingWork, 0, limit-count)
		for rows.Next() {
			var item missingWork
			if err := rows.Scan(&item.personID, &item.fingerprint, &item.revision); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan missing person enrichment work: %w", err)
			}
			missing = append(missing, item)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("iterate missing person enrichment work: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close missing person enrichment work: %w", err)
		}
		for _, item := range missing {
			if err := s.putPersonEnrichmentWorkTx(ctx, tx, EnrichmentTriggerInput{
				PersonID: item.personID, ProfileFingerprint: item.fingerprint,
				Kind:       personenrichment.TriggerTracked,
				Generation: "revision:" + strconv.FormatInt(item.revision, 10), DueAt: now,
			}); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	return count, err
}

func (s *Store) cancelPersonEnrichmentTx(
	ctx context.Context, tx *loggedTx, personID int64, fingerprint string,
) error {
	where := "person_id = ?"
	args := []any{personID}
	if fingerprint != "" {
		where += " AND profile_fingerprint = ?"
		args = append(args, fingerprint)
	}
	now := s.personEnrichmentTime()
	cancelableWhere := where + " AND (lease_owner IS NULL OR lease_until <= ?)"
	cancelableArgs := append(append([]any(nil), args...), now)
	attemptRows, err := tx.QueryContext(ctx, `SELECT id FROM person_enrichment_attempts
		WHERE state IN ('queued','starting','pending','retry_wait','uncertain_start') AND `+cancelableWhere,
		cancelableArgs...)
	if err != nil {
		return fmt.Errorf("list canceled person enrichment attempts: %w", err)
	}
	attemptIDs := make([]int64, 0)
	for attemptRows.Next() {
		var attemptID int64
		if err := attemptRows.Scan(&attemptID); err != nil {
			_ = attemptRows.Close()
			return fmt.Errorf("scan canceled person enrichment attempt: %w", err)
		}
		attemptIDs = append(attemptIDs, attemptID)
	}
	if err := attemptRows.Err(); err != nil {
		_ = attemptRows.Close()
		return fmt.Errorf("iterate canceled person enrichment attempts: %w", err)
	}
	if err := attemptRows.Close(); err != nil {
		return fmt.Errorf("close canceled person enrichment attempts: %w", err)
	}
	for _, attemptID := range attemptIDs {
		if _, err := reconcilePersonEnrichmentCostTx(
			ctx, tx, s.dialect, attemptID, personenrichment.Cost{}, true,
			now); err != nil {
			return fmt.Errorf("reconcile canceled person enrichment attempt: %w", err)
		}
	}
	terminalArgs := append([]any{now, string(personenrichment.FailurePolicy)}, cancelableArgs...)
	if _, err := tx.ExecContext(ctx, `UPDATE person_enrichment_attempts
		SET state = 'terminal', completed_at = ?, failure_class = ?,
		    next_action_at = NULL, lease_owner = NULL, lease_until = NULL
		WHERE state IN ('queued','starting','pending','retry_wait','uncertain_start') AND `+cancelableWhere,
		terminalArgs...); err != nil {
		return fmt.Errorf("cancel person enrichment attempts: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM person_enrichment_work WHERE `+cancelableWhere,
		cancelableArgs...); err != nil {
		return fmt.Errorf("cancel person enrichment work: %w", err)
	}
	return nil
}

func (s *Store) invalidatePersonEnrichmentIdentitiesTx(
	ctx context.Context, tx *loggedTx, personIDs ...int64,
) error {
	return s.invalidatePersonEnrichmentIdentitiesWithRevisionTx(
		ctx, tx, true, personIDs...)
}

func (s *Store) invalidatePersonEnrichmentIdentitiesAfterRevisionTx(
	ctx context.Context, tx *loggedTx, personIDs ...int64,
) error {
	return s.invalidatePersonEnrichmentIdentitiesWithRevisionTx(
		ctx, tx, false, personIDs...)
}

func (s *Store) invalidatePersonEnrichmentIdentitiesWithRevisionTx(
	ctx context.Context, tx *loggedTx, bumpAuthorized bool, personIDs ...int64,
) error {
	for _, personID := range sortedUniqueInt64s(personIDs...) {
		if s.personEnrichmentTxBarrier != nil {
			s.personEnrichmentTxBarrier("invalidation_before_person_lock")
		}
		if bumpAuthorized {
			authorized, err := s.personEnrichmentAuthorizedTx(ctx, tx, personID)
			if err != nil {
				return err
			}
			if authorized {
				if err := s.bumpPersonRevisionsTx(ctx, tx, personID); err != nil {
					return err
				}
			}
		}
		if s.personEnrichmentTxBarrier != nil {
			s.personEnrichmentTxBarrier("invalidation_person_locked")
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM person_enrichment_provider_identities WHERE person_id = ?`, personID); err != nil {
			return fmt.Errorf("invalidate person enrichment provider identities: %w", err)
		}
		if err := s.forceInvalidatePersonEnrichmentTx(ctx, tx, personID); err != nil {
			return err
		}
	}
	return s.publishPersonIdentityEnrichmentTx(ctx, tx, personIDs...)
}

func (s *Store) personEnrichmentAuthorizedTx(
	ctx context.Context, tx *loggedTx, personID int64,
) (bool, error) {
	var authorized bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM person_tracking tracked
		JOIN person_enrichment_consents consent ON consent.revoked_at IS NULL
		WHERE tracked.person_id = ?
	)`, personID).Scan(&authorized); err != nil {
		return false, fmt.Errorf("read person enrichment authorization: %w", err)
	}
	return authorized, nil
}

func (s *Store) bumpAuthorizedPersonEnrichmentRevisionsTx(
	ctx context.Context, tx *loggedTx, personIDs ...int64,
) error {
	authorizedIDs := make([]int64, 0, len(personIDs))
	for _, personID := range sortedUniqueInt64s(personIDs...) {
		authorized, err := s.personEnrichmentAuthorizedTx(ctx, tx, personID)
		if err != nil {
			return err
		}
		if authorized {
			authorizedIDs = append(authorizedIDs, personID)
		}
	}
	return s.bumpPersonRecordRevisionsTx(ctx, tx, authorizedIDs...)
}

// forceInvalidatePersonEnrichmentTx is reserved for mutations that change a
// person's identity boundary. Unlike consent/tracking cancellation, it fences
// actively leased attempts: their prepared results must never be applicable to
// the post-merge or post-split person.
func (s *Store) forceInvalidatePersonEnrichmentTx(
	ctx context.Context, tx *loggedTx, personID int64,
) error {
	now := s.personEnrichmentTime()
	workRows, err := tx.QueryContext(ctx, `SELECT profile_fingerprint
		FROM person_enrichment_work WHERE person_id = ?
		ORDER BY profile_fingerprint`+s.dialect.SelectForUpdate(), personID)
	if err != nil {
		return fmt.Errorf("lock force-invalidated person enrichment work: %w", err)
	}
	for workRows.Next() {
		var fingerprint string
		if err := workRows.Scan(&fingerprint); err != nil {
			_ = workRows.Close()
			return fmt.Errorf("scan force-invalidated person enrichment work: %w", err)
		}
	}
	if err := workRows.Err(); err != nil {
		_ = workRows.Close()
		return fmt.Errorf("iterate force-invalidated person enrichment work: %w", err)
	}
	if err := workRows.Close(); err != nil {
		return fmt.Errorf("close force-invalidated person enrichment work: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM person_enrichment_attempts
		WHERE person_id = ?
		  AND state IN ('queued','starting','pending','retry_wait','uncertain_start')
		ORDER BY id`+s.dialect.SelectForUpdate(), personID)
	if err != nil {
		return fmt.Errorf("list force-invalidated person enrichment attempts: %w", err)
	}
	attemptIDs := make([]int64, 0)
	for rows.Next() {
		var attemptID int64
		if err := rows.Scan(&attemptID); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan force-invalidated person enrichment attempt: %w", err)
		}
		attemptIDs = append(attemptIDs, attemptID)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate force-invalidated person enrichment attempts: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close force-invalidated person enrichment attempts: %w", err)
	}
	for _, attemptID := range attemptIDs {
		if _, err := reconcilePersonEnrichmentCostTx(
			ctx, tx, s.dialect, attemptID, personenrichment.Cost{}, true, now,
		); err != nil {
			return fmt.Errorf("reconcile force-invalidated person enrichment attempt: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE person_enrichment_attempts
		SET state = 'terminal', completed_at = ?, failure_class = ?,
		    next_action_at = NULL, lease_owner = NULL, lease_until = NULL
		WHERE person_id = ?
		  AND state IN ('queued','starting','pending','retry_wait','uncertain_start')`,
		now, string(personenrichment.FailurePolicy), personID); err != nil {
		return fmt.Errorf("force-invalidate person enrichment attempts: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM person_enrichment_work WHERE person_id = ?`, personID); err != nil {
		return fmt.Errorf("force-invalidate person enrichment work: %w", err)
	}
	return nil
}
