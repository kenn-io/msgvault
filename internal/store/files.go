package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// FileMetadata is the transactional authority for one attachment row and its
// containing archive identities. StoragePath is retained only for local blobs;
// URL-backed rows expose URL and deliberately clear content-hash authority.
// SourceID, SourceMessageID, MessageType, and ConversationType carry the
// facts that determine the containing item's canonical explore entry key.
type FileMetadata struct {
	ID               int64
	MessageID        int64
	ConversationID   int64
	SourceID         int64
	SourceMessageID  string
	MessageType      string
	ConversationType string
	Filename         string
	MimeType         string
	Size             int64
	ContentHash      string
	StoragePath      string
	URL              string
}

func (s *Store) GetFileMetadata(ctx context.Context, id int64) (*FileMetadata, error) {
	files, err := s.GetFileMetadataBatch(ctx, []int64{id})
	if err != nil {
		return nil, err
	}
	file, ok := files[id]
	if !ok {
		return nil, nil //nolint:nilnil // not-found is a normal metadata lookup result
	}
	return &file, nil
}

// GetFileMetadataBatch resolves one bounded page in one database query.
func (s *Store) GetFileMetadataBatch(ctx context.Context, ids []int64) (map[int64]FileMetadata, error) {
	result := make(map[int64]FileMetadata, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	unique := make([]int64, 0, len(ids))
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return nil, errors.New("file ID must be positive")
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	placeholders := make([]string, len(unique))
	args := make([]any, len(unique))
	for i, id := range unique {
		placeholders[i] = "?"
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(`
		SELECT a.id, a.message_id, m.conversation_id,
			m.source_id, COALESCE(m.source_message_id, ''),
			COALESCE(m.message_type, ''), COALESCE(c.conversation_type, ''),
			COALESCE(a.filename, ''), COALESCE(a.mime_type, ''), COALESCE(a.size, 0),
			COALESCE(a.content_hash, ''), COALESCE(a.storage_path, '')
		FROM attachments a
		JOIN messages m ON m.id = a.message_id
		JOIN conversations c ON c.id = m.conversation_id
		WHERE a.id IN (`+strings.Join(placeholders, ",")+`)
		ORDER BY a.id`), args...)
	if err != nil {
		return nil, fmt.Errorf("get file metadata batch: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var file FileMetadata
		if err := rows.Scan(&file.ID, &file.MessageID, &file.ConversationID,
			&file.SourceID, &file.SourceMessageID, &file.MessageType, &file.ConversationType,
			&file.Filename, &file.MimeType, &file.Size, &file.ContentHash, &file.StoragePath); err != nil {
			return nil, fmt.Errorf("scan file metadata: %w", err)
		}
		lowerPath := strings.ToLower(file.StoragePath)
		if strings.HasPrefix(lowerPath, "http://") || strings.HasPrefix(lowerPath, "https://") {
			file.URL = file.StoragePath
			file.StoragePath = ""
			file.ContentHash = ""
		}
		result[file.ID] = file
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate file metadata: %w", err)
	}
	return result, nil
}
