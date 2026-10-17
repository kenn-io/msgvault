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
	PersonID   int64      `json:"person_id"`
	Tracked    bool       `json:"tracked"`
	TrackedAt  *time.Time `json:"tracked_at"`
	Provenance string     `json:"-"`
	Actor      string     `json:"-"`
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
		var err error
		state, err = s.setPersonTrackingTx(ctx, tx, personID, tracked)
		return err
	})
	if err != nil {
		return nil, err
	}
	return state, nil
}

// SetPersonTrackingActorContext records the owner's explicit tracking intent.
func (s *Store) SetPersonTrackingActorContext(
	ctx context.Context, personID int64, tracked bool, actor string,
) (*PersonTracking, error) {
	var state *PersonTracking
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		var err error
		state, err = s.setPersonTrackingWithOptionsTx(ctx, tx, personID, tracked, personTierGrantOptions{
			Provenance: PersonTierProvenanceManual, Actor: actor, RecordOptOut: true,
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return state, nil
}

// setPersonTrackingTx preserves the existing manual transition contract.
func (s *Store) setPersonTrackingTx(
	ctx context.Context, tx *loggedTx, personID int64, tracked bool,
) (*PersonTracking, error) {
	return s.setPersonTrackingWithOptionsTx(ctx, tx, personID, tracked, personTierGrantOptions{
		Provenance: PersonTierProvenanceManual, RecordOptOut: true,
	})
}

func (s *Store) setPersonTrackingWithOptionsTx(
	ctx context.Context, tx *loggedTx, personID int64, tracked bool, opts personTierGrantOptions,
) (*PersonTracking, error) {
	opts, err := s.normalizePersonTierGrantOptions(opts)
	if err != nil {
		return nil, err
	}
	if s.personEnrichmentTxBarrier != nil {
		s.personEnrichmentTxBarrier("tracking_before_authority_lock")
	}
	if err := s.lockPersonEnrichmentAuthorityMutationTx(ctx, tx); err != nil {
		return nil, err
	}
	if err := s.lockProfileIdentityKeyTxContext(ctx, tx, "person-fact-generation", personID); err != nil {
		return nil, err
	}
	if _, err := lockPersonEnrichmentPersonTx(ctx, tx, s.dialect, personID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("track person %d: %w", personID, ErrPersonNotFound)
		}
		return nil, err
	}
	if s.personEnrichmentTxBarrier != nil {
		s.personEnrichmentTxBarrier("tracking_person_locked")
	}
	before, err := s.getPersonTrackingTx(ctx, tx, personID)
	if err != nil {
		return nil, err
	}
	automatic := opts.Provenance == PersonTierProvenanceAuto
	if automatic && before.Provenance == PersonTierProvenanceManual {
		return before, nil
	}
	if tracked {
		if automatic {
			blocked, err := s.personTierOptedOutTx(ctx, tx, personID, PersonTierLaneTracking)
			if err != nil {
				return nil, err
			}
			if blocked {
				return before, nil
			}
		}
		eligibleBefore, err := s.personEnrichmentScheduledEligibleTx(ctx, tx, personID)
		if err != nil {
			return nil, err
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO person_tracking(person_id,tracked_at,provenance,actor)
   VALUES(?,?,?,?) ON CONFLICT(person_id) DO NOTHING`, personID, opts.At, opts.Provenance, opts.Actor)
		if err != nil {
			return nil, fmt.Errorf("track person %d: %w", personID, err)
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("count person %d tracking grant: %w", personID, err)
		}
		if !automatic {
			if _, err := tx.ExecContext(ctx, `UPDATE person_tracking SET provenance='manual',actor=? WHERE person_id=? AND provenance='auto'`, opts.Actor, personID); err != nil {
				return nil, fmt.Errorf("upgrade person %d tracking to manual: %w", personID, err)
			}
			if err := s.clearPersonTierOptOutTx(ctx, tx, personID, PersonTierLaneTracking); err != nil {
				return nil, err
			}
		}
		if inserted == 1 {
			if _, err := tx.ExecContext(ctx, `UPDATE person_sweep_cursors
    SET reconcile_upper_key='',reconcile_after_key='',optimistic_document_key='',reconcile_document_key='',
     backstop_upper_key='',backstop_after_key='',backstop_document_key='',
     reconciliation_complete=FALSE,last_backstop_at=NULL,updated_at=? WHERE person_id=?`, opts.At, personID); err != nil {
				return nil, fmt.Errorf("reset person %d sweep cursors: %w", personID, err)
			}
			var highWater int64
			if err := tx.QueryRowContext(ctx, `SELECT sequence FROM person_sweep_change_clock WHERE singleton=TRUE`).Scan(&highWater); err != nil {
				return nil, fmt.Errorf("read person sweep change clock: %w", err)
			}
			if err := s.upsertPersonSweepWorkTx(ctx, tx, personID, highWater); err != nil {
				return nil, err
			}
		}
		if !automatic {
			eligibleAfter, err := s.personEnrichmentScheduledEligibleTx(ctx, tx, personID)
			if err != nil {
				return nil, err
			}
			if !eligibleBefore && eligibleAfter {
				generation := "enrollment:" + uuid.NewString()
				if before.Tracked {
					generation = "provenance:" + uuid.NewString()
				}
				if err := s.publishPersonEnrichmentTx(ctx, tx, personID, personenrichment.TriggerTracked, generation, opts.At); err != nil {
					return nil, err
				}
			}
		}
	} else {
		if automatic && before.Tracked {
			if err := s.refusePersonTierTrackingRemovalWithManualRunTx(ctx, tx, personID); err != nil {
				return nil, err
			}
		}
		briefPredicate := "person_id=?"
		if automatic {
			briefPredicate += " AND provenance='auto'"
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM person_brief_enrollments WHERE `+briefPredicate, personID); err != nil {
			return nil, fmt.Errorf("remove person %d dependent brief enrollment: %w", personID, err)
		}
		// Remove tracking before derived work while the authority gate excludes
		// concurrent scheduled publishers.
		if _, err := tx.ExecContext(ctx, `DELETE FROM person_tracking WHERE person_id=?`, personID); err != nil {
			return nil, fmt.Errorf("untrack person %d: %w", personID, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM person_sweep_work WHERE person_id=?`, personID); err != nil {
			return nil, fmt.Errorf("remove person %d sweep work: %w", personID, err)
		}
		if !automatic && opts.RecordOptOut {
			for _, lane := range []string{PersonTierLaneTracking, PersonTierLaneBriefs} {
				if err := s.recordPersonTierOptOutTx(ctx, tx, personID, lane, opts.Actor, "", opts.At); err != nil {
					return nil, err
				}
			}
		}
		if s.personEnrichmentTxBarrier != nil {
			s.personEnrichmentTxBarrier("untrack_authority_removed")
		}
		if automatic {
			if err := s.cancelScheduledPersonEnrichmentTx(ctx, tx, personID); err != nil {
				return nil, err
			}
		} else if err := s.forceInvalidatePersonEnrichmentTx(ctx, tx, personID); err != nil {
			return nil, err
		}
	}
	return s.getPersonTrackingTx(ctx, tx, personID)
}

func (s *Store) getPersonTrackingTx(
	ctx context.Context, tx *loggedTx, personID int64,
) (*PersonTracking, error) {
	state := &PersonTracking{PersonID: personID}
	var trackedAt nullableTimestamp
	var provenance, actor sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT p.id, pt.tracked_at,pt.provenance,pt.actor
		FROM persons p
		LEFT JOIN person_tracking pt ON pt.person_id = p.id
		WHERE p.id = ?
	`, personID).Scan(&state.PersonID, &trackedAt, &provenance, &actor)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("get person %d tracking: %w", personID, ErrPersonNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get person %d tracking: %w", personID, err)
	}
	if trackedAt.Valid {
		state.Tracked = true
		state.TrackedAt = &trackedAt.Time
		state.Provenance = provenance.String
		state.Actor = actor.String
	}
	return state, nil
}
