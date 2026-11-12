package cmd

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/identityindex"
	"go.kenn.io/msgvault/internal/store"
)

func stageMeetingOccurrences(ctx context.Context, db sqlRunner, dbPath, root string, through time.Time) error {
	archive, err := store.OpenReadOnlyContext(ctx, dbPath)
	if err != nil {
		return fmt.Errorf("open archive for meeting occurrences: %w", err)
	}
	defer func() { _ = archive.Close() }()
	occurrences, err := archive.MeetingOccurrencesContext(ctx, through)
	if err != nil {
		return fmt.Errorf("read meeting occurrences: %w", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TEMP TABLE tmp_meeting_occurrences (message_id BIGINT, occurrence_key VARCHAR, occurred_at TIMESTAMP, counted BOOLEAN, declined_participant_ids BIGINT[])`); err != nil {
		return fmt.Errorf("create meeting occurrence table: %w", err)
	}
	defer func() {
		// The temporary table must be removed even if the build is cancelled.
		_, _ = db.ExecContext(context.WithoutCancel(ctx), `DROP TABLE tmp_meeting_occurrences`)
	}()
	var values []string
	var args []any
	flush := func() error {
		if len(values) == 0 {
			return nil
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO tmp_meeting_occurrences VALUES `+strings.Join(values, ","), args...); err != nil {
			return fmt.Errorf("stage meeting occurrences: %w", err)
		}
		values, args = values[:0], args[:0]
		return nil
	}
	for _, occurrence := range occurrences {
		ids := make([]string, 0, len(occurrence.DeclinedParticipantIDs))
		for _, id := range occurrence.DeclinedParticipantIDs {
			ids = append(ids, strconv.FormatInt(id, 10))
		}
		values = append(values, `(?, ?, ?, ?, [`+strings.Join(ids, ",")+`]::BIGINT[])`)
		args = append(args, occurrence.MessageID, occurrence.OccurrenceKey, occurrence.OccurredAt, occurrence.Counted)
		if len(values) >= participantClusterMaxBatchRows {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}
	return copyParquet(ctx, db, filepath.Join(root, identityindex.DatasetMeetingOccurrences), "meeting_occurrences.parquet", `SELECT * FROM tmp_meeting_occurrences`)
}
