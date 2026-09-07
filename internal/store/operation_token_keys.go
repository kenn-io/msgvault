package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"
)

type OperationTokenKeyState string

const (
	OperationTokenKeyActive      OperationTokenKeyState = "active"
	OperationTokenKeyDecryptOnly OperationTokenKeyState = "decrypt_only"
)

var (
	ErrOperationTokenKeyNotFound = errors.New("operation token key not found")
	operationTokenKeyIDPattern   = regexp.MustCompile(`^[a-f0-9]{32}$`)
)

type OperationTokenKey struct {
	KeyID     string
	KeyBytes  []byte
	State     OperationTokenKeyState
	CreatedAt time.Time
	RetiredAt *time.Time
}

func (s *Store) ActiveOperationTokenKey(ctx context.Context) (OperationTokenKey, error) {
	key, err := s.readActiveOperationTokenKey(ctx, s.db)
	if err == nil {
		return key, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return OperationTokenKey{}, fmt.Errorf("read active operation token key: %w", err)
	}
	candidate, err := newOperationTokenKey(time.Now().UTC())
	if err != nil {
		return OperationTokenKey{}, err
	}
	var active OperationTokenKey
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		_, insertErr := tx.ExecContext(ctx, `INSERT INTO operation_token_keys
			(key_id, key_bytes, state, created_at) VALUES (?, ?, 'active', ?)
			ON CONFLICT DO NOTHING`, candidate.KeyID, candidate.KeyBytes,
			s.dialect.TimestampParam(candidate.CreatedAt))
		if insertErr != nil {
			return fmt.Errorf("create active operation token key: %w", insertErr)
		}
		var readErr error
		active, readErr = s.readActiveOperationTokenKey(ctx, tx)
		return readErr
	})
	return cloneOperationTokenKey(active), err
}

func (s *Store) OperationTokenKey(ctx context.Context, keyID string) (OperationTokenKey, error) {
	if !operationTokenKeyIDPattern.MatchString(keyID) {
		return OperationTokenKey{}, ErrOperationTokenKeyNotFound
	}
	key, err := scanOperationTokenKey(s.db.QueryRowContext(ctx, s.dialect.Rebind(`
		SELECT key_id, key_bytes, state, created_at, retired_at
		FROM operation_token_keys WHERE key_id = ?`), keyID))
	if errors.Is(err, sql.ErrNoRows) {
		return OperationTokenKey{}, ErrOperationTokenKeyNotFound
	}
	if err != nil {
		return OperationTokenKey{}, fmt.Errorf("read operation token key: %w", err)
	}
	return cloneOperationTokenKey(key), nil
}

func (s *Store) RotateOperationTokenKey(ctx context.Context, rotatedAt time.Time) (OperationTokenKey, error) {
	if rotatedAt.IsZero() {
		return OperationTokenKey{}, errors.New("operation token key rotation time is required")
	}
	rotatedAt = rotatedAt.UTC()
	candidate, err := newOperationTokenKey(rotatedAt)
	if err != nil {
		return OperationTokenKey{}, err
	}
	var active OperationTokenKey
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE operation_token_keys
			SET state = 'decrypt_only', retired_at = ? WHERE state = 'active'`,
			s.dialect.TimestampParam(rotatedAt)); err != nil {
			return fmt.Errorf("retire active operation token key: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO operation_token_keys
			(key_id, key_bytes, state, created_at) VALUES (?, ?, 'active', ?)`,
			candidate.KeyID, candidate.KeyBytes, s.dialect.TimestampParam(candidate.CreatedAt)); err != nil {
			return fmt.Errorf("create rotated operation token key: %w", err)
		}
		var readErr error
		active, readErr = s.readActiveOperationTokenKey(ctx, tx)
		return readErr
	})
	return cloneOperationTokenKey(active), err
}

func (s *Store) DeleteOperationTokenKey(ctx context.Context, keyID string) error {
	if !operationTokenKeyIDPattern.MatchString(keyID) {
		return ErrOperationTokenKeyNotFound
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		result, err := tx.ExecContext(ctx,
			`DELETE FROM operation_token_keys WHERE key_id = ? AND state = 'decrypt_only'`, keyID)
		if err != nil {
			return fmt.Errorf("delete retired operation token key: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows == 1 {
			return nil
		}
		var state OperationTokenKeyState
		err = tx.QueryRowContext(ctx, `SELECT state FROM operation_token_keys WHERE key_id = ?`, keyID).Scan(&state)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrOperationTokenKeyNotFound
		}
		if err != nil {
			return err
		}
		return errors.New("active operation token key cannot be deleted")
	})
}

func (s *Store) readActiveOperationTokenKey(ctx context.Context, queryer contextRowQuerier) (OperationTokenKey, error) {
	query := `SELECT key_id, key_bytes, state, created_at, retired_at
		FROM operation_token_keys WHERE state = 'active'`
	if queryer == s.db {
		query = s.dialect.Rebind(query)
	}
	return scanOperationTokenKey(queryer.QueryRowContext(ctx, query))
}

func scanOperationTokenKey(sc scanner) (OperationTokenKey, error) {
	var key OperationTokenKey
	var created requiredTimestamp
	var retired strictOperationNullableTimestamp
	if err := sc.Scan(&key.KeyID, &key.KeyBytes, &key.State, &created, &retired); err != nil {
		return OperationTokenKey{}, err
	}
	if !operationTokenKeyIDPattern.MatchString(key.KeyID) || len(key.KeyBytes) != 32 {
		return OperationTokenKey{}, errors.New("operation token key row is invalid")
	}
	key.CreatedAt = created.Time.UTC()
	if retired.Valid {
		value := retired.Time.UTC()
		key.RetiredAt = &value
	}
	if (key.State == OperationTokenKeyActive && key.RetiredAt != nil) ||
		(key.State == OperationTokenKeyDecryptOnly && key.RetiredAt == nil) {
		return OperationTokenKey{}, errors.New("operation token key lifecycle is invalid")
	}
	return cloneOperationTokenKey(key), nil
}

func newOperationTokenKey(createdAt time.Time) (OperationTokenKey, error) {
	keyIDBytes := make([]byte, 16)
	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyIDBytes); err != nil {
		return OperationTokenKey{}, fmt.Errorf("generate operation token key ID: %w", err)
	}
	if _, err := rand.Read(keyBytes); err != nil {
		return OperationTokenKey{}, fmt.Errorf("generate operation token key: %w", err)
	}
	return OperationTokenKey{
		KeyID: hex.EncodeToString(keyIDBytes), KeyBytes: keyBytes,
		State: OperationTokenKeyActive, CreatedAt: createdAt.UTC(),
	}, nil
}

func cloneOperationTokenKey(key OperationTokenKey) OperationTokenKey {
	key.KeyBytes = append([]byte(nil), key.KeyBytes...)
	if key.RetiredAt != nil {
		value := *key.RetiredAt
		key.RetiredAt = &value
	}
	return key
}
