package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

const participantDisplayNameRevisionKey = "participant_display_name_revision"

// lockParticipantDirectoryMutationTxContext serializes multi-participant
// ensures with participant merges on PostgreSQL. Those operations otherwise
// acquire participant row locks in different logical orders and can deadlock.
// SQLite already serializes writers, so it needs no additional lock.
func (s *Store) lockParticipantDirectoryMutationTxContext(
	ctx context.Context, tx *loggedTx,
) error {
	if !s.IsPostgreSQL() {
		return nil
	}
	return s.lockProfileIdentityKeyTxContext(
		ctx, tx, "participant-directory-mutation",
	)
}

// ParticipantDisplayNameRepair is one validated display-name replacement used
// by maintenance commands that must update participant data and its cache
// revision atomically.
type ParticipantDisplayNameRepair struct {
	ParticipantID int64
	DisplayName   string
}

// ParticipantDisplayNameRevision returns the current participant display-name
// revision (0 if never bumped). It advances when a participant row is added
// or when an existing participant receives a previously blank display name.
// Callers use it to detect stale participant-derived cache datasets.
func (s *Store) ParticipantDisplayNameRevision() (int64, error) {
	return readParticipantDisplayNameRevision(s.db)
}

func readParticipantDisplayNameRevision(q rowQuerier) (int64, error) {
	var value string
	err := q.QueryRow(
		`SELECT value FROM archive_metadata WHERE key = ?`, participantDisplayNameRevisionKey,
	).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read participant display-name revision: %w", err)
	}
	revision, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse participant display-name revision %q: %w", value, err)
	}
	return revision, nil
}

// bumpParticipantDisplayNameRevision increments the participant display-name
// revision inside tx, seeding the metadata row when it does not exist yet.
func (s *Store) bumpParticipantDisplayNameRevision(tx *loggedTx) error {
	return s.bumpParticipantDisplayNameRevisionContext(context.Background(), tx)
}

func (s *Store) bumpParticipantDisplayNameRevisionContext(
	ctx context.Context,
	tx *loggedTx,
) error {
	if _, err := tx.ExecContext(ctx, s.dialect.InsertOrIgnore(
		`INSERT OR IGNORE INTO archive_metadata (key, value) VALUES (?, '0')`),
		participantDisplayNameRevisionKey); err != nil {
		return fmt.Errorf("seed participant display-name revision: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE archive_metadata SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT)
		 WHERE key = ?`,
		participantDisplayNameRevisionKey); err != nil {
		return fmt.Errorf("bump participant display-name revision: %w", err)
	}
	return nil
}

func (s *Store) bumpParticipantDisplayNameRevisionIfChanged(
	tx *loggedTx,
	result sql.Result,
) (bool, error) {
	return s.bumpParticipantDisplayNameRevisionIfChangedContext(
		context.Background(), tx, result,
	)
}

func (s *Store) bumpParticipantDisplayNameRevisionIfChangedContext(
	ctx context.Context,
	tx *loggedTx,
	result sql.Result,
) (bool, error) {
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("check participant display-name change: %w", err)
	}
	if changed <= 0 {
		return false, nil
	}
	if err := s.bumpParticipantDisplayNameRevisionContext(ctx, tx); err != nil {
		return false, err
	}
	return true, nil
}

// RepairParticipantDisplayNames applies one maintenance batch and advances the
// display-name revision once when at least one participant row changes.
func (s *Store) RepairParticipantDisplayNames(
	repairs []ParticipantDisplayNameRepair,
) error {
	if len(repairs) == 0 {
		return nil
	}
	return s.withTx(func(tx *loggedTx) error {
		changed := false
		for _, repair := range repairs {
			result, err := tx.Exec(`
				UPDATE participants SET display_name = ?
				WHERE id = ?
				  AND (display_name IS NULL OR display_name <> ?)
			`, repair.DisplayName, repair.ParticipantID, repair.DisplayName)
			if err != nil {
				return fmt.Errorf(
					"repair participant display name %d: %w",
					repair.ParticipantID, err,
				)
			}
			rows, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("check participant display-name repair: %w", err)
			}
			changed = changed || rows > 0
		}
		if !changed {
			return nil
		}
		return s.bumpParticipantDisplayNameRevision(tx)
	})
}
