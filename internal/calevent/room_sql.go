package calevent

// RoomAddressSQL is the portable SQL counterpart of IsRoomAttendee. email and
// explicitEvidence are trusted SQL expressions, never user input.
func RoomAddressSQL(email, explicitEvidence string) string {
	value := "lower(trim(coalesce(" + email + ", '')))"
	return "(" + explicitEvidence + " OR (" + value + " NOT LIKE '%@%@%' AND (" + value + " LIKE '%@resource.calendar.google.com' OR " + value + " LIKE '%@%.resource.calendar.google.com')))"
}
