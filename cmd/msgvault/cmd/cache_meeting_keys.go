package cmd

import (
	"context"
	"fmt"

	"go.kenn.io/msgvault/internal/store"
)

// Read keys after the source snapshot. Only the source snapshot's journal
// boundary is acknowledged, so later decisions remain pending for replay.
func readMeetingCountKeys(ctx context.Context, dbPath string) (map[int64][]store.MeetingCountOccurrence, error) {
	st, err := store.OpenReadOnlyContext(ctx, dbPath)
	if err != nil {
		return nil, fmt.Errorf("open meeting count reader: %w", err)
	}
	defer func() { _ = st.Close() }()
	return st.MeetingCountKeysContext(ctx)
}

func stageMeetingKeys(ctx context.Context, db sqlRunner, keys map[int64][]store.MeetingCountOccurrence, maxMessageID int64) error {
	if _, err := db.ExecContext(ctx, `CREATE TEMP TABLE tmp_meeting_keys (message_id BIGINT, meeting_key VARCHAR, occurrence_at TIMESTAMP, occurrence_date DATE)`); err != nil {
		return fmt.Errorf("create meeting keys: %w", err)
	}
	rows := func(yield func(int64, store.MeetingCountOccurrence) bool) {
		for id, occurrences := range keys {
			if id <= maxMessageID {
				for _, occurrence := range occurrences {
					if !yield(id, occurrence) {
						return
					}
				}
			}
		}
	}
	if err := stageCacheMapping(ctx, db, `INSERT INTO tmp_meeting_keys VALUES `, rows, func(id int64, occurrence store.MeetingCountOccurrence) string {
		at, date := "NULL", "NULL"
		if !occurrence.Start.IsZero() {
			at = "TIMESTAMP '" + occurrence.Start.UTC().Format("2006-01-02 15:04:05.999999999") + "'"
		}
		if occurrence.Date != "" {
			date = "DATE '" + quoteCacheSQL(occurrence.Date) + "'"
		}
		return fmt.Sprintf("(%d,'%s',%s,%s)", id, quoteCacheSQL(occurrence.Key), at, date)
	}); err != nil {
		return fmt.Errorf("stage meeting keys: %w", err)
	}
	return nil
}

const meetingKeysSelectSQL = `SELECT message_id, meeting_key, occurrence_at, occurrence_date FROM tmp_meeting_keys`
