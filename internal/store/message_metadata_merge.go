package store

import (
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"maps"
)

// Called after upsertMessageWith has acquired the message's writer lock, in
// the same transaction. Readers can never observe a partially merged snapshot.
func mergeMessageMetadataWith(q querier, dialect Dialect, messageID int64, incoming sql.NullString) error {
	if !incoming.Valid {
		return nil
	}
	var stored sql.NullString
	if err := q.QueryRow(dialect.Rebind("SELECT metadata FROM messages WHERE id = ?"), messageID).Scan(&stored); err != nil {
		return fmt.Errorf("read message metadata: %w", err)
	}
	fields := make(map[string]jsontext.Value)
	if stored.Valid && stored.String != "" {
		if err := json.Unmarshal([]byte(stored.String), &fields); err != nil || fields == nil {
			// Historical metadata may be damaged or contain a non-object value.
			// Discard partial decoding so valid provider snapshots can repair it.
			fields = make(map[string]jsontext.Value)
		}
	}
	var added map[string]jsontext.Value
	if err := json.Unmarshal([]byte(incoming.String), &added); err != nil {
		return fmt.Errorf("decode incoming message metadata: %w", err)
	}
	maps.Copy(fields, added)
	encoded, err := json.Marshal(fields, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("encode merged message metadata: %w", err)
	}
	return setMessageMetadataWith(q, dialect, messageID, sql.NullString{String: string(encoded), Valid: true})
}
