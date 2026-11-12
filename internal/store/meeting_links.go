package store

import (
	"context"
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
	owners := make(map[int64]string)
	for i := range rows {
		owner, ok := owners[rows[i].SourceID]
		if !ok {
			source, err := s.GetSourceByIDContext(ctx, rows[i].SourceID)
			if err != nil {
				return nil, err
			}
			owner = calendarMailbox(source.SyncConfig)
			owners[rows[i].SourceID] = owner
		}
		rows[i].CalendarID = owner
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
