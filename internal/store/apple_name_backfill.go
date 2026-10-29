package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
)

// backfillAppleParticipantNamesContext preserves only names the archived Apple
// payload itself attests to. Shared participant display_name values cannot prove
// native provenance. Direct-chat labels, owner names and push names were not
// retained in older payloads; marker-version replay recovers those on reimport.
func (s *Store) backfillAppleParticipantNamesContext(ctx context.Context) error {
	const batchSize = 256
	errTooLarge := errors.New("apple name payload too large")
	var after int64
	for {
		rows, err := s.db.QueryContext(ctx, `SELECT m.id, m.source_id, m.sender_id, r.raw_data, r.compression
   FROM messages m
   JOIN sources source ON source.id = m.source_id
   JOIN message_raw r ON r.message_id = m.id
   WHERE m.id > ? AND source.source_type = 'whatsapp'
     AND r.raw_format = 'whatsapp_apple_json'
     AND m.sender_id IS NOT NULL AND NOT m.is_from_me
   ORDER BY m.id LIMIT ?`, after, batchSize)
		if err != nil {
			return fmt.Errorf("read Apple name provenance: %w", err)
		}
		type evidence struct {
			participant, source int64
			name                string
		}
		var names []evidence
		count := 0
		for rows.Next() {
			var source, participant int64
			var compressed []byte
			var compression sql.NullString
			if err := rows.Scan(&after, &source, &participant, &compressed, &compression); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan Apple name provenance: %w", err)
			}
			count++
			raw, _, err := decodeMessageRawBounded(compressed, compression, 64<<20, errTooLarge)
			if err != nil {
				continue
			}
			var payload struct {
				FromMe         int    `json:"from_me"`
				GroupMemberJID string `json:"group_member_jid"`
				GroupContact   string `json:"group_contact_name"`
				GroupFirst     string `json:"group_first_name"`
			}
			if json.Unmarshal(raw, &payload) != nil || payload.FromMe != 0 || payload.GroupMemberJID == "" {
				continue
			}
			name := strings.TrimSpace(payload.GroupContact)
			if name == "" {
				name = strings.TrimSpace(payload.GroupFirst)
			}
			if name != "" {
				names = append(names, evidence{participant, source, name})
			}
		}
		readErr := rows.Err()
		closeErr := rows.Close()
		if readErr != nil {
			return fmt.Errorf("iterate Apple name provenance: %w", readErr)
		}
		if closeErr != nil {
			return closeErr
		}
		for _, name := range names {
			_, err := s.RecordContactObservationContext(ctx, name.participant, ParticipantContactObservationInput{
				SourceID: &name.source, AddressKind: ContactAddressDisplayName, OriginalValue: name.name,
				Envelope: ValueEnvelopeInput{Source: ProvenanceArchiveObservation, SourceRef: new("whatsapp_apple_json")},
			})
			if err != nil {
				return fmt.Errorf("backfill Apple participant name: %w", err)
			}
		}
		if count < batchSize {
			return nil
		}
	}
}
