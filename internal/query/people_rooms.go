package query

import "go.kenn.io/msgvault/internal/calevent"

func (e *DuckDBEngine) roomAddressSQL(email string) string {
	return calevent.RoomAddressSQL(email, `EXISTS (SELECT 1 FROM read_parquet('`+quoteIdentitySQLPath(e.parquetPath("room_addresses"))+`') room WHERE room.email = lower(trim(`+email+`)))`)
}

func (e *DuckDBEngine) observedRoomOnlySQL(alias string) string {
	identifiers := `read_parquet('` + quoteIdentitySQLPath(e.parquetPath(datasetParticipantIdentifiers)) + `')`
	contacts := `SELECT 'email' AS kind, ` + alias + `.email_address AS value WHERE trim(coalesce(` + alias + `.email_address,'')) <> '' UNION ALL SELECT 'phone', ` + alias + `.phone_number WHERE trim(coalesce(` + alias + `.phone_number,'')) <> '' UNION ALL SELECT pi.identifier_type,pi.identifier_value FROM ` + identifiers + ` pi WHERE pi.participant_id=` + alias + `.id AND trim(pi.identifier_value) <> ''`
	return `(EXISTS(SELECT 1 FROM (` + contacts + `) observed_room_contacts) AND NOT EXISTS(SELECT 1 FROM (` + contacts + `) observed_room_contacts WHERE kind <> 'email' OR NOT ` + e.roomAddressSQL("observed_room_contacts.value") + `))`
}

// personRoomOnlySQL includes curated contacts for members that have no activity
// and therefore do not appear in the cached people population.
func (e *DuckDBEngine) personRoomOnlySQL(alias string) string {
	identifiers := `read_parquet('` + quoteIdentitySQLPath(e.parquetPath(datasetParticipantIdentifiers)) + `')`
	contacts := `SELECT 'email' AS kind, ` + alias + `.email_address AS value WHERE trim(coalesce(` + alias + `.email_address,'')) <> '' UNION ALL SELECT 'phone', ` + alias + `.phone_number WHERE trim(coalesce(` + alias + `.phone_number,'')) <> '' UNION ALL SELECT pi.identifier_type,pi.identifier_value FROM ` + identifiers + ` pi WHERE pi.participant_id=` + alias + `.id AND trim(pi.identifier_value) <> '' UNION ALL SELECT pc.address_kind, pc.normalized_value FROM read_parquet('` + quoteIdentitySQLPath(e.parquetPath("person_contacts")) + `') pc WHERE pc.participant_id=` + alias + `.id AND trim(pc.normalized_value) <> ''`
	return `(EXISTS(SELECT 1 FROM (` + contacts + `) person_room_contacts) AND NOT EXISTS(SELECT 1 FROM (` + contacts + `) person_room_contacts WHERE kind <> 'email' OR NOT ` + e.roomAddressSQL("person_room_contacts.value") + `))`
}

func (e *DuckDBEngine) roomNameSuppressedSQL(alias string) string {
	return "(" + e.observedRoomOnlySQL(alias) + " OR " + e.personRoomOnlySQL(alias) + ")"
}
