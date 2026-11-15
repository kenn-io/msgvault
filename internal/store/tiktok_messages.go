package store

import (
	"context"
	"database/sql"
	"errors"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
)

// TikTokMessageState is the recoverable work belonging to one message snapshot.
// A nil reply preserves a sparse projection; an explicit missing target clears
// the old link so deferred resolution can find the new parent later.
type TikTokMessageState struct {
	PendingMedia *AttachmentWrite
	ReplyTo      *string
}

// PersistTikTokMessageContext commits the snapshot, pending media occurrence,
// and explicit reply target together. Retrieval and CAS writes happen later.
func (s *Store) PersistTikTokMessageContext(ctx context.Context, data *MessagePersistData, state TikTokMessageState) (int64, error) {
	if data == nil || data.Message == nil || data.Message.MessageType != "tiktok_messages" {
		return 0, errors.New("persist TikTok message requires a TikTok message")
	}
	partKey := ""
	if state.PendingMedia != nil {
		pending := state.PendingMedia.normalized()
		if err := validateTikTokPendingMedia(pending); err != nil {
			return 0, err
		}
		state.PendingMedia = &pending
		partKey = pending.SourcePartKey
	}
	after := func(ctx context.Context, tx *loggedTx, data *MessagePersistData, id int64) error {
		q := boundQuerier{ctx: ctx, q: tx}
		if state.ReplyTo != nil {
			var parent, current sql.NullInt64
			if err := q.QueryRow(`SELECT reply_to_message_id FROM messages WHERE id = ?`, id).Scan(&current); err != nil {
				return err
			}
			err := q.QueryRow(`SELECT p.id FROM messages p JOIN messages child ON child.id = ?
    WHERE p.source_id = child.source_id AND p.conversation_id = child.conversation_id AND p.source_message_id = ?`, id, *state.ReplyTo).Scan(&parent)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if current != parent {
				if _, err := q.Exec(`UPDATE messages SET reply_to_message_id = ? WHERE id = ?`, parent, id); err != nil {
					return err
				}
			}
		}
		// Only retire obsolete, unstored occurrences owned by this importer. A
		// stored or merge-preserved attachment remains archive evidence.
		result, err := q.Exec(`DELETE FROM attachments WHERE message_id = ?
   AND source_part_key IN ('tiktok:image', 'tiktok:video', 'tiktok:sticker', 'tiktok:emoji') AND source_part_key <> ?
   AND attachment_state = 'pending' AND COALESCE(content_hash, '') = '' AND COALESCE(storage_path, '') = ''
   AND source_attachment_id IS NULL
   AND NOT EXISTS (SELECT 1 FROM source_merge_preserved_attachments spa WHERE spa.attachment_id = attachments.id)
   AND NOT EXISTS (SELECT 1 FROM source_merge_archive_only_messages am WHERE am.message_id = attachments.message_id)`, id, partKey)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if state.PendingMedia != nil {
			inserted, err := s.queueTikTokMedia(q, id, *state.PendingMedia)
			if err != nil {
				return err
			}
			if inserted {
				changed++
			}
		}
		if changed == 0 {
			return nil
		}
		if err := recomputeMessageAttachmentStatsWith(q, id); err != nil {
			return err
		}
		return s.bumpDerivedDataRevisionContext(ctx, tx)
	}
	return s.persistMessageWithParticipantsTransaction(ctx, attributionLock{}, nil, nil, func([]int64) *MessagePersistData { return data }, nil, after)
}

// queueTikTokMedia leaves all existing occurrences untouched, including stored
// evidence of a different kind. The caller owns the transaction and stats.
func (s *Store) queueTikTokMedia(q querier, messageID int64, pending AttachmentWrite) (bool, error) {
	var skip bool
	if err := q.QueryRow(`SELECT EXISTS (SELECT 1 FROM attachments WHERE message_id = ? AND source_part_key = ?)
 OR EXISTS (SELECT 1 FROM source_merge_archive_only_messages WHERE message_id = ?)`, messageID, pending.SourcePartKey, messageID).Scan(&skip); err != nil {
		return false, err
	}
	if skip {
		return false, nil
	}
	return true, s.upsertAttachmentRecord(q, messageID, pending)
}

// Only importer-owned pending occurrences may be downloaded or completed.
const tikTokWritableMedia = `attachment_state = 'pending'
 AND COALESCE(content_hash, '') = '' AND COALESCE(storage_path, '') = ''
 AND source_attachment_id IS NULL
 AND NOT EXISTS (SELECT 1 FROM source_merge_preserved_attachments spa WHERE spa.attachment_id = attachments.id)
 AND NOT EXISTS (SELECT 1 FROM source_merge_archive_only_messages am WHERE am.message_id = attachments.message_id)`

// PrepareTikTokMediaContext also queues missing work on unchanged snapshots left
// by older importers. Zero means the current part is stored or not owned here.
// The importer holds its source execution lease through preparation and completion.
func (s *Store) PrepareTikTokMediaContext(ctx context.Context, messageID int64, pending AttachmentWrite) (int64, error) {
	pending = pending.normalized()
	if err := validateTikTokPendingMedia(pending); err != nil {
		return 0, err
	}
	var attachmentID int64
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		if err := s.requireSyncMessageSourceTx(q, messageID); err != nil {
			return err
		}
		inserted, err := s.queueTikTokMedia(q, messageID, pending)
		if err != nil {
			return err
		}
		if inserted {
			if err := recomputeMessageAttachmentStatsWith(q, messageID); err != nil {
				return err
			}
			if err := s.bumpDerivedDataRevisionContext(ctx, tx); err != nil {
				return err
			}
		}
		err = q.QueryRow(`SELECT id FROM attachments WHERE message_id = ? AND source_part_key = ? AND `+tikTokWritableMedia, messageID, pending.SourcePartKey).Scan(&attachmentID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	return attachmentID, err
}

// CompleteTikTokMediaContext never recreates or overwrites an occurrence that
// was replaced, stored, or preserved while its bytes were being retrieved.
// Its caller retains the source execution lease acquired before preparation.
func (s *Store) CompleteTikTokMediaContext(ctx context.Context, messageID, attachmentID int64, write AttachmentWrite) error {
	write = write.normalized()
	if err := write.validate(); err != nil {
		return err
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		if err := s.requireSyncMessageSourceTx(q, messageID); err != nil {
			return err
		}
		result, err := q.Exec(`UPDATE attachments SET storage_path = ?, content_hash = ?, size = ?, mime_type = ?, attachment_state = ?
 WHERE id = ? AND message_id = ? AND source_part_key = ? AND `+tikTokWritableMedia,
			write.StoragePath, write.ContentHash, write.Size, write.MIMEType, string(write.State), attachmentID, messageID, write.SourcePartKey)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed == 0 {
			return errors.New("TikTok pending media changed before storage")
		}
		if err := recomputeMessageAttachmentStatsWith(q, messageID); err != nil {
			return err
		}
		return s.bumpDerivedDataRevisionContext(ctx, tx)
	})
}

func validateTikTokPendingMedia(pending AttachmentWrite) error {
	if err := pending.validate(); err != nil {
		return err
	}
	switch pending.SourcePartKey {
	case "tiktok:image", "tiktok:video", "tiktok:sticker", "tiktok:emoji":
	default:
		return errors.New("invalid TikTok pending media part")
	}
	if pending.State != attachmentpolicy.StatePending || pending.ContentHash != "" || pending.StoragePath != "" || pending.SourceAttachmentID != "" {
		return errors.New("invalid TikTok pending media state")
	}
	return nil
}
