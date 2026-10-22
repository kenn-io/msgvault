package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/personscope"
)

// ListMediaSearchOccurrences reads the complete live population for a destination.
func (s *Store) ListMediaSearchOccurrences(ctx context.Context, destination string, scope *personscope.Scope) ([]MessageMediaOccurrence, error) {
	return s.listMessageOccurrenceRows(ctx, destination, 0, scope)
}

// MediaSearchLocalGaps counts audio that has no current mapping to search.
func (s *Store) MediaSearchLocalGaps(ctx context.Context, destination string, scope *personscope.Scope) (unavailable int, err error) {
	where := ""
	args := []any{destination}
	if scope != nil {
		predicate, values := personscope.MessagePredicate(*scope, "m", "cv")
		where = " AND (" + predicate + ")"
		args = append(args, values...)
	}
	err = s.db.QueryRowContext(ctx, s.Rebind(`
		SELECT COUNT(*)
		FROM attachments a
		JOIN messages m ON m.id = a.message_id
		JOIN conversations cv ON cv.id = m.conversation_id
		JOIN sources src ON src.id = m.source_id
		WHERE `+LiveMessagesWhere("m", true)+` AND `+messageRecordingAudio("")+`
		  AND NOT EXISTS (SELECT 1 FROM beeper_media_occurrences o
		    WHERE o.destination_key = ? AND o.retention_state <> 'revoked'
		      AND o.source_type = src.source_type AND o.source_identifier = src.identifier
		      AND o.source_message_id = m.source_message_id
		      AND o.source_conversation_id = COALESCE(cv.source_conversation_id, '')
		      AND o.source_attachment_id = COALESCE(a.source_attachment_id, '')
		      AND o.source_part_key = COALESCE(NULLIF(a.source_part_key, ''), NULLIF(a.source_attachment_id, ''), src.source_type || ':unknown')
		      AND o.source_sha256 = a.content_hash AND `+beeperMediaEligible+`)`+where), args...).Scan(&unavailable)
	if err != nil {
		err = fmt.Errorf("read media search gaps: %w", err)
	}
	return
}

// DocumentSearchOccurrence links one delivered version to a visible attachment.
type DocumentSearchOccurrence struct {
	VersionID       string
	AttachmentID    int64
	MessageID       int64
	ConversationID  int64
	Filename        string
	ContainingTitle string
	OccurredAt      *time.Time
}

// documentSearchRows shares attachment eligibility, visibility, and person scope.
func documentSearchRows(destination string, scope *personscope.Scope) (string, []any) {
	args := []any{destination}
	query := `
 FROM attachments a
 JOIN messages m ON m.id = a.message_id
 JOIN conversations cv ON cv.id = m.conversation_id
 LEFT JOIN docbank_attachment_deliveries d
   ON d.destination_key = ? AND d.content_hash = LOWER(COALESCE(NULLIF(a.content_hash,''),SUBSTR(a.storage_path,4)))
 WHERE ` + docbankAttachmentEligibleWhere() + ` AND ` + LiveMessagesWhere("m", true)
	if scope != nil {
		predicate, values := personscope.MessagePredicate(*scope, "m", "cv")
		query += " AND (" + predicate + ")"
		args = append(args, values...)
	}
	return query, args
}

// DocumentSearchVersion is a delivered version and its newest visible message date.
// Together they form the cursor for newest-first pagination, with undated versions last.
type DocumentSearchVersion struct {
	VersionID  string
	OccurredAt *time.Time
}

// DocumentSearchVersions pages delivered versions after the previous ordering key.
func (s *Store) DocumentSearchVersions(ctx context.Context, destination string, scope *personscope.Scope, after *DocumentSearchVersion) ([]DocumentSearchVersion, error) {
	query, args := documentSearchRows(destination, scope)
	const newest = "MAX(COALESCE(m.sent_at,m.received_at,m.internal_date))"
	query = `SELECT d.version_id, ` + newest + query + ` AND d.state = 'delivered' AND d.version_id <> '' GROUP BY d.version_id`
	if after != nil {
		if after.OccurredAt == nil {
			query += ` HAVING ` + newest + ` IS NULL AND d.version_id > ?`
			args = append(args, after.VersionID)
		} else {
			query += ` HAVING (` + newest + ` < ? OR ` + newest + ` IS NULL OR (` + newest + ` = ? AND d.version_id > ?))`
			args = append(args, *after.OccurredAt, *after.OccurredAt, after.VersionID)
		}
	}
	query += ` ORDER BY ` + newest + ` DESC NULLS LAST, d.version_id LIMIT 4096`
	rows, err := s.db.QueryContext(ctx, s.Rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("list document search versions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var versions []DocumentSearchVersion
	for rows.Next() {
		var version DocumentSearchVersion
		var date nullableTimestamp
		if err := rows.Scan(&version.VersionID, &date); err != nil {
			return nil, fmt.Errorf("scan document search version: %w", err)
		}
		version.OccurredAt = optionalTimestamp(date)
		versions = append(versions, version)
	}
	return versions, rows.Err()
}

// DocumentSearchOccurrences maps hit versions to their current visible attachments.
func (s *Store) DocumentSearchOccurrences(ctx context.Context, destination string, scope *personscope.Scope, versionIDs []string) ([]DocumentSearchOccurrence, error) {
	var result []DocumentSearchOccurrence
	// Keep parameters below both backends' limits.
	for start := 0; start < len(versionIDs); start += docbankReceiptChunk {
		chunk := versionIDs[start:min(start+docbankReceiptChunk, len(versionIDs))]
		query, args := documentSearchRows(destination, scope)
		query = `SELECT d.version_id, a.id, m.id, cv.id, COALESCE(a.filename,''), ` + containingTitleMCVSQL + `, COALESCE(m.sent_at,m.received_at,m.internal_date)` + query + `
 AND d.state = 'delivered' AND d.version_id IN (?` + strings.Repeat(",?", len(chunk)-1) + `) ORDER BY COALESCE(m.sent_at,m.received_at,m.internal_date) DESC NULLS LAST, a.id`
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := s.db.QueryContext(ctx, s.Rebind(query), args...)
		if err != nil {
			return nil, fmt.Errorf("list document search occurrences: %w", err)
		}
		for rows.Next() {
			var o DocumentSearchOccurrence
			var date nullableTimestamp
			if err := rows.Scan(&o.VersionID, &o.AttachmentID, &o.MessageID, &o.ConversationID, &o.Filename, &o.ContainingTitle, &date); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan document search occurrence: %w", err)
			}
			o.OccurredAt = optionalTimestamp(date)
			result = append(result, o)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}
