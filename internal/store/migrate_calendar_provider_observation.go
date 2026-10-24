package store

import "context"

func (s *Store) backfillCalendarProviderObservationOrderContext(ctx context.Context) error {
	return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		if _, err := tx.ExecContext(ctx, `
WITH ranked AS (
    SELECT e.message_id,
           ROW_NUMBER() OVER (ORDER BY m.last_modified NULLS FIRST, e.message_id) AS observation_order
    FROM calendar_events e
    JOIN messages m ON m.id = e.message_id
)
UPDATE calendar_events AS e
SET provider_observation_order = ranked.observation_order
FROM ranked
WHERE ranked.message_id = e.message_id
`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
UPDATE calendar_event_observation_clock
SET observation_order = (SELECT COALESCE(MAX(provider_observation_order), 0) FROM calendar_events)
WHERE singleton = 1
`)
		return err
	})
}
