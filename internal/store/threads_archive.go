package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// ThreadsArchiveMessageMetadataContext looks up one source-qualified media ID
// and its actual archive conversation, without loading any message body.
func (s *Store) ThreadsArchiveMessageMetadataContext(ctx context.Context, sourceID int64, mediaID string) (MessageMetadataRecord, bool, error) {
	var record MessageMetadataRecord
	err := s.db.QueryRowContext(ctx, `
		SELECT m.id, m.metadata, c.source_conversation_id
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id AND c.source_id = m.source_id
		WHERE m.source_id = ? AND m.source_message_id = ?`, sourceID, mediaID).
		Scan(&record.ID, &record.Metadata, &record.SourceConversationID)
	if errors.Is(err, sql.ErrNoRows) {
		return MessageMetadataRecord{}, false, nil
	}
	if err != nil {
		return MessageMetadataRecord{}, false, fmt.Errorf("get Threads archive message metadata: %w", err)
	}
	return record, true, nil
}

// ThreadsReplyRepairCandidate contains only metadata for an unlinked reply
// whose parent is already archived in the same source.
type ThreadsReplyRepairCandidate struct {
	MessageID            int64
	SourceMessageID      string
	SourceConversationID string
	Metadata             string
}

func threadsParentRepairKey(sourceID int64) string {
	return "threads_parent_repair:" + strconv.FormatInt(sourceID, 10)
}

// ListThreadsReplyRepairCandidatesContext examines at most limit source rows,
// then filters available native parents. next resumes the scan or wraps to zero.
// Callers checkpoint next only after validating and repairing this batch.
func (s *Store) ListThreadsReplyRepairCandidatesContext(ctx context.Context, sourceID int64, limit int) ([]ThreadsReplyRepairCandidate, int64, error) {
	if limit <= 0 {
		return nil, 0, errors.New("threads parent repair limit must be positive")
	}
	var after int64
	err := s.db.QueryRowContext(ctx, `SELECT value FROM archive_metadata WHERE key=?`, threadsParentRepairKey(sourceID)).Scan(&after)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, 0, fmt.Errorf("read Threads repair cursor: %w", err)
	}
	parent := `CASE WHEN json_valid(m.metadata) THEN json_extract(m.metadata,'$.parent_id') END`
	if s.IsPostgreSQL() {
		parent = `m.metadata->>'parent_id'`
	}
	// LIMIT precedes all availability checks: even permanently missing or
	// archive-only rows consume the scan budget and advance its durable position.
	rows, err := s.db.QueryContext(ctx, `WITH batch AS (
 SELECT id,source_message_id,conversation_id,reply_to_message_id,message_type,metadata
 FROM messages WHERE source_id=? AND id>? ORDER BY id LIMIT ?
 ) SELECT m.id,m.source_message_id,COALESCE(c.source_conversation_id,''),COALESCE(CAST(m.metadata AS TEXT),''),
 CASE WHEN m.message_type='threads' AND m.reply_to_message_id IS NULL AND c.id IS NOT NULL
 AND NOT EXISTS(SELECT 1 FROM source_merge_archive_only_messages archived WHERE archived.message_id=m.id)
 AND EXISTS(SELECT 1 FROM messages parent JOIN conversations pc ON pc.id=parent.conversation_id AND pc.source_id=parent.source_id
 WHERE parent.source_id=? AND parent.message_type='threads' AND parent.source_message_id=`+parent+`
 AND NOT EXISTS(SELECT 1 FROM source_merge_archive_only_messages archived WHERE archived.message_id=parent.id))
 THEN TRUE ELSE FALSE END
 FROM batch m LEFT JOIN conversations c ON c.id=m.conversation_id AND c.source_id=? ORDER BY m.id`, sourceID, after, limit, sourceID, sourceID)
	if err != nil {
		return nil, 0, fmt.Errorf("list Threads parent repair candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ThreadsReplyRepairCandidate
	var next int64
	examined := 0
	for rows.Next() {
		var row ThreadsReplyRepairCandidate
		var eligible bool
		if err := rows.Scan(&row.MessageID, &row.SourceMessageID, &row.SourceConversationID, &row.Metadata, &eligible); err != nil {
			return nil, 0, fmt.Errorf("scan Threads parent repair candidate: %w", err)
		}
		next = row.MessageID
		examined++
		if eligible {
			out = append(out, row)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate Threads parent repair candidates: %w", err)
	}
	if examined < limit {
		next = 0
	}
	return out, next, nil
}

// CheckpointThreadsParentRepairContext retains completed scan work independently
// of pinned provider coverage. Source execution ownership serializes callers.
func (s *Store) CheckpointThreadsParentRepairContext(ctx context.Context, sourceID, next int64) error {
	if next < 0 {
		return errors.New("threads parent repair cursor must be nonnegative")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO archive_metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, threadsParentRepairKey(sourceID), strconv.FormatInt(next, 10))
	if err != nil {
		return fmt.Errorf("checkpoint Threads parent repair: %w", err)
	}
	return nil
}
