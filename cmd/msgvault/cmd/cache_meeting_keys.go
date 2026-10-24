package cmd

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/store"
)

// Read keys after the source snapshot. Only the source snapshot's journal
// boundary is acknowledged, so later decisions remain pending for replay.
func readMeetingCountKeys(ctx context.Context, dbPath string) (map[int64]string, error) {
	st, err := store.OpenReadOnlyContext(ctx, dbPath)
	if err != nil {
		return nil, fmt.Errorf("open meeting count reader: %w", err)
	}
	defer func() { _ = st.Close() }()
	keys, err := st.MeetingCountKeysContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("read meeting count keys: %w", err)
	}
	return keys, nil
}

func stageMeetingKeys(ctx context.Context, db sqlRunner, keys map[int64]string, maxMessageID int64) error {
	if _, err := db.ExecContext(ctx, `CREATE TEMP TABLE tmp_meeting_keys (message_id BIGINT, meeting_key VARCHAR)`); err != nil {
		return fmt.Errorf("create meeting keys: %w", err)
	}
	values := make([]string, 0, len(keys))
	for id, key := range keys {
		if id <= maxMessageID {
			values = append(values, fmt.Sprintf("(%d,'%s')", id, quoteCacheSQL(key)))
		}
	}
	if len(values) > 0 {
		if _, err := db.ExecContext(ctx, `INSERT INTO tmp_meeting_keys VALUES `+strings.Join(values, ",")); err != nil {
			return fmt.Errorf("stage meeting keys: %w", err)
		}
	}
	return nil
}

const meetingKeysSelectSQL = `SELECT message_id, meeting_key FROM tmp_meeting_keys`
