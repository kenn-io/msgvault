package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
			var personID int64
			switch {
			case strings.HasPrefix(subject, "person/"):
				err := tx.QueryRowContext(ctx, `SELECT id FROM persons WHERE vcard_uid = ?`, strings.TrimPrefix(subject, "person/")).Scan(&personID)
				if err != nil {
					return errors.New("important-date subject is unavailable")
				}
			case strings.HasPrefix(subject, "owned/"):
				err := tx.QueryRowContext(ctx, `SELECT p.id FROM important_date_subjects d JOIN persons p ON (
					(p.vcard_uid = d.owner_uid AND NOT EXISTS (SELECT 1 FROM important_date_identity_bindings b
						JOIN important_date_reconciliations r ON r.uid = b.reconciliation_uid AND r.active = 1
						WHERE b.original_subject = 'owned/' || d.uid AND b.meaning = 'birthday'))
					OR EXISTS (SELECT 1 FROM important_date_identity_bindings b
						JOIN important_date_reconciliations r ON r.uid = b.reconciliation_uid AND r.active = 1
						WHERE b.original_subject = 'owned/' || d.uid AND b.resolved_subject = 'owned/' || d.uid
						AND b.meaning = 'birthday' AND b.effective_owner = 'person/' || p.vcard_uid)
				) WHERE d.uid = ? AND d.withdrawn = 0`, strings.TrimPrefix(subject, "owned/")).Scan(&personID)
				if err != nil {
					return errors.New("important-date subject is unavailable")
				}
			default:
				return errors.New("invalid important-date subject")
			}
			stable, err := s.ImportantDateIdentityStableContext(withWritableSnapshot(ctx, s.db.DB, tx), personID)
			if err != nil {
				return err
			}
			if !stable {
				return errors.New("important-date subject requires identity reconciliation")
			}
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
// routing using the provided transaction context before its external write.
// Record the in-flight state in a separate committed transaction before calling.
// An error can mean the external write
// committed; callers must retain an unknown delivery state rather than retry.
func (s *Store) WithImportantDateConsentContext(ctx context.Context, subject, owner, project, recipient string, dispatch func(context.Context) error) (bool, error) {
	if !validImportantDateConsent(subject, project, recipient) || !strings.HasPrefix(owner, "person/") || dispatch == nil {
		return false, errors.New("invalid important-date consent dispatch")
	}
	var active bool
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		if err := s.lockImportantDatePeopleTx(ctx, tx, subject, owner); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM important_date_consents WHERE subject_uid = ? AND project = ? AND recipient = ? AND granted = 1)`, subject, project, recipient).Scan(&active); err != nil {
			return fmt.Errorf("read important-date consent: %w", err)
		}
		if !active {
			return nil
		}
		return dispatch(withWritableSnapshot(ctx, s.db.DB, tx))
	})
	return active, err
}

// lockImportantDatePeopleTx fences writers that update a relationship or
// cadence before bumping a person's projection revision in the same transaction.
// Lock the subject first, then discover and lock every potential parent.
func (s *Store) lockImportantDatePeopleTx(ctx context.Context, tx *loggedTx, subject, owner string) error {
	ownerUID := strings.TrimPrefix(owner, "person/")
	var ownerID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM persons WHERE vcard_uid = ?`, ownerUID).Scan(&ownerID); err != nil {
		return fmt.Errorf("resolve important-date owner: %w", err)
	}
	subjectID := ownerID
	if subjectUID, ok := strings.CutPrefix(subject, "person/"); ok {
		if err := tx.QueryRowContext(ctx, `SELECT id FROM persons WHERE vcard_uid = ?`, subjectUID).Scan(&subjectID); err != nil {
			return fmt.Errorf("resolve important-date person: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE persons SET revision = revision WHERE id = ?`, subjectID); err != nil {
		return fmt.Errorf("lock important-date person: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT r.source_person_id FROM person_relationships r JOIN relationship_types t ON t.id = r.relationship_type_id WHERE r.target_person_id = ? AND t.slug = 'parent' ORDER BY r.source_person_id`, subjectID)
	if err != nil {
		return fmt.Errorf("list important-date parents: %w", err)
	}
	ids := []int64{ownerID}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	slices.Sort(ids)
	for _, id := range slices.Compact(ids) {
		if id == subjectID {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE persons SET revision = revision WHERE id = ?`, id); err != nil {
			return fmt.Errorf("lock important-date parent: %w", err)
		}
	}
	return nil
}

// RevokeImportantDateConsentContext revokes every destination for a subject,
// including destinations that are currently unreachable.
func (s *Store) RevokeImportantDateConsentContext(ctx context.Context, subject string) error {
	if strings.TrimSpace(subject) == "" {
		return errors.New("invalid important-date subject")
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE important_date_consents SET granted = 0 WHERE subject_uid = ?`, subject); err != nil {
			return fmt.Errorf("revoke important-date consent: %w", err)
		}
		return nil
	})
}
