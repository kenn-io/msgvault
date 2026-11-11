package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/mail"
	"strings"
)

const nativeEmailObservationBackfillBatchSize = 256

var errNativeEmailPayloadTooLarge = errors.New("native email payload too large")

type nativeEmailObservationEvidence struct {
	participantID int64
	sourceID      int64
	sourceType    string
	originalValue string
}

// backfillNativeEmailObservationProvenanceContext recovers exact email handles
// from the stored native message payloads and recipient envelopes. Older Apple
// imports wrote participant email_address without source-qualified evidence;
// the policy-aware projections cannot safely use that value once chat data is
// present unless the native source is recorded as an observation.
func (s *Store) backfillNativeEmailObservationProvenanceContext(ctx context.Context) error {
	if err := s.backfillNativeEmailObservationPayloadsContext(ctx); err != nil {
		return err
	}
	return s.backfillNativeEmailObservationEnvelopesContext(ctx)
}

func (s *Store) backfillNativeEmailObservationPayloadsContext(ctx context.Context) error {
	var after int64
	for {
		rows, err := s.db.QueryContext(ctx, `
			SELECT m.id, m.source_id, source.source_type, m.sender_id,
			       raw.raw_data, raw.raw_format, raw.compression
			FROM messages m
			JOIN sources source ON source.id = m.source_id
			JOIN message_raw raw ON raw.message_id = m.id
			WHERE m.id > ? AND m.sender_id IS NOT NULL AND m.is_from_me = FALSE
			  AND ((source.source_type = 'apple_messages' AND raw.raw_format = 'imessage_json')
			       OR (source.source_type = 'imazing_csv' AND raw.raw_format = 'imazing_csv_json'))
			ORDER BY m.id LIMIT ?`, after, nativeEmailObservationBackfillBatchSize)
		if err != nil {
			return fmt.Errorf("read native email payloads: %w", err)
		}
		evidence := make([]nativeEmailObservationEvidence, 0, nativeEmailObservationBackfillBatchSize)
		count := 0
		for rows.Next() {
			var item nativeEmailObservationEvidence
			var compressed []byte
			var rawFormat string
			var compression sql.NullString
			if err := rows.Scan(&after, &item.sourceID, &item.sourceType, &item.participantID,
				&compressed, &rawFormat, &compression); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan native email payload: %w", err)
			}
			count++
			raw, _, err := decodeMessageRawBounded(compressed, compression, 64<<20, errNativeEmailPayloadTooLarge)
			if err != nil {
				continue
			}
			handle, ok := nativeEmailHandleFromPayload(item.sourceType, raw)
			if !ok {
				continue
			}
			item.originalValue = handle
			evidence = append(evidence, item)
		}
		readErr := rows.Err()
		closeErr := rows.Close()
		if readErr != nil {
			return fmt.Errorf("iterate native email payloads: %w", readErr)
		}
		if closeErr != nil {
			return closeErr
		}
		for _, item := range evidence {
			if err := s.recordBackfilledNativeEmailObservationContext(ctx, item); err != nil {
				return err
			}
		}
		if count < nativeEmailObservationBackfillBatchSize {
			return nil
		}
	}
}

func nativeEmailHandleFromPayload(sourceType string, raw []byte) (string, bool) {
	switch sourceType {
	case "apple_messages":
		var payload struct {
			HandleID string `json:"handle_id"`
			FromMe   int    `json:"is_from_me"`
		}
		if json.Unmarshal(raw, &payload) != nil || payload.FromMe != 0 {
			return "", false
		}
		return payload.HandleID, isPlainEmailAddress(payload.HandleID)
	case "imazing_csv":
		var payload struct {
			SenderID string `json:"Sender ID"`
			Type     string `json:"Type"`
		}
		if json.Unmarshal(raw, &payload) != nil || !strings.EqualFold(strings.TrimSpace(payload.Type), "incoming") {
			return "", false
		}
		return payload.SenderID, isPlainEmailAddress(payload.SenderID)
	default:
		return "", false
	}
}

