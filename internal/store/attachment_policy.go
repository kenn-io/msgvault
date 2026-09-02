package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
)

// AttachmentPolicyCandidate is one stored provider occurrence that current
// policy may exclude.
type AttachmentPolicyCandidate struct {
	AttachmentID     int64
	MessageID        int64
	SourceType       string
	SourceIdentifier string
	ConversationType string
	ParticipantCount int
	// RosterUnresolved reports that no authoritative roster backs
	// ParticipantCount: the provider could not read one, or none was ever
	// archived, so the count is only the accumulated participant rows. Purge
	// must not exclude on the participant threshold while it is set: excluding
	// deletes blobs, and the current membership may be under the limit even
	// when the accumulated count is not.
	RosterUnresolved   bool
	Size               int64
	ContentHash        string
	StoragePath        string
	ThumbnailHash      string
	ThumbnailPath      string
	SourceAttachmentID string
}

// AttachmentExclusion selects one occurrence and records its policy reason.
type AttachmentExclusion struct {
	AttachmentID       int64
	Reason             attachmentpolicy.SkipReason
	SourceAttachmentID string
}

// ListAttachmentPolicyCandidates returns stored provider media with the
// source and conversation context needed to apply current configuration.
func (s *Store) ListAttachmentPolicyCandidates(ctx context.Context) ([]AttachmentPolicyCandidate, error) {
	rows, err := s.db.QueryContext(ctx, s.Rebind(`
		SELECT a.id, a.message_id, src.source_type, src.identifier,
		       c.conversation_type, COALESCE(c.participant_count, 0),
		       COALESCE(CAST(c.metadata AS TEXT), ''), COALESCE(a.size, 0),
		       COALESCE(a.content_hash, ''), a.storage_path,
		       COALESCE(a.thumbnail_hash, ''), COALESCE(a.thumbnail_path, ''),
		       COALESCE(a.source_attachment_id, ''), COALESCE(a.attachment_state, '')
		FROM attachments a
		JOIN messages m ON m.id = a.message_id
		JOIN conversations c ON c.id = m.conversation_id
		JOIN sources src ON src.id = m.source_id
		WHERE src.source_type IN ('beeper', 'slack', 'slackdump', 'discord', 'teams')
		  AND COALESCE(a.attachment_state, '') IN (?, '')
		  AND (
		    COALESCE(a.source_attachment_id, '') <> ''
		    OR (
		      src.source_type = 'teams'
		      AND COALESCE(a.filename, '') = '' AND COALESCE(a.mime_type, '') = ''
		      AND COALESCE(a.content_hash, '') <> ''
		      AND a.storage_path NOT LIKE 'http://%' AND a.storage_path NOT LIKE 'https://%'
		    )
		  )
		ORDER BY a.id
	`), attachmentpolicy.StateStored)
	if err != nil {
		return nil, fmt.Errorf("list attachment policy candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var candidates []AttachmentPolicyCandidate
	for rows.Next() {
		var candidate AttachmentPolicyCandidate
		var state attachmentpolicy.DownloadState
		var conversationMetadata string
		if err := rows.Scan(
			&candidate.AttachmentID, &candidate.MessageID, &candidate.SourceType,
			&candidate.SourceIdentifier, &candidate.ConversationType,
			&candidate.ParticipantCount, &conversationMetadata, &candidate.Size, &candidate.ContentHash,
			&candidate.StoragePath, &candidate.ThumbnailHash, &candidate.ThumbnailPath,
			&candidate.SourceAttachmentID, &state,
		); err != nil {
			return nil, fmt.Errorf("scan attachment policy candidate: %w", err)
		}
		candidate.ParticipantCount = attachmentPolicyParticipantCount(
			candidate.SourceType, candidate.ParticipantCount, conversationMetadata,
		)
		record := decodeMembershipRecord(conversationMetadata)
		candidate.RosterUnresolved = !record.counted || record.unknown
		if state == "" && candidate.ContentHash == "" {
			contentHash, ok := casPathHash(candidate.StoragePath)
			if !ok {
				continue
			}
			candidate.ContentHash = contentHash
		}
		if candidate.SourceAttachmentID == "" {
			candidate.SourceAttachmentID = fmt.Sprintf("teams:inline:legacy:%d", candidate.AttachmentID)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list attachment policy candidates: %w", err)
	}
	return candidates, nil
}

// ExcludeAttachmentOccurrences atomically detaches selected occurrences from
// their blobs and leaves typed metadata markers in their place.
func (s *Store) ExcludeAttachmentOccurrences(ctx context.Context, exclusions []AttachmentExclusion) error {
	if len(exclusions) == 0 {
		return nil
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		messageIDs := make(map[int64]struct{})
		for _, exclusion := range exclusions {
			if exclusion.AttachmentID <= 0 || exclusion.Reason == "" || exclusion.Reason == attachmentpolicy.SkipFetchFailure {
				return fmt.Errorf("invalid attachment exclusion: id=%d reason=%q", exclusion.AttachmentID, exclusion.Reason)
			}
			var messageID int64
			var sourceAttachmentID string
			err := tx.QueryRow(`
				SELECT message_id, COALESCE(source_attachment_id, '')
				FROM attachments WHERE id = ?
			`, exclusion.AttachmentID).Scan(&messageID, &sourceAttachmentID)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("attachment occurrence %d not found", exclusion.AttachmentID)
			}
			if err != nil {
				return fmt.Errorf("select attachment occurrence %d: %w", exclusion.AttachmentID, err)
			}
			if sourceAttachmentID == "" {
				sourceAttachmentID = exclusion.SourceAttachmentID
			}
			if sourceAttachmentID == "" {
				return fmt.Errorf("attachment occurrence %d has no provider identity", exclusion.AttachmentID)
			}
			result, err := tx.Exec(`
				UPDATE attachments
				SET storage_path = ?, content_hash = NULL,
				    thumbnail_hash = NULL, thumbnail_path = NULL,
				    source_attachment_id = ?, attachment_state = ?, attachment_skip_reason = ?
				WHERE id = ?
			`, "excluded:"+sourceAttachmentID, sourceAttachmentID, attachmentpolicy.StateSkipped,
				exclusion.Reason, exclusion.AttachmentID)
			if err != nil {
				return fmt.Errorf("exclude attachment occurrence %d: %w", exclusion.AttachmentID, err)
			}
			changed, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("exclude attachment occurrence %d rows affected: %w", exclusion.AttachmentID, err)
			}
			if changed != 1 {
				return fmt.Errorf("exclude attachment occurrence %d: changed %d rows", exclusion.AttachmentID, changed)
			}
			messageIDs[messageID] = struct{}{}
		}
		for messageID := range messageIDs {
			if _, err := tx.Exec(`
				UPDATE messages
				SET has_attachments = (SELECT COUNT(*) FROM attachments WHERE message_id = ?) > 0,
				    attachment_count = (SELECT COUNT(*) FROM attachments WHERE message_id = ?)
				WHERE id = ?
			`, messageID, messageID, messageID); err != nil {
				return fmt.Errorf("recompute attachment stats for message %d: %w", messageID, err)
			}
		}
		return nil
	})
}

// AttachmentBlobReferenced reports whether any occurrence still owns the
// given content hash or canonical storage path as full-size or thumbnail media.
func (s *Store) AttachmentBlobReferenced(ctx context.Context, contentHash, storagePath string) (bool, error) {
	if contentHash == "" && storagePath == "" {
		return false, nil
	}
	var exists bool
	err := s.db.QueryRowContext(ctx, s.Rebind(`
		SELECT EXISTS (
			SELECT 1 FROM attachments
			WHERE (? <> '' AND (content_hash = ? OR thumbnail_hash = ?))
			   OR (? <> '' AND (storage_path = ? OR thumbnail_path = ?))
		)
	`), contentHash, contentHash, contentHash,
		storagePath, storagePath, storagePath).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check attachment blob reference: %w", err)
	}
	return exists, nil
}
