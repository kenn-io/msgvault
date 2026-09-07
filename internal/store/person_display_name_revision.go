package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

const personDisplayNameRevisionKey = "person_display_name_revision"

// PersonDisplayNameRevision tracks curated display-name updates.
// Imported-name creation and remote or local display-name changes advance it.
// Binding, merge, and deletion advance identity_revision instead. Profile
// component patches do not change display_name or this counter.
func (s *Store) PersonDisplayNameRevision() (int64, error) {
	return readPersonDisplayNameRevision(s.db)
}

func readPersonDisplayNameRevision(q rowQuerier) (int64, error) {
	var value string
	err := q.QueryRow(
		`SELECT value FROM archive_metadata WHERE key = ?`, personDisplayNameRevisionKey,
	).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read person display-name revision: %w", err)
	}
	revision, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse person display-name revision %q: %w", value, err)
	}
	return revision, nil
}

// bumpPersonDisplayNameRevisionContext seeds and advances the counter inside
// the curated display-name transaction.
func (s *Store) bumpPersonDisplayNameRevisionContext(
	ctx context.Context,
	tx *loggedTx,
) error {
	if _, err := tx.ExecContext(ctx, s.dialect.InsertOrIgnore(
		`INSERT OR IGNORE INTO archive_metadata (key, value) VALUES (?, '0')`),
		personDisplayNameRevisionKey); err != nil {
		return fmt.Errorf("seed person display-name revision: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE archive_metadata SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT)
		 WHERE key = ?`,
		personDisplayNameRevisionKey); err != nil {
		return fmt.Errorf("bump person display-name revision: %w", err)
	}
	return nil
}
