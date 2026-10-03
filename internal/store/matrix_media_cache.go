package store

import (
	"database/sql"
	"errors"
)

// CacheMatrixAttachment keeps a stored Matrix media blob mapped to its message
// after an edit replaces the media, so restoring that content later does not
// download it again.
func (s *Store) CacheMatrixAttachment(messageID int64, ref AttachmentRef) error {
	return s.withTx(func(tx *loggedTx) error {
		if err := s.requireSyncMessageSourceTx(tx, messageID); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO matrix_media_cache
			(message_id, source_attachment_id, filename, mime_type, storage_path, content_hash, size, media_type)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(message_id, source_attachment_id) DO UPDATE SET
				filename = excluded.filename, mime_type = excluded.mime_type,
				storage_path = excluded.storage_path, content_hash = excluded.content_hash,
				size = excluded.size, media_type = excluded.media_type`,
			messageID, ref.SourceAttachmentID, ref.Filename, ref.MimeType, ref.StoragePath,
			ref.ContentHash, ref.Size, ref.MediaType)
		return err
	})
}

// CachedMatrixAttachment returns a stored blob mapping kept for a message.
func (s *Store) CachedMatrixAttachment(messageID int64, sourceAttachmentID string) (AttachmentRef, bool, error) {
	var ref AttachmentRef
	err := s.db.QueryRow(`SELECT source_attachment_id, COALESCE(filename, ''), COALESCE(mime_type, ''),
		storage_path, content_hash, size, COALESCE(media_type, '')
		FROM matrix_media_cache WHERE message_id = ? AND source_attachment_id = ?`,
		messageID, sourceAttachmentID).Scan(&ref.SourceAttachmentID, &ref.Filename, &ref.MimeType,
		&ref.StoragePath, &ref.ContentHash, &ref.Size, &ref.MediaType)
	if errors.Is(err, sql.ErrNoRows) {
		return AttachmentRef{}, false, nil
	}
	return ref, err == nil, err
}
