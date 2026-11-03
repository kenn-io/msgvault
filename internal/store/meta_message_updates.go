package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"go.kenn.io/msgvault/internal/textutil"
)

// MetaMessageSnippetRunes is the maximum number of runes in a Meta inbox snippet.
const MetaMessageSnippetRunes = 500

// MetaMessageReaction replaces the single reaction represented by a provider
// event ID. An empty Value removes it while retaining ordering in Metadata.
type MetaMessageReaction struct {
	ParticipantID    int64
	SourceReactionID string
	Value            string
	CreatedAt        time.Time
}

// ResolveMetaMessageRepliesContext links the changed inbox message and children
// that reference it. A missing parent stays deferred; other conversations and
// unsupported metadata versions are left for their owning reader.
func (s *Store) ResolveMetaMessageRepliesContext(ctx context.Context, sourceID, messageID int64, metadataVersion int) error {
	if metadataVersion <= 0 {
		return errors.New("inbox reply metadata version must be positive")
	}
	return s.withSyncSourceWriteContext(ctx, sourceID, func(q querier) error {
		var mid, kind string
		var conversationID int64
		if err := q.QueryRow(`SELECT source_message_id, message_type, conversation_id FROM messages WHERE id = ? AND source_id = ?`, messageID, sourceID).Scan(&mid, &kind, &conversationID); err != nil {
			return fmt.Errorf("read changed inbox message: %w", err)
		}
		if kind != "facebook_messages" && kind != "instagram_messages" {
			return errors.New("message is not an inbox message")
		}
		reference := "CASE WHEN json_valid(child.metadata) THEN json_extract(child.metadata, '$.referenced_message_id') END"
		version := "CAST(CASE WHEN json_valid(child.metadata) THEN json_extract(child.metadata, '$.version') END AS TEXT)"
		if s.IsPostgreSQL() {
			reference = "child.metadata ->> 'referenced_message_id'"
			version = "child.metadata ->> 'version'"
		}
		parent := `SELECT parent.id FROM messages parent
			WHERE parent.source_id = child.source_id
			  AND parent.conversation_id = child.conversation_id
			  AND parent.message_type = child.message_type
			  AND parent.id <> child.id
			  AND parent.source_message_id = ` + reference
		_, err := q.Exec(`UPDATE messages AS child SET reply_to_message_id = (`+parent+`)
			WHERE child.source_id = ? AND child.conversation_id = ? AND child.message_type = ?
			  AND child.reply_to_message_id IS NULL AND (`+version+`) = ?
			  AND (child.id = ? OR (`+reference+`) = ?) AND EXISTS (`+parent+`)`,
			sourceID, conversationID, kind, strconv.Itoa(metadataVersion), messageID, mid)
		if err != nil {
			return fmt.Errorf("resolve changed inbox replies: %w", err)
		}
		return nil
	})
}

// MetaMessageUpdate commits a provider edit or reaction with the metadata that
// makes replay safe. PreviousMetadata prevents overwriting a newer projection.
type MetaMessageUpdate struct {
	PreviousMetadata sql.NullString
	Metadata         sql.NullString
	Text             *string
	Reaction         *MetaMessageReaction
	Watermark        *ArchiveWatermark
}

