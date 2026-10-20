package store

import (
	"context"
	"time"

	"go.kenn.io/msgvault/internal/calrecur"
)

// Civil overrides use the calendar zone independently of the timed master's
// recurrence zone, while preserving original identities and effective types.
func linkReadExceptions(ctx context.Context, r rowReader, master CalendarEventRow, rows []CalendarEventRow) ([]calrecur.Exception, error) {
	exceptions := linkExceptions(master, rows)
	var zone *time.Location
	for i := range exceptions {
		e := &exceptions[i]
		if e.StartDate == "" && e.EndDate == "" {
			continue
		}
		if zone == nil {
			var err error
			zone, err = currentCalendarZone(ctx, r, master.SourceID)
			if err != nil {
				return nil, err
			}
		}
		e.Zone = zone
	}
	return exceptions, nil
}
