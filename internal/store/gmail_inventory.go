package store

import (
	"context"
	"errors"
	"fmt"
)

// GmailInventoryIdentity describes presence only. Hidden and remotely deleted
// rows still represent archived identities; merge-only history cannot be used
// as an identity in the destination Gmail mailbox.
type GmailInventoryIdentity struct {
	ID          string
	ArchiveOnly bool
	Hidden      bool
}

// ListGmailInventoryIdentitiesContext reads at most limit rows, including
// intentional local deletions and merge-only history. The caller must use a
// read snapshot and request one extra row to detect a resource limit.
func (s *Store) ListGmailInventoryIdentitiesContext(ctx context.Context, sourceID int64, limit int) ([]GmailInventoryIdentity, error) {
	if limit <= 0 {
		return nil, errors.New("gmail inventory row limit must be positive")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT m.source_message_id,
		EXISTS (SELECT 1 FROM source_merge_archive_only_messages a WHERE a.message_id = m.id),
		m.deleted_at IS NOT NULL
		FROM messages m WHERE m.source_id = ? ORDER BY m.id LIMIT ?`, sourceID, limit)
	if err != nil {
		return nil, fmt.Errorf("list Gmail inventory identities: %w", err)
	}
	defer func() { _ = rows.Close() }()
	identities := make([]GmailInventoryIdentity, 0)
	for rows.Next() {
		var identity GmailInventoryIdentity
		if err := rows.Scan(&identity.ID, &identity.ArchiveOnly, &identity.Hidden); err != nil {
			return nil, fmt.Errorf("scan Gmail inventory identity: %w", err)
		}
		identities = append(identities, identity)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read Gmail inventory identities: %w", err)
	}
	return identities, nil
}
