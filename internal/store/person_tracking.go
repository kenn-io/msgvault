package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.kenn.io/msgvault/internal/personenrichment"
)

// PersonTracking reports whether a durable person is selected for future
// profile maintenance. An absent person_tracking row means untracked.
type PersonTracking struct {
	PersonID  int64      `json:"person_id"`
	Tracked   bool       `json:"tracked"`
	TrackedAt *time.Time `json:"tracked_at"`
}

const maxTrackedPeopleList = 1_000

// ListTrackedPeopleContext returns a bounded ascending page of durable people
// that are currently enrolled in profile maintenance.
func (s *Store) ListTrackedPeopleContext(
	ctx context.Context, afterID int64, limit int,
) ([]int64, error) {
	if limit < 1 || limit > maxTrackedPeopleList {
		return nil, fmt.Errorf("list tracked people: limit must be between 1 and %d", maxTrackedPeopleList)
	}
	rows, err := s.db.QueryContext(ctx, s.Rebind(`
		SELECT person_id FROM person_tracking
		WHERE person_id > ?
		ORDER BY person_id
		LIMIT ?`), afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("list tracked people: %w", err)
	}
	defer func() { _ = rows.Close() }()
	people := make([]int64, 0, limit)
	for rows.Next() {
		var personID int64
		if err := rows.Scan(&personID); err != nil {
			return nil, fmt.Errorf("scan tracked person: %w", err)
		}
		people = append(people, personID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tracked people: %w", err)
	}
	return people, nil
}

// GetPersonTrackingContext reads the explicit tracking state for a person.
func (s *Store) GetPersonTrackingContext(
	ctx context.Context, personID int64,
) (*PersonTracking, error) {
	var state *PersonTracking
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		var err error
		state, err = s.getPersonTrackingTx(ctx, tx, personID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return state, nil
}

// SetPersonTrackingContext replaces the tracking state idempotently.
func (s *Store) SetPersonTrackingContext(
	ctx context.Context, personID int64, tracked bool,
) (*PersonTracking, error) {
	var state *PersonTracking
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if s.personEnrichmentTxBarrier != nil {
			s.personEnrichmentTxBarrier("tracking_before_authority_lock")
		}
		if err := s.lockPersonEnrichmentAuthorityMutationTx(ctx, tx); err != nil {
			return err
		}
		if err := s.lockProfileIdentityKeyTxContext(
			ctx, tx, "person-fact-generation", personID); err != nil {
			return err
		}
		_, err := lockPersonEnrichmentPersonTx(ctx, tx, s.dialect, personID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("track person %d: %w", personID, ErrPersonNotFound)
		}
		if err != nil {
			return err
		}
		if s.personEnrichmentTxBarrier != nil {
			s.personEnrichmentTxBarrier("tracking_person_locked")
		}

		trackingAdded := false
		trackingGeneration := ""
		if tracked {
			var result sql.Result
			result, err = tx.ExecContext(ctx, `
				INSERT INTO person_tracking (person_id, tracked_at)
				VALUES (?, ?)
				ON CONFLICT (person_id) DO NOTHING
			`, personID, time.Now().UTC())
			var inserted int64
			if err == nil {
				inserted, err = result.RowsAffected()
				trackingAdded = inserted == 1
				if trackingAdded {
					trackingGeneration = "enrollment:" + uuid.NewString()
				}
			}
			if err == nil && inserted == 1 {
				_, err = tx.ExecContext(ctx, `UPDATE person_sweep_cursors
					SET reconcile_upper_key = '', reconcile_after_key = '',
					    optimistic_document_key = '', reconcile_document_key = '',
					    backstop_upper_key = '', backstop_after_key = '', backstop_document_key = '',
					    reconciliation_complete = FALSE, last_backstop_at = NULL,
					    updated_at = ?
					WHERE person_id = ?`, time.Now().UTC(), personID)
			}
			if err == nil && inserted == 1 {
				var highWater int64
				err = tx.QueryRowContext(ctx, `
					SELECT sequence FROM person_sweep_change_clock WHERE singleton = TRUE`,
				).Scan(&highWater)
				if err == nil {
					err = s.upsertPersonSweepWorkTx(ctx, tx, personID, highWater)
				}
			}
		} else {
			// Delete enrollment first. PostgreSQL publishers hold a key-share
			// lock on this row through their work upsert, so tracking-off waits
			// for that publication and then removes any row it committed.
			if _, err = tx.ExecContext(ctx,
				`DELETE FROM person_tracking WHERE person_id = ?`, personID); err == nil {
				_, err = tx.ExecContext(ctx,
					`DELETE FROM person_sweep_work WHERE person_id = ?`, personID)
			}
		}
		if err != nil {
			return fmt.Errorf("set person %d tracking to %t: %w", personID, tracked, err)
		}
		if tracked {
			if trackingAdded {
				if err := s.publishPersonEnrichmentTx(ctx, tx, personID,
					personenrichment.TriggerTracked, trackingGeneration,
					s.personEnrichmentTime()); err != nil {
					return err
				}
			}
		} else {
			if s.personEnrichmentTxBarrier != nil {
				s.personEnrichmentTxBarrier("untrack_authority_removed")
			}
			if err := s.forceInvalidatePersonEnrichmentTx(ctx, tx, personID); err != nil {
				return err
			}
		}
		state, err = s.getPersonTrackingTx(ctx, tx, personID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return state, nil
}

func (s *Store) getPersonTrackingTx(
	ctx context.Context, tx *loggedTx, personID int64,
) (*PersonTracking, error) {
	state := &PersonTracking{PersonID: personID}
	var trackedAt nullableTimestamp
	err := tx.QueryRowContext(ctx, `
		SELECT p.id, pt.tracked_at
		FROM persons p
		LEFT JOIN person_tracking pt ON pt.person_id = p.id
		WHERE p.id = ?
	`, personID).Scan(&state.PersonID, &trackedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("get person %d tracking: %w", personID, ErrPersonNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get person %d tracking: %w", personID, err)
	}
	if trackedAt.Valid {
		state.Tracked = true
		state.TrackedAt = &trackedAt.Time
	}
	return state, nil
}
