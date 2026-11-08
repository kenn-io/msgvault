package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// ImportantDateSubject is an explicitly maintained dependent or pet.
type ImportantDateSubject struct {
	UID         string      `json:"uid"`
	OwnerUID    string      `json:"owner_uid"`
	Kind        string      `json:"kind"`
	DisplayName string      `json:"display_name"`
	Date        PartialDate `json:"date"`
	Revision    int64       `json:"revision"`
}

type ImportantDateSubjectInput struct {
	OwnerUID    string
	Kind        string
	DisplayName string
	Date        PartialDate
}

// CreateImportantDateSubjectContext stores a subject without creating a person.
func (s *Store) CreateImportantDateSubjectContext(ctx context.Context, input ImportantDateSubjectInput) (*ImportantDateSubject, error) {
	if strings.TrimSpace(input.OwnerUID) == "" || (input.Kind != "dependent" && input.Kind != "pet") || strings.TrimSpace(input.DisplayName) == "" || input.Date.Month == nil || input.Date.Day == nil || input.Date.Validate() != nil {
		return nil, errors.New("invalid important-date subject")
	}
	uid, err := uuid.NewRandom()
	if err != nil {
		return nil, fmt.Errorf("create subject identity: %w", err)
	}
	var result *ImportantDateSubject
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM persons WHERE vcard_uid = ?)`, input.OwnerUID).Scan(&exists); err != nil {
			return fmt.Errorf("check owner: %w", err)
		}
		if !exists {
			return errors.New("important-date owner not found")
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO important_date_subjects (uid, owner_uid, kind, display_name, birth_year, birth_month, birth_day) VALUES (?, ?, ?, ?, ?, ?, ?)`, uid.String(), input.OwnerUID, input.Kind, strings.TrimSpace(input.DisplayName), input.Date.Year, *input.Date.Month, *input.Date.Day)
		if err != nil {
			return fmt.Errorf("create important-date subject: %w", err)
		}
		result = &ImportantDateSubject{UID: uid.String(), OwnerUID: input.OwnerUID, Kind: input.Kind, DisplayName: strings.TrimSpace(input.DisplayName), Date: input.Date, Revision: 1}
		return nil
	})
	return result, err
}

// ListImportantDateSubjectsContext lists explicit active subjects for one owner.
func (s *Store) ListImportantDateSubjectsContext(ctx context.Context, ownerUID string) ([]ImportantDateSubject, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT uid, owner_uid, kind, display_name, birth_year, birth_month, birth_day, revision FROM important_date_subjects
		WHERE withdrawn = 0 AND owner_uid = ? ORDER BY uid`, ownerUID)
	if err != nil {
		return nil, fmt.Errorf("list important-date subjects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var subjects []ImportantDateSubject
	for rows.Next() {
		var subject ImportantDateSubject
		var year sql.NullInt64
		var month, day int
		if err := rows.Scan(&subject.UID, &subject.OwnerUID, &subject.Kind, &subject.DisplayName, &year, &month, &day, &subject.Revision); err != nil {
			return nil, fmt.Errorf("read important-date subject: %w", err)
		}
		subject.Date = PartialDate{Month: &month, Day: &day}
		if year.Valid {
			value := int(year.Int64)
			subject.Date.Year = &value
		}
		subjects = append(subjects, subject)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list important-date subjects: %w", err)
	}
	return subjects, nil
}

// UpdateImportantDateSubjectContext revises or withdraws a subject under an
// optimistic revision check, preserving the prior explicit value in history.
func (s *Store) UpdateImportantDateSubjectContext(ctx context.Context, uid string, revision int64, name string, date PartialDate, withdraw bool) (bool, error) {
	if strings.TrimSpace(uid) == "" || revision <= 0 || (!withdraw && (strings.TrimSpace(name) == "" || date.Month == nil || date.Day == nil || date.Validate() != nil)) {
		return false, errors.New("invalid important-date subject update")
	}
	var changed bool
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO important_date_subject_history (subject_uid, revision, display_name, birth_year, birth_month, birth_day, withdrawn)
			SELECT uid, revision, display_name, birth_year, birth_month, birth_day, withdrawn FROM important_date_subjects
			WHERE uid = ? AND revision = ? AND withdrawn = 0 ON CONFLICT (subject_uid, revision) DO NOTHING`, uid, revision)
		if err != nil {
			return fmt.Errorf("retain important-date subject history: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count == 0 {
			return nil
		}
		if withdraw {
			result, err = tx.ExecContext(ctx, `UPDATE important_date_subjects SET withdrawn = 1, revision = revision + 1 WHERE uid = ? AND revision = ? AND withdrawn = 0`, uid, revision)
		} else {
			result, err = tx.ExecContext(ctx, `UPDATE important_date_subjects SET display_name = ?, birth_year = ?, birth_month = ?, birth_day = ?, revision = revision + 1 WHERE uid = ? AND revision = ? AND withdrawn = 0`, strings.TrimSpace(name), date.Year, *date.Month, *date.Day, uid, revision)
		}
		if err != nil {
			return fmt.Errorf("update important-date subject: %w", err)
		}
		count, err = result.RowsAffected()
		if err != nil {
			return err
		}
		changed = count == 1
		if !changed {
			return errors.New("important-date subject changed concurrently")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE important_date_consents SET granted = 0 WHERE subject_uid = ?`, "owned/"+uid); err != nil {
			return fmt.Errorf("revoke changed-subject consent: %w", err)
		}
		return nil
	})
	return changed, err
}