func (s *Store) backfillNativeEmailObservationEnvelopesContext(ctx context.Context) error {
	var after int64
	for {
		rows, err := s.db.QueryContext(ctx, `
			SELECT recipient.id, message.source_id, source.source_type,
			       recipient.participant_id, recipient.email_address
			FROM message_recipients recipient
			JOIN messages message ON message.id = recipient.message_id
			JOIN sources source ON source.id = message.source_id
			WHERE recipient.id > ?
			  AND source.source_type IN ('apple_messages', 'imazing_csv')
			  AND recipient.email_address IS NOT NULL AND recipient.email_address <> ''
			  AND ((message.is_from_me = TRUE AND recipient.recipient_type = 'to')
			       OR (message.is_from_me = FALSE AND recipient.recipient_type = 'from'))
			ORDER BY recipient.id LIMIT ?`, after, nativeEmailObservationBackfillBatchSize)
		if err != nil {
			return fmt.Errorf("read native email recipient envelopes: %w", err)
		}
		evidence := make([]nativeEmailObservationEvidence, 0, nativeEmailObservationBackfillBatchSize)
		count := 0
		for rows.Next() {
			var item nativeEmailObservationEvidence
			if err := rows.Scan(&after, &item.sourceID, &item.sourceType,
				&item.participantID, &item.originalValue); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan native email recipient envelope: %w", err)
			}
			count++
			if isPlainEmailAddress(item.originalValue) {
				evidence = append(evidence, item)
			}
		}
		readErr := rows.Err()
		closeErr := rows.Close()
		if readErr != nil {
			return fmt.Errorf("iterate native email recipient envelopes: %w", readErr)
		}
		if closeErr != nil {
			return closeErr
		}
		for _, item := range evidence {
			if err := s.recordBackfilledNativeEmailObservationContext(ctx, item); err != nil {
				return err
			}
		}
		if count < nativeEmailObservationBackfillBatchSize {
			return nil
		}
	}
}

func (s *Store) recordBackfilledNativeEmailObservationContext(
	ctx context.Context, item nativeEmailObservationEvidence,
) error {
	original := strings.TrimSpace(item.originalValue)
	normalized, ok := normalizePlainEmailAddress(original)
	if !ok {
		return nil
	}
	sourceRef := nativeEmailObservationSourceRef(item.sourceType, original, normalized)
	_, err := s.RecordContactObservationContext(ctx, item.participantID, ParticipantContactObservationInput{
		SourceID: &item.sourceID, AddressKind: ContactAddressEmail, OriginalValue: original,
		Envelope: ValueEnvelopeInput{Source: ProvenanceArchiveObservation, SourceRef: &sourceRef},
	})
	if err != nil {
		return fmt.Errorf("record native email provenance from %s: %w", item.sourceType, err)
	}
	return nil
}

func nativeEmailObservationSourceRef(sourceType, original, normalized string) string {
	switch sourceType {
	case "apple_messages":
		return "imessage:handle:" + strings.TrimSpace(original)
	case "imazing_csv":
		return "imazing_csv:participant:" + nativeEmailObservationStableHash("email", normalized)
	default:
		return ""
	}
}

func nativeEmailObservationStableHash(parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		_, _ = fmt.Fprintf(hash, "%d:", len(part))
		_, _ = hash.Write([]byte(part))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func isPlainEmailAddress(value string) bool {
	_, ok := normalizePlainEmailAddress(value)
	return ok
}

func normalizePlainEmailAddress(value string) (string, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", false
	}
	address, err := mail.ParseAddress(trimmed)
	if err != nil || !strings.Contains(address.Address, "@") || !strings.EqualFold(address.Address, trimmed) {
		return "", false
	}
	return strings.ToLower(address.Address), true
}
