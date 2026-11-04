package store

// activityLiveEligibleSQL closes the interval between source edits and projection.
// Callers use the fixed SQL aliases m and src.
func (s *Store) activityLiveEligibleSQL() string {
	status := "m.metadata ->> 'status'"
	superseded := "m.metadata ->> 'ics_superseded'"
	if !s.IsPostgreSQL() {
		status = "CASE WHEN json_valid(m.metadata) THEN m.metadata ->> 'status' END"
		superseded = "CASE WHEN json_valid(m.metadata) THEN m.metadata ->> 'ics_superseded' END"
	}
	return `m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL
 AND COALESCE(m.sent_at,m.received_at,m.internal_date) IS NOT NULL
 AND NOT ` + s.activityUnsentSQL() + `
 AND (COALESCE(m.message_type, '') <> 'calendar_event' OR (
 LOWER(TRIM(COALESCE((SELECT NULLIF(e.status,'decode_error') FROM calendar_events e WHERE e.message_id=m.id),` + status + `,''))) <> 'cancelled'
 AND (EXISTS(SELECT 1 FROM calendar_events e WHERE e.message_id=m.id AND e.status<>'decode_error') OR COALESCE(CAST(` + superseded + ` AS TEXT),'false') NOT IN ('true','1'))))`
}
