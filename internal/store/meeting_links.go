package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/calrecur"
)

type OccurrenceRef struct {
	MessageID int64
	Identity  calrecur.Identity
}

func (s *Store) meetingEventRows(ctx context.Context, predicate string, args []any, budget *int) ([]CalendarEventRow, error) {
	rows, err := queryCalendarEventRowsWithinBudget(ctx, s.db, predicate, args, *budget)
	if err != nil {
		return nil, err
	}
	*budget -= len(rows)
	owners := make(map[int64]*Source)
	for i := range rows {
		source, ok := owners[rows[i].SourceID]
		if !ok {
			source, err = s.GetSourceByIDContext(ctx, rows[i].SourceID)
			if err != nil {
				return nil, err
			}
			owners[rows[i].SourceID] = source
		}
		rows[i].CalendarID = calendarMailbox(source.SyncConfig)
		if s.IsPostgreSQL() && rows[i].Event.ICalUID != "" {
			// PostgreSQL timestamps have microsecond precision; recover the exact key from the source.
			var raw []byte
			var compression, metadata sql.NullString
			err := s.db.QueryRowContext(ctx, `SELECT r.raw_data, r.compression, m.metadata FROM message_raw r JOIN messages m ON m.id = r.message_id WHERE r.message_id = ?`, rows[i].MessageID).Scan(&raw, &compression, &metadata)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("read calendar meeting evidence: %w", err)
			}
			event, ok := decodeCalendarProjection(raw, compression, []byte(metadata.String), calendarConfigZone(source.SyncConfig))
			if err != nil || !ok || event.ICalUID != rows[i].Event.ICalUID {
				rows[i].Event.ICalUID = ""
				continue
			}
			rows[i].Event.Start, rows[i].Event.End, rows[i].Event.Original = event.Start, event.End, event.Original
		}
	}
	return rows, nil
}

func formatMeetingOccurrence(ref OccurrenceRef) string {
	value := fmt.Sprintf("evt:%d", ref.MessageID)
	if ref.Identity != (calrecur.Identity{}) {
		value += "@" + ref.Identity.Key()
	}
	return value
}
