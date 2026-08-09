package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type PersonCategory struct {
	Envelope        ValueEnvelope `json:"envelope"`
	PersonID        int64         `json:"person_id"`
	OriginalValue   string        `json:"original_value"`
	NormalizedValue string        `json:"normalized_value"`
}

type PersonCategoryInput struct {
	OriginalValue string             `json:"original_value"`
	Envelope      ValueEnvelopeInput `json:"envelope"`
}

var (
	ErrPersonCategoryDuplicate = errors.New("person already has this current category")
	ErrPersonCategoryEmpty     = errors.New("person category must be non-empty")
)

func (s *Store) AddPersonCategoryContext(
	ctx context.Context, personID int64, input PersonCategoryInput,
) (*PersonCategory, error) {
	var result *PersonCategory
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		if err := ensureProfilePersonTx(ctx, tx, personID); err != nil {
			return err
		}
		var err error
		result, err = s.addPersonCategoryTx(ctx, tx, personID, input)
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

func (s *Store) ListPersonCategoriesContext(
	ctx context.Context, personID int64, currentOnly bool,
) ([]PersonCategory, error) {
	var categories []PersonCategory
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		var err error
		categories, err = s.listPersonCategoriesTx(ctx, tx, personID, currentOnly)
		return err
	})
	return categories, err
}

func (s *Store) SupersedePersonCategoryContext(
	ctx context.Context, personID, categoryID int64, activeUntil *time.Time,
) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		if err := s.supersedePersonCategoryTx(
			ctx, tx, personID, categoryID, activeUntil,
		); err != nil {
			return err
		}
		return s.bumpPersonRevisionsTx(ctx, tx, personID)
	})
}

func (s *Store) addPersonCategoryTx(
	ctx context.Context, tx *loggedTx, personID int64, input PersonCategoryInput,
) (*PersonCategory, error) {
	original := strings.TrimSpace(input.OriginalValue)
	if original == "" {
		return nil, ErrPersonCategoryEmpty
	}
	normalized := strings.ToLower(original)
	var duplicate int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM person_categories
		WHERE person_id = ? AND normalized_value = ?
		  AND active_until IS NULL AND superseded_at IS NULL`,
		personID, normalized,
	).Scan(&duplicate); err != nil {
		return nil, fmt.Errorf("check person category: %w", err)
	}
	if duplicate > 0 {
		return nil, ErrPersonCategoryDuplicate
	}
	env, err := resolveProfileEnvelopeTx(
		ctx, tx, "person_categories", "", personID, nil, input.Envelope,
	)
	if err != nil {
		return nil, err
	}
	args := []any{personID, original, normalized}
	args = append(args, profileEnvelopeArgs(env)...)
	var id int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO person_categories (
		person_id, original_value, normalized_value, `+profileEnvelopeWriteColumns+`,
		created_at, updated_at
	) VALUES (
		?, ?, ?,
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		`+s.dialect.Now()+`, `+s.dialect.Now()+`
	) RETURNING id`, args...).Scan(&id); err != nil {
		return nil, fmt.Errorf("add person category: %w", err)
	}
	return getPersonCategoryTx(ctx, tx, personID, id)
}

func (s *Store) listPersonCategoriesTx(
	ctx context.Context, tx *loggedTx, personID int64, currentOnly bool,
) ([]PersonCategory, error) {
	query := personCategorySelect + ` WHERE person_id = ?`
	if currentOnly {
		query += ` AND active_until IS NULL AND superseded_at IS NULL`
	}
	query += ` ORDER BY normalized_value,
		CASE WHEN pref IS NULL THEN 1 ELSE 0 END, pref, ordinal, id`
	return queryProfileRowsTx(ctx, tx, query, scanPersonCategory, personID)
}

func (s *Store) supersedePersonCategoryTx(
	ctx context.Context, tx *loggedTx, personID, categoryID int64, activeUntil *time.Time,
) error {
	return s.supersedeProfileValueTx(
		ctx, tx, "person_categories", personID, categoryID, activeUntil,
	)
}

const personCategorySelect = `SELECT
	id, person_id, original_value, normalized_value, ` + profileEnvelopeReadColumns + `
	FROM person_categories`

func getPersonCategoryTx(
	ctx context.Context, tx *loggedTx, personID, id int64,
) (*PersonCategory, error) {
	category, err := scanPersonCategory(tx.QueryRowContext(ctx,
		personCategorySelect+` WHERE person_id = ? AND id = ?`, personID, id,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrProfileValueNotFound
	}
	return category, err
}

func scanPersonCategory(row scanner) (*PersonCategory, error) {
	var category PersonCategory
	var env profileEnvelopeScanValues
	dest := []any{
		&category.Envelope.ID, &category.PersonID,
		&category.OriginalValue, &category.NormalizedValue,
	}
	dest = append(dest, env.destinations()...)
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	if err := env.apply(&category.Envelope); err != nil {
		return nil, err
	}
	return &category, nil
}
