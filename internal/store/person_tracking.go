package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// PersonTracking reports whether a durable person is selected for future
// profile maintenance. An absent person_tracking row means untracked.
type PersonTracking struct {
	PersonID  int64      `json:"person_id"`
	Tracked   bool       `json:"tracked"`
	TrackedAt *time.Time `json:"tracked_at"`
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
		var exists int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM persons WHERE id = ?`, personID).Scan(&exists); err != nil {
			return fmt.Errorf("check person %d for tracking: %w", personID, err)
		}
		if exists == 0 {
			return fmt.Errorf("track person %d: %w", personID, ErrPersonNotFound)
		}

		var err error
		if tracked {
			_, err = tx.ExecContext(ctx, `
				INSERT INTO person_tracking (person_id, tracked_at)
				VALUES (?, ?)
				ON CONFLICT (person_id) DO NOTHING
			`, personID, time.Now().UTC())
		} else {
			_, err = tx.ExecContext(ctx,
				`DELETE FROM person_tracking WHERE person_id = ?`, personID)
		}
		if err != nil {
			return fmt.Errorf("set person %d tracking to %t: %w", personID, tracked, err)
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
