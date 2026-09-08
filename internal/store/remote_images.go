package store

import (
	"context"
	"errors"
)

// UpsertRemoteImageAttachment stores a distinct remote URL occurrence. Matching
// bytes never establish that a legacy MIME attachment is the same occurrence.
func (s *Store) UpsertRemoteImageAttachment(ctx context.Context, messageID int64, write AttachmentWrite) error {
	write = write.normalized()
	if err := write.validate(); err != nil {
		return err
	}
	if write.SourcePartKey == "" {
		return errors.New("remote image attachment requires a source-part key")
	}
	return s.withSyncMessageWriteContext(ctx, messageID, func(q querier) error {
		return s.upsertAttachmentRecordWithPolicy(q, messageID, write, preserveLegacyAttachmentRows)
	})
}

// MessageRemoteImages returns only this message's archived remote images.
// The namespace survives MIME repair, which replaces MIME-owned rows only.
func (s *Store) MessageRemoteImages(messageID int64) (map[string]AttachmentRef, error) {
	return s.messageProviderAttachments(messageID, "remote-image:")
}

// RemoteImageBackfillMessageIDs pages email identities without scanning body
// storage. Bodies are subsequently read by message primary key.
func (s *Store) RemoteImageBackfillMessageIDs(ctx context.Context, after, sourceID int64, limit int) ([]int64, error) {
	// Match IsEmailMessageType: legacy NULL and empty types also denote email.
	query := "SELECT id FROM messages WHERE id > ? AND COALESCE(message_type, '') IN ('', 'email') AND deleted_at IS NULL"
	args := []any{after}
	if sourceID != 0 {
		query += " AND source_id = ?"
		args = append(args, sourceID)
	}
	query += " ORDER BY id LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
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
