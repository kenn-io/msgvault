package store

import (
	"context"
	"errors"
	"fmt"
)

// PersonMatchConsentEgressContext serializes one provider request with consent
// revocation. The consent row lock is held only while dispatching that request;
// callers must invoke it again for each retry.
func (s *Store) PersonMatchConsentEgressContext(ctx context.Context, fingerprint string, dispatch func() error) (bool, error) {
	if !personMatchFingerprint.MatchString(fingerprint) {
		return false, errors.New("person match disclosure fingerprint is invalid")
	}
	if dispatch == nil {
		return false, errors.New("person match provider dispatch is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin person match consent egress: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// A no-op update acquires a row write lock on PostgreSQL and SQLite's
	// database write lock. Revocation uses the same row, so either revocation
	// commits first and blocks dispatch, or this request dispatches before it.
	result, err := tx.ExecContext(ctx, s.dialect.Rebind(`
		UPDATE person_match_consents SET granted_by = granted_by
		WHERE disclosure_fingerprint = ? AND revoked_at IS NULL`), fingerprint)
	if err != nil {
		return false, fmt.Errorf("lock person match consent for provider dispatch: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read person match consent dispatch lock result: %w", err)
	}
	if rows != 1 {
		return false, nil
	}
	if err := dispatch(); err != nil {
		return true, err
	}
	if err := tx.Commit(); err != nil {
		return true, fmt.Errorf("commit person match consent provider dispatch: %w", err)
	}
	return true, nil
}
