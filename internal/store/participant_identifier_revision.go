package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

const participantIdentifierRevisionKey = "participant_identifier_revision"

// ParticipantIdentifierRevision returns the current participant-identifier
// revision (0 if never bumped). It increments whenever an identifier row is
// created, repointed, or given new service/scope classification. Identifiers
// bake into the identity directory datasets (relationship_people search values
// and display labels, the participant_identifiers Parquet export) but not into
// per-row activity facts, so drift on this revision alone is repairable by the
// derived-dataset refresh — unlike AccountIdentityRevision drift, it never
// demands a full rebuild, and coinciding new messages stay incremental.
func (s *Store) ParticipantIdentifierRevision() (int64, error) {
	var value string
	err := s.db.QueryRow(
		`SELECT value FROM archive_metadata WHERE key = ?`, participantIdentifierRevisionKey,
	).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read participant identifier revision: %w", err)
	}
	revision, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse participant identifier revision %q: %w", value, err)
	}
	return revision, nil
}

// bumpParticipantIdentifierRevision increments the participant-identifier
// revision inside tx, seeding the row with 0 first if it does not exist yet,
// following bumpAccountIdentityRevision's approach.
func (s *Store) bumpParticipantIdentifierRevision(tx *loggedTx) error {
	ctx := context.Background()
	if _, err := tx.ExecContext(ctx, s.dialect.InsertOrIgnore(
		`INSERT OR IGNORE INTO archive_metadata (key, value) VALUES (?, '0')`),
		participantIdentifierRevisionKey); err != nil {
		return fmt.Errorf("seed participant identifier revision: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE archive_metadata SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT)
		 WHERE key = ?`,
		participantIdentifierRevisionKey); err != nil {
		return fmt.Errorf("bump participant identifier revision: %w", err)
	}
	return nil
}

func (s *Store) bumpParticipantIdentifierRevisionIfChanged(
	tx *loggedTx, result sql.Result,
) error {
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check participant identifier change: %w", err)
	}
	if changed == 0 {
		return nil
	}
	return s.bumpParticipantIdentifierRevision(tx)
}
