package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type PersonDateKind string

const (
	PersonDateBirthday    PersonDateKind = "birthday"
	PersonDateAnniversary PersonDateKind = "anniversary"
	PersonDateDeath       PersonDateKind = "death"
	PersonDateCustom      PersonDateKind = "custom"
)

func (k PersonDateKind) Valid() bool {
	switch k {
	case PersonDateBirthday, PersonDateAnniversary, PersonDateDeath, PersonDateCustom:
		return true
	default:
		return false
	}
}

type PersonDate struct {
	Envelope      ValueEnvelope  `json:"envelope"`
	PersonID      int64          `json:"person_id"`
	DateKind      PersonDateKind `json:"date_kind"`
	Label         *string        `json:"label,omitzero" nullable:"false"`
	Date          PartialDate    `json:"date"`
	DateText      *string        `json:"date_text,omitzero" nullable:"false"`
	CalendarScale *string        `json:"calendar_scale,omitzero" nullable:"false"`
	OriginalValue string         `json:"original_value"`
}

type PersonDateInput struct {
	DateKind      PersonDateKind     `json:"date_kind"`
	Label         *string            `json:"label,omitzero" nullable:"false"`
	Date          PartialDate        `json:"date"`
	DateText      *string            `json:"date_text,omitzero" nullable:"false"`
	CalendarScale *string            `json:"calendar_scale,omitzero" nullable:"false"`
	OriginalValue string             `json:"original_value"`
	Envelope      ValueEnvelopeInput `json:"envelope"`
}

var (
	ErrInvalidPersonDateKind  = errors.New("invalid person date kind")
	ErrPersonDateValueMissing = errors.New("person date requires a partial date or date text")
)

func (s *Store) AddPersonDateContext(
	ctx context.Context, personID int64, input PersonDateInput,
) (*PersonDate, error) {
	var result *PersonDate
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		if err := ensureProfilePersonTx(ctx, tx, personID); err != nil {
			return err
		}
		var err error
		result, err = s.addPersonDateTx(ctx, tx, personID, input)
		if err != nil {
			return err
		}
		if err := s.bumpPersonRevisionsTx(ctx, tx, personID); err != nil {
			return err
		}
		return nil
	})
	return result, err
}

func (s *Store) ListPersonDatesContext(
	ctx context.Context, personID int64, currentOnly bool,
) ([]PersonDate, error) {
	var dates []PersonDate
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		var err error
		dates, err = s.listPersonDatesTx(ctx, tx, personID, currentOnly)
		return err
	})
	return dates, err
}

func (s *Store) SupersedePersonDateContext(
	ctx context.Context, personID, dateID int64, activeUntil *time.Time,
) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		if err := s.supersedePersonDateTx(ctx, tx, personID, dateID, activeUntil); err != nil {
			return err
		}
		return s.bumpPersonRevisionsTx(ctx, tx, personID)
	})
}

