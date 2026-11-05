package store

// activityLiveEligibleSQL closes the interval between source edits and projection.
// Queries use m for messages and src for sources.
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
 AND (m.message_type <> 'calendar_event' OR (
 LOWER(TRIM(COALESCE((SELECT NULLIF(e.status,'decode_error') FROM calendar_events e WHERE e.message_id=m.id),` + status + `,''))) <> 'cancelled'
 AND (EXISTS(SELECT 1 FROM calendar_events e WHERE e.message_id=m.id AND e.status<>'decode_error') OR COALESCE(CAST(` + superseded + ` AS TEXT),'false') NOT IN ('true','1'))))`
}

// activityInvitationSQL reports meeting records and email invitations, which
// stay visible when an excluded sender would otherwise make them noise.
// Queries use ae for activity events and m for messages.
func (s *Store) activityInvitationSQL() string {
	flag := "m.metadata ->> '" + emailInvitationMetadataKey + "'"
	if !s.IsPostgreSQL() {
		flag = "CASE WHEN json_valid(m.metadata) THEN " + flag + " END"
	}
	return `CASE WHEN ae.ref_kind='meeting' OR COALESCE(CAST(` + flag + ` AS TEXT),'false') IN ('true','1') THEN 1 ELSE 0 END`
}
