package store

import (
	"context"
	"fmt"
)

func (s *Store) ensureCalendarEventRecurrenceDurationColumn(ctx context.Context) error {
	query := `SELECT EXISTS (
		SELECT 1 FROM pragma_table_info('calendar_events')
		WHERE name = 'recurrence_duration_days'
	)`
	if s.IsPostgreSQL() {
		query = `SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = current_schema()
			  AND table_name = 'calendar_events'
			  AND column_name = 'recurrence_duration_days'
		)`
	}
	var exists bool
	if err := s.db.QueryRowContext(ctx, query).Scan(&exists); err != nil {
		return fmt.Errorf("inspect calendar event recurrence duration column: %w", err)
	}
	if exists {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE calendar_events ADD COLUMN recurrence_duration_days INTEGER NOT NULL DEFAULT 0`); err != nil && !s.dialect.IsDuplicateColumnError(err) {
		return fmt.Errorf("add calendar event recurrence duration column: %w", err)
	}
	return nil
}
