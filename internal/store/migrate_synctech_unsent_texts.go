package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"

	"go.kenn.io/msgvault/internal/synctechsms/sendstate"
)

// migrationSynctechUnsentTexts marks Synctech texts that never left the phone
// as unsent. Earlier imports recorded them with no send state.
const migrationSynctechUnsentTexts = "synctech_unsent_texts_v1"

const synctechUnsentBatch = 500

type synctechTextRecord struct {
	id          int64
	messageType string
	raw         []byte
	compression sql.NullString
}

// markSynctechUnsentTexts reads each Synctech SMS and MMS row's retained record
// in batches by id and sets is_sent to FALSE where its status says the text
// never left. A row without a readable record stays as it is.
func markSynctechUnsentTexts(ctx context.Context, tx *loggedTx) error {
	var after int64
	for {
		batch, err := readContactLookupRows(ctx, tx, `
			SELECT m.id, m.message_type, r.raw_data, r.compression
			FROM messages m
			JOIN sources src ON src.id = m.source_id
			JOIN message_raw r ON r.message_id = m.id
			WHERE src.source_type = 'synctech_sms' AND r.raw_format = 'synctech_sms_xml_json'
			  AND m.message_type IN ('sms', 'mms') AND m.id > ?
			ORDER BY m.id LIMIT ?`,
			func(row scanner) (*synctechTextRecord, error) {
				var record synctechTextRecord
				err := row.Scan(&record.id, &record.messageType, &record.raw, &record.compression)
				return &record, err
			}, after, synctechUnsentBatch)
		if err != nil {
			return fmt.Errorf("read Synctech texts: %w", err)
		}
		var unsent []int64
		for _, record := range batch {
			if synctechTextUnsent(record) {
				unsent = append(unsent, record.id)
			}
		}
		if err = execInChunksContext(ctx, tx, unsent, nil,
			`UPDATE messages SET is_sent = FALSE WHERE id IN (%s)`); err != nil {
			return fmt.Errorf("mark Synctech texts unsent: %w", err)
		}
		if len(batch) < synctechUnsentBatch {
			return nil
		}
		after = batch[len(batch)-1].id
	}
}

// synctechTextUnsent reads the status from a stored backup record: an SMS has
// its Android type and an MMS its message box.
func synctechTextUnsent(record synctechTextRecord) bool {
	raw, err := decodeMessageRaw(record.raw, record.compression)
	if err != nil {
		return false
	}
	var status struct{ Type, MessageBox int }
	if json.Unmarshal(raw, &status) != nil {
		return false
	}
	if record.messageType == "mms" {
		return sendstate.MMSUnsent(status.MessageBox)
	}
	return sendstate.SMSUnsent(status.Type)
}
