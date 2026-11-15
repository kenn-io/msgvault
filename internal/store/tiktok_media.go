package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
)

// TikTokMediaSweep is independent of the provider's shallow pull cursor. A
// fixed upper bound makes each pass finite even while new messages arrive.
type TikTokMediaSweep struct {
	Version int    `json:"version"`
	Source  string `json:"source"`
	After   int64  `json:"after"`
	Through int64  `json:"through"`
}

func tikTokMediaSweepKey(sourceID int64) string {
	return fmt.Sprintf("tiktok.media_sweep:%d", sourceID)
}

func (s *Store) LoadTikTokMediaSweepContext(ctx context.Context, sourceID int64, identifier string) (TikTokMediaSweep, error) {
	state := TikTokMediaSweep{Version: 1, Source: identifier}
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM archive_metadata WHERE key = ?`, tikTokMediaSweepKey(sourceID)).Scan(&value)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return state, err
	}
	if err == nil {
		state = TikTokMediaSweep{}
		if json.Unmarshal([]byte(value), &state) != nil || state.Version != 1 || state.Source != identifier || state.After < 0 || state.Through < state.After {
			return state, errors.New("invalid TikTok media sweep progress")
		}
	}
	if state.After == state.Through {
		state.After = 0
		err = s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM messages WHERE source_id = ?`, sourceID).Scan(&state.Through)
	}
	return state, err
}

func (s *Store) SaveTikTokMediaSweepContext(ctx context.Context, sourceID int64, state TikTokMediaSweep) error {
	value, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return s.withSyncSourceWriteContext(ctx, sourceID, func(q querier) error {
		_, err := q.Exec(`INSERT INTO archive_metadata (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, tikTokMediaSweepKey(sourceID), string(value))
		return err
	})
}

const tikTokPendingMediaWhere = `m.source_id = ? AND m.message_type = 'tiktok_messages'
 AND NOT EXISTS (SELECT 1 FROM source_merge_archive_only_messages am WHERE am.message_id = m.id)
 AND EXISTS (SELECT 1 FROM attachments a WHERE a.message_id = m.id
  AND a.source_part_key IN ('tiktok:image', 'tiktok:video', 'tiktok:sticker', 'tiktok:emoji')
  AND a.attachment_state = 'pending' AND COALESCE(a.content_hash, '') = ''
  AND NOT EXISTS (SELECT 1 FROM source_merge_preserved_attachments spa WHERE spa.attachment_id = a.id))`

// ListPendingTikTokMediaContext reads only candidate IDs, not archived bodies.
func (s *Store) ListPendingTikTokMediaContext(ctx context.Context, sourceID, after, through int64, limit int) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.id FROM messages m WHERE `+tikTokPendingMediaWhere+` AND m.id > ? AND m.id <= ? ORDER BY m.id LIMIT ?`, sourceID, after, through, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) HasPendingTikTokMediaContext(ctx context.Context, sourceID int64) (bool, error) {
	var pending bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM messages m WHERE `+tikTokPendingMediaWhere+`)`, sourceID).Scan(&pending)
	return pending, err
}

type TikTokMediaCandidate struct {
	SourceMessageID, Conversation, Metadata, PartKey string
	Raw                                              []byte
}

// ReadTikTokMediaCandidateContext rechecks ownership and occurrence provenance
// before reading one raw payload by PK. Both stored and decoded bytes are capped.
func (s *Store) ReadTikTokMediaCandidateContext(ctx context.Context, sourceID, messageID int64) (TikTokMediaCandidate, error) {
	const maxRawBytes = 1 << 20
	var item TikTokMediaCandidate
	err := s.db.QueryRowContext(ctx, `SELECT m.source_message_id, c.source_conversation_id, COALESCE(CAST(m.metadata AS TEXT), ''), a.source_part_key
 FROM messages m JOIN conversations c ON c.id = m.conversation_id JOIN attachments a ON a.message_id = m.id
 WHERE `+tikTokPendingMediaWhere+` AND m.id = ? AND c.source_id = m.source_id
 AND a.source_part_key IN ('tiktok:image', 'tiktok:video', 'tiktok:sticker', 'tiktok:emoji')
 AND a.attachment_state = 'pending' AND COALESCE(a.content_hash, '') = ''
 AND NOT EXISTS (SELECT 1 FROM source_merge_preserved_attachments spa WHERE spa.attachment_id = a.id)
 ORDER BY a.id LIMIT 1`, sourceID, messageID).Scan(&item.SourceMessageID, &item.Conversation, &item.Metadata, &item.PartKey)
	if err != nil {
		return item, err
	}
	var raw []byte
	var format, compression sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT raw_data, raw_format, compression FROM message_raw WHERE message_id = ? AND LENGTH(raw_data) <= ?`, messageID, maxRawBytes).Scan(&raw, &format, &compression)
	if err != nil {
		return item, err
	}
	if format.String != "tiktok-message-json" || (compression.Valid && compression.String != "" && compression.String != "zlib") {
		return item, errors.New("invalid TikTok media raw format")
	}
	item.Raw, _, err = decodeMessageRawBounded(raw, compression, maxRawBytes, errors.New("TikTok media raw exceeds decode limit"))
	return item, err
}
