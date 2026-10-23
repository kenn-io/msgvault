package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/synctechstate"
)

const synctechMigrationPageSize = 500

// migrateSynctechUnsentMessages marks provider-native drafts and unsent SMS/MMS
// rows so live-message readers can exclude them. Synctech imports preserve the
// source record as compressed JSON, which lets this migration repair archives
// written before the importer stored is_sent.
func (s *Store) migrateSynctechUnsentMessages(ctx context.Context) error {
	return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		var after int64
		for {
			rows, err := tx.QueryContext(ctx, `
				SELECT m.id, m.message_type, r.raw_data, r.compression
				FROM messages m
			JOIN sources src ON src.id = m.source_id AND src.source_type = 'synctech_sms'
			LEFT JOIN message_raw r ON r.message_id = m.id AND r.raw_format = ?
			WHERE m.id > ? AND m.message_type IN ('sms', 'mms')
			  AND (m.is_sent IS NULL OR m.is_sent = TRUE)
			ORDER BY m.id LIMIT ?`, synctechstate.RawFormat, after, synctechMigrationPageSize)
			if err != nil {
				return fmt.Errorf("list Synctech messages for send-state migration: %w", err)
			}
			var count int
			var unsent []int64
			for rows.Next() {
				var id int64
				var messageType string
				var raw []byte
				var compression sql.NullString
				if err := rows.Scan(&id, &messageType, &raw, &compression); err != nil {
					_ = rows.Close()
					return fmt.Errorf("scan Synctech message for send-state migration: %w", err)
				}
				count++
				after = id
				if len(raw) == 0 {
					continue
				}
				raw, err = decodeMessageRaw(raw, compression)
				if err != nil {
					continue
				}
				if synctechRawRecordIsUnsent(messageType, raw) {
					unsent = append(unsent, id)
				}
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				return fmt.Errorf("read Synctech messages for send-state migration: %w", err)
			}
			if err := rows.Close(); err != nil {
				return fmt.Errorf("close Synctech messages for send-state migration: %w", err)
			}
			if err := markSynctechMessagesUnsent(ctx, tx, unsent); err != nil {
				return err
			}
			if count < synctechMigrationPageSize {
				return nil
			}
		}
	})
}

func synctechRawRecordIsUnsent(messageType string, raw []byte) bool {
	switch messageType {
	case "sms":
		var record struct {
			Type int `json:"Type"`
		}
		if json.Unmarshal(raw, &record) != nil {
			return false
		}
		return !synctechstate.SMSWasSent(record.Type)
	case "mms":
		var record struct {
			MessageBox int `json:"MessageBox"`
		}
		if json.Unmarshal(raw, &record) != nil {
			return false
		}
		return !synctechstate.MMSWasSent(record.MessageBox)
	}
	return false
}

func markSynctechMessagesUnsent(ctx context.Context, tx *loggedTx, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET is_sent = FALSE WHERE id IN (`+placeholders+`)`, appendInt64Args(nil, ids)...); err != nil {
		return fmt.Errorf("mark Synctech drafts and unsent messages: %w", err)
	}
	return nil
}
