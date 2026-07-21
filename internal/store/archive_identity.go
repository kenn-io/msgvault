package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
)

const (
	archiveUIDKey     = "archive_uid"
	archiveRevisionV1 = "1"
)

// ErrArchiveIdentityCorrupt means the one-time identity migration was
// recorded but its durable UID is missing. Generating a replacement would
// make external references silently point at a different archive.
var ErrArchiveIdentityCorrupt = errors.New("archive identity is corrupt")

// ArchiveUID returns the durable identity of this archive.
func (s *Store) ArchiveUID() (string, error) {
	var uid string
	if err := s.db.QueryRow(`SELECT value FROM archive_metadata WHERE key = ?`, archiveUIDKey).Scan(&uid); err != nil {
		return "", fmt.Errorf("read archive UID: %w", err)
	}
	return uid, nil
}

// ArchiveRevision identifies the archive identity contract stored in
// archive_metadata. It is intentionally independent from both the UID and any
// external project revision so caches can compare all three values explicitly.
func (s *Store) ArchiveRevision() (string, error) {
	if _, err := s.ArchiveUID(); err != nil {
		return "", err
	}
	return archiveRevisionV1, nil
}

func (s *Store) ensureArchiveUID() error {
	return s.withTx(func(tx *loggedTx) error {
		random := make([]byte, 32)
		if _, err := rand.Read(random); err != nil {
			return fmt.Errorf("generate archive UID: %w", err)
		}
		uid := hex.EncodeToString(random)
		statement := s.dialect.InsertOrIgnore(`
			INSERT OR IGNORE INTO archive_metadata (key, value)
			SELECT ?, ?
			WHERE NOT EXISTS (
				SELECT 1 FROM applied_migrations WHERE name = ?
			)`)
		if _, err := tx.Exec(statement, archiveUIDKey, uid, migrationArchiveIdentity); err != nil {
			return fmt.Errorf("persist archive UID: %w", err)
		}

		var existing string
		if err := tx.QueryRow(`SELECT value FROM archive_metadata WHERE key = ?`, archiveUIDKey).Scan(&existing); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("verify archive UID: %w", err)
			}
			var migrationPresent int
			if countErr := tx.QueryRow(`SELECT COUNT(*) FROM applied_migrations WHERE name = ?`, migrationArchiveIdentity).Scan(&migrationPresent); countErr != nil {
				return fmt.Errorf("check archive identity migration: %w", countErr)
			}
			if migrationPresent > 0 {
				return fmt.Errorf("%w: migration ledger is present but archive UID is missing", ErrArchiveIdentityCorrupt)
			}
			return fmt.Errorf("verify archive UID: %w", err)
		}
		migrationSQL := s.dialect.InsertOrIgnore(`INSERT OR IGNORE INTO applied_migrations (name) VALUES (?)`)
		if _, err := tx.Exec(migrationSQL, migrationArchiveIdentity); err != nil {
			return fmt.Errorf("record archive identity migration: %w", err)
		}
		return nil
	})
}