func (s *Store) addPersonDateTx(
	ctx context.Context, tx *loggedTx, personID int64, input PersonDateInput,
) (*PersonDate, error) {
	if !input.DateKind.Valid() {
		return nil, ErrInvalidPersonDateKind
	}
	if input.Date.IsZero() && (input.DateText == nil || strings.TrimSpace(*input.DateText) == "") {
		return nil, ErrPersonDateValueMissing
	}
	if !input.Date.IsZero() {
		if err := input.Date.Validate(); err != nil {
			return nil, err
		}
	}
	original := strings.TrimSpace(input.OriginalValue)
	if original == "" {
		if input.Date.IsZero() {
			original = strings.TrimSpace(*input.DateText)
		} else {
			original = input.Date.String()
		}
	}
	env, err := resolveProfileEnvelopeTx(
		ctx, tx, "person_dates", "date_kind", personID, input.DateKind, input.Envelope,
	)
	if err != nil {
		return nil, err
	}
	args := []any{personID, input.DateKind, stringValue(input.Label)}
	args = append(args, PartialDateArgs(input.Date)...)
	args = append(args, stringValue(input.DateText), stringValue(input.CalendarScale), original)
	args = append(args, profileEnvelopeArgs(env)...)
	var id int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO person_dates (
		person_id, date_kind, label, date_year, date_month, date_day,
		date_text, calendar_scale, original_value, `+profileEnvelopeWriteColumns+`,
		created_at, updated_at
	) VALUES (
		?, ?, ?, ?, ?, ?, ?, ?, ?,
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		`+s.dialect.Now()+`, `+s.dialect.Now()+`
	) RETURNING id`, args...).Scan(&id); err != nil {
		return nil, fmt.Errorf("add person date: %w", err)
	}
	if err := s.revokePersonImportantDateConsentTx(ctx, tx, personID); err != nil {
		return nil, err
	}
	return getPersonDateTx(ctx, tx, personID, id)
}

func (s *Store) listPersonDatesTx(
	ctx context.Context, tx *loggedTx, personID int64, currentOnly bool,
) ([]PersonDate, error) {
	query := personDateSelect + ` WHERE person_id = ?`
	if currentOnly {
		query += ` AND active_until IS NULL AND superseded_at IS NULL`
	}
	query += ` ORDER BY date_kind,
		CASE WHEN pref IS NULL THEN 1 ELSE 0 END, pref, ordinal, id`
	return queryProfileRowsTx(ctx, tx, query, scanPersonDate, personID)
}

func (s *Store) supersedePersonDateTx(
	ctx context.Context, tx *loggedTx, personID, dateID int64, activeUntil *time.Time,
) error {
	if err := s.supersedeProfileValueTx(ctx, tx, "person_dates", personID, dateID, activeUntil); err != nil {
		return err
	}
	return s.revokePersonImportantDateConsentTx(ctx, tx, personID)
}

// Date facts can change the information an alert represents. Consent is tied
// to the prior subject data and must be granted again after a date mutation.
func (s *Store) revokePersonImportantDateConsentTx(ctx context.Context, tx *loggedTx, personID int64) error {
	// Relationship writers and dispatch lock the person before touching
	// consent, so keep the same order for date updates.
	if _, err := tx.ExecContext(ctx, `UPDATE persons SET revision = revision WHERE id = ?`, personID); err != nil {
		return fmt.Errorf("lock person for important-date consent: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE important_date_consents SET granted = 0 WHERE subject_uid = 'person/' || (SELECT vcard_uid FROM persons WHERE id = ?)`, personID); err != nil {
		return fmt.Errorf("revoke changed person-date consent: %w", err)
	}
	return nil
}

const personDateSelect = `SELECT
	id, person_id, date_kind, label, date_year, date_month, date_day,
	date_text, calendar_scale, original_value, ` + profileEnvelopeReadColumns + `
	FROM person_dates`

func getPersonDateTx(
	ctx context.Context, tx *loggedTx, personID, id int64,
) (*PersonDate, error) {
	date, err := scanPersonDate(tx.QueryRowContext(ctx,
		personDateSelect+` WHERE person_id = ? AND id = ?`, personID, id,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrProfileValueNotFound
	}
	return date, err
}

func scanPersonDate(row scanner) (*PersonDate, error) {
	var date PersonDate
	var label, dateText, calendarScale sql.NullString
	var year, month, day sql.NullInt64
	var env profileEnvelopeScanValues
	dest := []any{
		&date.Envelope.ID, &date.PersonID, &date.DateKind, &label,
		&year, &month, &day, &dateText, &calendarScale, &date.OriginalValue,
	}
	dest = append(dest, env.destinations()...)
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	date.Label = nullStringPtr(label)
	date.Date = ScanPartialDate(year, month, day)
	date.DateText = nullStringPtr(dateText)
	date.CalendarScale = nullStringPtr(calendarScale)
	if err := env.apply(&date.Envelope); err != nil {
		return nil, err
	}
	return &date, nil
}
