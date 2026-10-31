package store

import "strings"

// activityLiveEligibleSQL closes the interval between source edits and projection.
// Aliases are internal SQL identifiers, never user input.
func (s *Store) activityLiveEligibleSQL(m, src string) string {
	status := "m.metadata ->> 'status'"
	superseded := "m.metadata ->> 'ics_superseded'"
	if !s.IsPostgreSQL() {
		status = "CASE WHEN json_valid(m.metadata) THEN m.metadata ->> 'status' END"
		superseded = "CASE WHEN json_valid(m.metadata) THEN m.metadata ->> 'ics_superseded' END"
	}
	query := `m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL
 AND COALESCE(m.sent_at,m.received_at,m.internal_date) IS NOT NULL
 AND NOT ` + s.activityUnsentSQL() + `
 AND (m.message_type <> 'calendar_event' OR (
 LOWER(TRIM(COALESCE((SELECT NULLIF(e.status,'decode_error') FROM calendar_events e WHERE e.message_id=m.id),` + status + `,''))) <> 'cancelled'
 AND (EXISTS(SELECT 1 FROM calendar_events e WHERE e.message_id=m.id AND e.status<>'decode_error') OR COALESCE(CAST(` + superseded + ` AS TEXT),'false') NOT IN ('true','1'))))`
	return strings.NewReplacer("m.", m+".", "src.", src+".").Replace(query)
}
