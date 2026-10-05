package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
)

// ActivityProjectionCursorContext reads a resumable pass's last committed
// message. A changed token (identity epoch or configuration generation)
// starts a new pass, without trusting progress from the previous target.
func (s *Store) ActivityProjectionCursorContext(ctx context.Context, pass, token string) (int64, error) {
	var value string
	err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT value FROM archive_metadata WHERE key = ?`), "activity_spine_cursor_"+pass).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read activity %s cursor: %w", pass, err)
	}
	var progress activityProjectionProgress
	if err := json.Unmarshal([]byte(value), &progress); err != nil {
		return 0, fmt.Errorf("decode activity %s cursor: %w", pass, err)
	}
	if progress.AfterID < 0 {
		return 0, fmt.Errorf("%w: negative activity %s cursor", ErrInvalidActivity, pass)
	}
	if progress.Token != token {
		return 0, nil
	}
	return progress.AfterID, nil
}

type activityProjectionProgress struct {
	Token   string `json:"token"`
	AfterID int64  `json:"after_id"`
}

// SetActivityProjectionCursorContext persists progress after a batch commits.
// A crash before this write only replays that batch; projection is idempotent.
func (s *Store) SetActivityProjectionCursorContext(ctx context.Context, pass, token string, afterID int64) error {
	if afterID < 0 {
		return fmt.Errorf("%w: negative projection cursor", ErrInvalidActivity)
	}
	value, err := json.Marshal(activityProjectionProgress{Token: token, AfterID: afterID})
	if err != nil {
		return fmt.Errorf("encode activity cursor: %w", err)
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		return setArchiveMetadataValueTx(ctx, tx, "activity_spine_cursor_"+pass, string(value))
	})
}
