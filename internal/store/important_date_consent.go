package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

func validImportantDateConsent(subject, project, recipient string) bool {
	return strings.TrimSpace(subject) == subject && subject != "" && len(subject) <= 512 &&
		strings.TrimSpace(project) == project && project != "" && len(project) <= 256 &&
		strings.TrimSpace(recipient) == recipient && recipient != "" && len(recipient) <= 128
}

// SetImportantDateConsentContext records consent for exactly one subject and destination.
// Identity mutations share the same lock; callers must separately fence dispatch.
func (s *Store) SetImportantDateConsentContext(ctx context.Context, subject, project, recipient string, granted bool) error {
	if !validImportantDateConsent(subject, project, recipient) {
		return errors.New("invalid important-date consent scope")
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		value := 0
		if granted {
			value = 1
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO important_date_consents (subject_uid, project, recipient, granted) VALUES (?, ?, ?, ?)
			ON CONFLICT (subject_uid, project, recipient) DO UPDATE SET granted = excluded.granted`, subject, project, recipient, value)
		if err != nil {
			return fmt.Errorf("set important-date consent: %w", err)
		}
		return nil
	})
}

// HasImportantDateConsentContext checks one exact subject and destination.
func (s *Store) HasImportantDateConsentContext(ctx context.Context, subject, project, recipient string) (bool, error) {
	if !validImportantDateConsent(subject, project, recipient) {
		return false, errors.New("invalid important-date consent scope")
	}
	var active bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM important_date_consents WHERE subject_uid = ? AND project = ? AND recipient = ? AND granted = 1)`, subject, project, recipient).Scan(&active)
	if err != nil {
		return false, fmt.Errorf("read important-date consent: %w", err)
	}
	return active, nil
}

// WithImportantDateConsentContext holds the identity mutation fence while it
// checks consent and dispatches. The callback must revalidate the event and
// routing before its external write. An error can mean the external write
// committed; callers must retain an unknown delivery state rather than retry.
func (s *Store) WithImportantDateConsentContext(ctx context.Context, subject, project, recipient string, dispatch func() error) (bool, error) {
	if !validImportantDateConsent(subject, project, recipient) || dispatch == nil {
		return false, errors.New("invalid important-date consent dispatch")
	}
	var active bool
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM important_date_consents WHERE subject_uid = ? AND project = ? AND recipient = ? AND granted = 1)`, subject, project, recipient).Scan(&active); err != nil {
			return fmt.Errorf("read important-date consent: %w", err)
		}
		if !active {
			return nil
		}
		return dispatch()
	})
	return active, err
}
