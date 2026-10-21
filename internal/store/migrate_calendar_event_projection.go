package store

import "context"

func (s *Store) backfillCalendarEventProjectionsContext(ctx context.Context) error {
	return s.backfillMessageProjectionsContext(ctx, "calendar_event", "calendar_events", calendarEventProjectionVersion, s.refreshCalendarEventProjectionWith)
}
