package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/callhistory"
)

// GetDiscordCallRecordContext reads one visible Discord payload by exact archive
// primary key. Stored and inflated bytes are bounded; message bodies are unused.
func (s *Store) GetDiscordCallRecordContext(ctx context.Context, messageID int64) ([]byte, error) {
	if messageID <= 0 {
		return nil, fmt.Errorf("%w: Discord archive message ID", callhistory.ErrInvalid)
	}
	var raw []byte
	var compression sql.NullString
	var size int64
	err := s.db.QueryRowContext(ctx, s.Rebind(`
  SELECT CASE WHEN length(r.raw_data) <= ? THEN r.raw_data END,
         r.compression, length(r.raw_data)
  FROM messages m
  JOIN sources s ON s.id = m.source_id
  JOIN message_raw r ON r.message_id = m.id
  WHERE m.id = ? AND s.source_type = 'discord' AND r.raw_format = 'discord_json'
    AND `+LiveMessagesWhere("m", true)), callhistory.MaxRawBytes, messageID).Scan(&raw, &compression, &size)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: live archived Discord JSON not found", callhistory.ErrInvalid)
	}
	if err != nil {
		return nil, fmt.Errorf("read archived Discord call: %w", err)
	}
	if size > callhistory.MaxRawBytes {
		return nil, callhistory.ErrImportTooLarge
	}
	data, _, err := decodeMessageRawBounded(raw, compression, callhistory.MaxRawBytes, callhistory.ErrImportTooLarge)
	if err != nil && !errors.Is(err, callhistory.ErrImportTooLarge) {
		return nil, fmt.Errorf("%w: decode archived Discord JSON", callhistory.ErrInvalid)
	}
	return data, err
}
