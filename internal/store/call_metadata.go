package store

import (
	"database/sql"
	"fmt"
)

// Call records whose importers store how long an answered call stayed
// connected, so person tiers can count calls as contact.
const (
	MessageTypeSynctechCall    = "synctech_sms_call"
	MessageTypeGoogleVoiceCall = "google_voice_call"
	// CallConnectedSecondsKey is the messages.metadata key for how long an
	// answered call stayed connected. Missed, rejected, and voicemail calls
	// never carry it.
	CallConnectedSecondsKey = "call_connected_seconds"
)

// CallConnectedMetadata is the message metadata for a call that stayed
// connected for seconds, or SQL NULL for a call that never connected.
func CallConnectedMetadata(seconds int) sql.NullString {
	if seconds <= 0 {
		return sql.NullString{}
	}
	return sql.NullString{String: fmt.Sprintf(`{%q:%d}`, CallConnectedSecondsKey, seconds), Valid: true}
}

// callConnectedSecondsSQL reads CallConnectedSecondsKey from m.metadata, or 0.
func (s *Store) callConnectedSecondsSQL() string {
	value := `m.metadata ->> '` + CallConnectedSecondsKey + `'`
	if s.dialect.DriverName() != postgresDriverName {
		value = `CASE WHEN json_valid(m.metadata) THEN m.metadata ->> '` + CallConnectedSecondsKey + `' END`
	}
	return `CAST(COALESCE(` + value + `, '0') AS INTEGER)`
}
