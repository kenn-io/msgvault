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
			type candidate struct {
				id          int64
				messageType string
				raw         []byte
				compression sql.NullString
			}
			var page []candidate
			for rows.Next() {
				var item candidate
				if err := rows.Scan(&item.id, &item.messageType, &item.raw, &item.compression); err != nil {
					_ = rows.Close()
					return fmt.Errorf("scan Synctech message for send-state migration: %w", err)
				}
				page = append(page, item)
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				return fmt.Errorf("read Synctech messages for send-state migration: %w", err)
			}
			if err := rows.Close(); err != nil {
				return fmt.Errorf("close Synctech messages for send-state migration: %w", err)
			}
			if len(page) == 0 {
				return nil
			}
			after = page[len(page)-1].id

			var unsent []int64
			for _, item := range page {
				if len(item.raw) == 0 {
					continue
				}
				raw, err := decodeMessageRaw(item.raw, item.compression)
				if err != nil {
					continue
				}
				if synctechRawRecordIsUnsent(item.messageType, raw) {
					unsent = append(unsent, item.id)
				}
			}
			if err := markSynctechMessagesUnsent(ctx, tx, unsent); err != nil {
				return err
			}
			if len(page) < synctechMigrationPageSize {
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
	for start := 0; start < len(ids); start += synctechMigrationPageSize {
		end := min(start+synctechMigrationPageSize, len(ids))
		page := ids[start:end]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(page)), ",")
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET is_sent = FALSE WHERE id IN (`+placeholders+`)`, int64Args(page)...); err != nil {
			return fmt.Errorf("mark Synctech drafts and unsent messages: %w", err)
		}
	}
	return nil
}

func int64Args(ids []int64) []any {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return args
}
