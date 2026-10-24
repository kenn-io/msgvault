package store

import "context"

func (s *Store) backfillCalendarEventProjectionsContext(ctx context.Context) error {
	return s.backfillMessageProjectionsContext(ctx, MessageTypeCalendarEvent, "calendar_events", calendarEventProjectionVersion, s.refreshCalendarEventProjectionWith)
}
