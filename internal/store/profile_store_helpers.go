package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func ensureProfilePersonTx(ctx context.Context, tx *loggedTx, personID int64) error {
	var exists int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM persons WHERE id = ?`, personID,
	).Scan(&exists); err != nil {
		return fmt.Errorf("check person: %w", err)
	}
	if exists == 0 {
		return ErrPersonNotFound
	}
	return nil
}

func nextProfileOrdinalTx(
	ctx context.Context,
	tx *loggedTx,
	table, kindColumn string,
	personID int64,
	kind any,
) (int, error) {
	var ordinal int
	query := fmt.Sprintf(`SELECT COALESCE(MAX(ordinal) + 1, 0)
		FROM %s
		WHERE person_id = ? AND %s = ?
		  AND active_until IS NULL AND superseded_at IS NULL`,
		table, kindColumn,
	)
	if err := tx.QueryRowContext(ctx, query, personID, kind).Scan(&ordinal); err != nil {
		return 0, fmt.Errorf("choose %s ordinal: %w", table, err)
	}
	return ordinal, nil
}

func (s *Store) supersedeProfileValueTx(
	ctx context.Context,
	tx *loggedTx,
	table string,
	personID, valueID int64,
	activeUntil *time.Time,
) error {
	if err := s.validateProfileValueCloseTimeTx(
		ctx, tx, table, "person_id", personID, valueID, activeUntil,
	); err != nil {
		return err
	}
	query := fmt.Sprintf(`UPDATE %s
		SET active_until = COALESCE(?, %s),
		    superseded_at = %s,
		    updated_at = %s
		WHERE id = ? AND person_id = ?
		  AND active_until IS NULL AND superseded_at IS NULL`,
		table, s.dialect.Now(), s.dialect.Now(), s.dialect.Now(),
	)
	result, err := tx.ExecContext(ctx, query, timeValue(activeUntil), valueID, personID)
	if err != nil {
		return fmt.Errorf("supersede %s value: %w", table, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check superseded %s value: %w", table, err)
	}
	if changed == 0 {
		return ErrProfileValueNotFound
	}
	return nil
}

func (s *Store) validateProfileValueCloseTimeTx(
	ctx context.Context,
	tx *loggedTx,
	table, ownerColumn string,
	ownerID, valueID int64,
	activeUntil *time.Time,
) error {
	if activeUntil == nil {
		return nil
	}
	query := fmt.Sprintf(`SELECT active_from FROM %s
		WHERE id = ? AND %s = ?
		  AND active_until IS NULL AND superseded_at IS NULL%s`,
		table, ownerColumn, s.dialect.SelectForUpdate(),
	)
	var activeFrom sql.NullTime
	err := tx.QueryRowContext(ctx, query, valueID, ownerID).Scan(&activeFrom)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrProfileValueNotFound
	}
	if err != nil {
		return fmt.Errorf("read %s active_from: %w", table, err)
	}
	if activeFrom.Valid && activeUntil.Before(activeFrom.Time) {
		return ErrProfileValueCloseBeforeActive
	}
	return nil
}

func queryProfileRowsTx[T any](
	ctx context.Context,
	tx *loggedTx,
	query string,
	scan func(scanner) (*T, error),
	args ...any,
) ([]T, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	values := make([]T, 0)
	for rows.Next() {
		value, err := scan(rows)
		if err != nil {
			return nil, err
		}
		values = append(values, *value)
	}
	return values, rows.Err()
}