// ApplyMetaMessageUpdateContext atomically updates an archived inbox message.
// Edits retain the stored sender/recipient search envelope and original raw data.
func (s *Store) ApplyMetaMessageUpdateContext(ctx context.Context, messageID int64, update MetaMessageUpdate) error {
	if (update.Text == nil) == (update.Reaction == nil) {
		return errors.New("inbox update requires one edit or reaction")
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockMeetingEvidenceWith(ctx, tx, messageID); err != nil {
			return err
		}
		q := boundQuerier{ctx: ctx, q: tx}
		if err := s.requireSyncMessageSourceTx(q, messageID); err != nil {
			return err
		}
		var sourceID int64
		var kind string
		var prior sql.NullString
		if err := q.QueryRow(`SELECT source_id, message_type, metadata FROM messages WHERE id = ?`, messageID).Scan(&sourceID, &kind, &prior); err != nil {
			return err
		}
		if kind != "facebook_messages" && kind != "instagram_messages" {
			return errors.New("message is not an inbox message")
		}
		if prior != update.PreviousMetadata {
			return errors.New("inbox metadata changed before update")
		}
		var watermarkCurrent int64
		if update.Watermark != nil {
			watermark := update.Watermark
			if watermark.Key == "" || watermark.ObservedAt <= 0 || watermark.Floor < 0 {
				return errors.New("invalid archive watermark")
			}
			current, err := s.lockArchiveWatermarkWith(q, watermark.Key, watermark.Floor)
			if err != nil {
				return err
			}
			watermarkCurrent = current
			if watermark.RequireNewer && watermark.ObservedAt <= watermarkCurrent {
				return ErrStaleArchiveWatermark
			}
		}
		if update.Text != nil {
			fts, err := s.metaMessageEnvelope(q, messageID)
			if err != nil {
				return fmt.Errorf("load inbox search envelope: %w", err)
			}
			fts.Body = *update.Text
			text := sql.NullString{Valid: true, String: *update.Text}
			snippet := sql.NullString{Valid: true, String: textutil.PrefixRunes(*update.Text, MetaMessageSnippetRunes)}
			if err = s.updateMessageDerivedText(q, messageID, text, sql.NullString{}, snippet, fts, func(q querier, doc FTSDoc) error {
				_, err := s.upsertFTSOrUnsearchable(q, doc)
				return err
			}); err != nil {
				return err
			}
			if _, err = q.Exec(`UPDATE messages SET is_edited = ?, size_estimate = ? WHERE id = ?`, true, len(*update.Text), messageID); err != nil {
				return err
			}
			if err := s.bumpDerivedDataRevisionContext(ctx, tx); err != nil {
				return fmt.Errorf("invalidate edited inbox cache: %w", err)
			}
		} else {
			r := update.Reaction
			var oldID int64
			err := q.QueryRow(`SELECT rse.reaction_id FROM reaction_source_events rse JOIN reactions r ON r.id = rse.reaction_id WHERE rse.source_id = ? AND rse.source_reaction_id = ? AND r.message_id = ?`, sourceID, r.SourceReactionID, messageID).Scan(&oldID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err == nil {
				if _, err = q.Exec(`DELETE FROM reaction_source_events WHERE source_id = ? AND source_reaction_id = ?`, sourceID, r.SourceReactionID); err != nil {
					return err
				}
				if _, err = q.Exec(`DELETE FROM reactions WHERE id = ? AND NOT EXISTS (SELECT 1 FROM reaction_source_events WHERE reaction_id = ?)`, oldID, oldID); err != nil {
					return err
				}
			}
			if r.Value != "" {
				if _, err = q.Exec(s.dialect.InsertOrIgnore(`INSERT OR IGNORE INTO reactions (message_id, participant_id, reaction_type, reaction_value, created_at) VALUES (?, ?, 'emoji', ?, ?)`), messageID, r.ParticipantID, r.Value, r.CreatedAt); err != nil {
					return err
				}
				if _, err = q.Exec(s.dialect.InsertOrIgnore(`INSERT OR IGNORE INTO reaction_source_events (source_id, source_reaction_id, reaction_id) SELECT ?, ?, id FROM reactions WHERE message_id = ? AND participant_id = ? AND reaction_type = 'emoji' AND reaction_value = ?`), sourceID, r.SourceReactionID, messageID, r.ParticipantID, r.Value); err != nil {
					return err
				}
			}
		}
		if err := setMessageMetadataWith(q, s.dialect, messageID, update.Metadata); err != nil {
			return err
		}
		if update.Watermark != nil {
			return advanceArchiveWatermarkWith(q, update.Watermark.Key, watermarkCurrent, update.Watermark.ObservedAt)
		}
		return nil
	})
}

// metaMessageEnvelope preserves the archived display names used by inbox search.
func (s *Store) metaMessageEnvelope(q querier, messageID int64) (FTSDoc, error) {
	aggregate := "GROUP_CONCAT"
	if s.IsPostgreSQL() {
		aggregate = "STRING_AGG"
	}
	recipients := func(kind string) string {
		return fmt.Sprintf("COALESCE((SELECT %s(COALESCE(NULLIF(mr.display_name, ''), p.phone_number, p.email_address, ''), ' ') FROM message_recipients mr JOIN participants p ON p.id = mr.participant_id WHERE mr.message_id = m.id AND mr.recipient_type = '%s'), '')", aggregate, kind)
	}
	doc := FTSDoc{MessageID: messageID}
	err := q.QueryRow(`SELECT COALESCE(m.subject, ''), `+recipients("from")+`, `+recipients("to")+`, `+recipients("cc")+` FROM messages m WHERE m.id = ?`, messageID).Scan(&doc.Subject, &doc.FromAddr, &doc.ToAddrs, &doc.CcAddrs)
	return doc, err
}
