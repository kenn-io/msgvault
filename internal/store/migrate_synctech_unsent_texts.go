package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"

	"go.kenn.io/msgvault/internal/synctechsms/sendstate"
)

// migrationSynctechUnsentTexts marks Synctech texts that never left the phone
// as unsent. Imports before this change recorded drafts, outbox, queued, and
// failed texts as owner-authored with no send state, so they counted as
// messages sent.
const migrationSynctechUnsentTexts = "synctech_unsent_texts_v1"

const synctechUnsentBackfillBatch = 500

// markSynctechUnsentTexts walks the Synctech SMS and MMS rows that keep their
// raw backup record, in id order, and sets is_sent to FALSE where the record's
// status says the text never left. A row with no readable record stays as it
// is.
func markSynctechUnsentTexts(ctx context.Context, tx *loggedTx) error {
	var after int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT m.id, m.message_type, r.raw_data, r.compression
			FROM messages m
			JOIN sources src ON src.id = m.source_id
			JOIN message_raw r ON r.message_id = m.id
			WHERE src.source_type = 'synctech_sms'
			  AND r.raw_format = 'synctech_sms_xml_json'
			  AND m.message_type IN ('sms', 'mms')
			  AND m.id > ?
			ORDER BY m.id
			LIMIT ?
		`, after, synctechUnsentBackfillBatch)
		if err != nil {
			return fmt.Errorf("list Synctech texts: %w", err)
		}
		var (
			scanned int
			unsent  []int64
		)
		for rows.Next() {
			var (
				id          int64
				messageType string
				raw         []byte
				compression sql.NullString
			)
			if err := rows.Scan(&id, &messageType, &raw, &compression); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan Synctech text: %w", err)
			}
			after = id
			scanned++
			if synctechTextUnsent(messageType, raw, compression) {
				unsent = append(unsent, id)
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("iterate Synctech texts: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close Synctech texts: %w", err)
		}
		if err := execInChunksContext(ctx, tx, unsent, nil,
			`UPDATE messages SET is_sent = FALSE WHERE id IN (%s)`); err != nil {
			return fmt.Errorf("mark Synctech texts unsent: %w", err)
		}
		if scanned < synctechUnsentBackfillBatch {
			return nil
		}
	}
}

// synctechTextUnsent reads the status from a stored backup record: an SMS
// carries its Android type and an MMS its message box.
func synctechTextUnsent(messageType string, raw []byte, compression sql.NullString) bool {
	raw, err := decodeMessageRaw(raw, compression)
	if err != nil {
		return false
	}
	var record struct {
		Type       int
		MessageBox int
	}
	if json.Unmarshal(raw, &record) != nil {
		return false
	}
	if messageType == "mms" {
		return sendstate.MMSUnsent(record.MessageBox)
	}
	return sendstate.SMSUnsent(record.Type)
}
