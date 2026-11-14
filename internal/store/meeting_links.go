package store

import (
	"context"
	"database/sql"
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
	var precise []int
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
			precise = append(precise, i)
		}
	}
	// PostgreSQL timestamps have microsecond precision; recover exact evidence in bounded batches.
	for start := 0; start < len(precise); start += 500 {
		batch := precise[start:min(start+500, len(precise))]
		args := make([]any, 0, len(batch))
		indexes := make(map[int64]int, len(batch))
		for _, i := range batch {
			args = append(args, rows[i].MessageID)
			indexes[rows[i].MessageID] = i
		}
		rawRows, err := s.db.QueryContext(ctx, `SELECT r.message_id, r.raw_data, r.compression, m.metadata FROM message_raw r JOIN messages m ON m.id = r.message_id WHERE r.message_id IN (`+placeholders(len(args))+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("read calendar meeting evidence: %w", err)
		}
		for rawRows.Next() {
			var id int64
			var raw []byte
			var compression, metadata sql.NullString
			if err := rawRows.Scan(&id, &raw, &compression, &metadata); err != nil {
				_ = rawRows.Close()
				return nil, fmt.Errorf("scan calendar meeting evidence: %w", err)
			}
			i := indexes[id]
			delete(indexes, id)
			event, ok := decodeCalendarProjection(raw, compression, []byte(metadata.String), calendarConfigZone(owners[rows[i].SourceID].SyncConfig))
			if !ok || event.ICalUID != rows[i].Event.ICalUID {
				rows[i].Event.ICalUID = ""
				continue
			}
			rows[i].Event.Start, rows[i].Event.End, rows[i].Event.Original = event.Start, event.End, event.Original
		}
		if err := rawRows.Err(); err != nil {
			_ = rawRows.Close()
			return nil, fmt.Errorf("read calendar meeting evidence: %w", err)
		}
		if err := rawRows.Close(); err != nil {
			return nil, fmt.Errorf("close calendar meeting evidence: %w", err)
		}
		for _, i := range indexes {
			rows[i].Event.ICalUID = ""
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
