package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxPersonAppreciationStateKeys bounds one explicit-key lookup.
const MaxPersonAppreciationStateKeys = 500

var (
	// ErrInvalidPersonAppreciationKey identifies malformed source keys.
	ErrInvalidPersonAppreciationKey = errors.New("invalid appreciation key")
	// ErrTooManyPersonAppreciationKeys identifies a lookup exceeding the bound.
	ErrTooManyPersonAppreciationKeys = errors.New("too many appreciation keys")
	// ErrInvalidPersonAppreciationAudit identifies incomplete dismissal audit fields.
	ErrInvalidPersonAppreciationAudit = errors.New("invalid appreciation dismissal audit")
)

// PersonAppreciationState holds a durable dismissal and an optional exact brief
// reference. A nil reference does not establish whether thanks were given.
type PersonAppreciationState struct {
	ContributionKey    string
	DismissedPersonUID *string
	DismissedBy        *string
	DismissedAt        *time.Time
	// ThanksBriefID is written only by the later validated brief-proof transaction.
	ThanksBriefID *int64
}

func validAppreciationKey(key string) bool {
	if len(key) != 64 {
		return false
	}
	for _, c := range key {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

type appreciationScanner interface{ Scan(...any) error }

func scanAppreciationState(row appreciationScanner) (PersonAppreciationState, error) {
	var state PersonAppreciationState
	var uid, actor sql.NullString
	var at sql.NullTime
	var brief sql.NullInt64
	err := row.Scan(&state.ContributionKey, &uid, &actor, &at, &brief)
	if uid.Valid {
		state.DismissedPersonUID = &uid.String
	}
	if actor.Valid {
		state.DismissedBy = &actor.String
	}
	if at.Valid {
		state.DismissedAt = &at.Time
	}
	if brief.Valid {
		state.ThanksBriefID = &brief.Int64
	}
	return state, err
}

const appreciationStateColumns = `contribution_key, dismissed_person_uid, dismissed_by, dismissed_at, thanks_brief_id`

// GetPersonAppreciationStatesContext returns only existing rows for the supplied
// source keys. It never scans the ledger without an explicit bounded key set.
func (s *Store) GetPersonAppreciationStatesContext(ctx context.Context, keys []string) (map[string]PersonAppreciationState, error) {
	result := make(map[string]PersonAppreciationState)
	if len(keys) > MaxPersonAppreciationStateKeys {
		return nil, fmt.Errorf("%w: maximum %d", ErrTooManyPersonAppreciationKeys, MaxPersonAppreciationStateKeys)
	}
	if len(keys) == 0 {
		return result, nil
	}
	args := make([]any, len(keys))
	for i, key := range keys {
		if !validAppreciationKey(key) {
			return nil, fmt.Errorf("%w at index %d", ErrInvalidPersonAppreciationKey, i)
		}
		args[i] = key
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+appreciationStateColumns+` FROM person_appreciation_state WHERE contribution_key IN (`+strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("read appreciation states: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		state, err := scanAppreciationState(rows)
		if err != nil {
			return nil, fmt.Errorf("scan appreciation state: %w", err)
		}
		result[state.ContributionKey] = state
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read appreciation states: %w", err)
	}
	return result, nil
}

// RecordPersonAppreciationDismissalContext preserves the first audit, including
// when another caller has already inserted a proof-only row.
func (s *Store) RecordPersonAppreciationDismissalContext(ctx context.Context, key, personUID, actor string, at time.Time) (PersonAppreciationState, error) {
	if !validAppreciationKey(key) {
		return PersonAppreciationState{}, fmt.Errorf("dismiss appreciation: %w", ErrInvalidPersonAppreciationKey)
	}
	if strings.TrimSpace(personUID) == "" || strings.TrimSpace(actor) == "" || !utf8.ValidString(personUID) || !utf8.ValidString(actor) || strings.ContainsRune(personUID, '\x00') || strings.ContainsRune(actor, '\x00') || at.IsZero() {
		return PersonAppreciationState{}, fmt.Errorf("dismiss appreciation: %w", ErrInvalidPersonAppreciationAudit)
	}
	var state PersonAppreciationState
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		row := tx.QueryRowContext(ctx, `INSERT INTO person_appreciation_state (contribution_key, dismissed_person_uid, dismissed_by, dismissed_at)
 VALUES (?, ?, ?, ?) ON CONFLICT (contribution_key) DO UPDATE SET
 dismissed_person_uid = CASE WHEN person_appreciation_state.dismissed_at IS NULL THEN excluded.dismissed_person_uid ELSE person_appreciation_state.dismissed_person_uid END,
 dismissed_by = CASE WHEN person_appreciation_state.dismissed_at IS NULL THEN excluded.dismissed_by ELSE person_appreciation_state.dismissed_by END,
 dismissed_at = COALESCE(person_appreciation_state.dismissed_at, excluded.dismissed_at)
 RETURNING `+appreciationStateColumns, key, personUID, actor, at.UTC())
		var err error
		state, err = scanAppreciationState(row)
		return err
	})
	if err != nil {
		return PersonAppreciationState{}, fmt.Errorf("dismiss appreciation: %w", err)
	}
	return state, nil
}
