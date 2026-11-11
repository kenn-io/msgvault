package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

const wakeupStatePrefix = "wakeup:v1:"

type WakeupState struct {
	At            time.Time `json:"at"`
	AfterID       int64     `json:"after_id"`
	AfterRow      bool      `json:"after_row"`
	Failures      int       `json:"failures"`
	NextAttemptAt time.Time `json:"next_attempt_at"`
}

func (s *Store) LoadWakeupState(ctx context.Context, name string) (WakeupState, []byte, error) {
	return s.LoadWakeupStateKey(ctx, wakeupStatePrefix+name)
}

func (s *Store) LoadWakeupStateKey(ctx context.Context, key string) (WakeupState, []byte, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM archive_metadata WHERE key = ?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return WakeupState{}, nil, nil
	}
	if err != nil {
		return WakeupState{}, nil, fmt.Errorf("load wakeup state: %w", err)
	}
	var state WakeupState
	if err = json.Unmarshal([]byte(raw), &state); err != nil {
		return WakeupState{}, nil, fmt.Errorf("decode wakeup state: %w", err)
	}
	return state, []byte(raw), nil
}

// EncodeWakeupState returns the exact bytes used by the state CAS.
func EncodeWakeupState(state WakeupState) ([]byte, error) { return json.Marshal(state) }

func (s *Store) AdvanceWakeupState(ctx context.Context, name string, before []byte, after WakeupState) (bool, error) {
	return s.AdvanceWakeupStateKey(ctx, wakeupStatePrefix+name, before, after)
}

func (s *Store) AdvanceWakeupStateKey(ctx context.Context, key string, before []byte, after WakeupState) (bool, error) {
	value, err := EncodeWakeupState(after)
	if err != nil {
		return false, err
	}
	var swapped bool
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		var result sql.Result
		var err error
		if before == nil {
			result, err = q.Exec(`INSERT INTO archive_metadata (key,value) VALUES (?,?) ON CONFLICT (key) DO NOTHING`, key, string(value))
		} else {
			result, err = q.Exec(`UPDATE archive_metadata SET value=? WHERE key=? AND value=?`, string(value), key, string(before))
		}
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		swapped = n == 1
		return err
	})
	if err != nil {
		return false, fmt.Errorf("advance wakeup state: %w", err)
	}
	return swapped, nil
}

func (s *Store) PruneWakeupStates(ctx context.Context, keep []string) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key FROM archive_metadata WHERE key LIKE ?`, wakeupStatePrefix+"%")
	if err != nil {
		return 0, fmt.Errorf("list wakeup states: %w", err)
	}
	var remove []string
	for rows.Next() {
		var key string
		if err = rows.Scan(&key); err != nil {
			break
		}
		if !slices.Contains(keep, strings.TrimPrefix(key, wakeupStatePrefix)) {
			remove = append(remove, key)
		}
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return 0, err
	}
	count := 0
	for _, key := range remove {
		result, err := s.db.ExecContext(ctx, `DELETE FROM archive_metadata WHERE key=?`, key)
		if err != nil {
			return count, fmt.Errorf("prune wakeup state: %w", err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return count, err
		}
		count += int(n)
	}
	return count, nil
}
