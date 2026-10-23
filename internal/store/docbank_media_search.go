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
   ON d.destination_key = ? AND d.content_hash = ` + docbankAttachmentHashExpr + `
 WHERE ` + docbankAttachmentEligibleWhere() + ` AND ` + LiveMessagesWhere("m", true)
	if scope != nil {
		predicate, values := personscope.MessagePredicate(*scope, "m", "cv")
		query += " AND (" + predicate + ")"
		args = append(args, values...)
	}
	return query, args
}

// DocumentSearchVersions returns up to 4096 delivered versions.
// A larger scope returns no versions and reports that the scope limit was exceeded.
func (s *Store) DocumentSearchVersions(ctx context.Context, destination string, scope *personscope.Scope) ([]string, bool, error) {
	query, args := documentSearchRows(destination, scope)
	query = `SELECT d.version_id` + query + ` AND d.state = 'delivered' AND d.version_id <> '' GROUP BY d.version_id LIMIT 4097`
	rows, err := s.db.QueryContext(ctx, s.Rebind(query), args...)
	if err != nil {
		return nil, false, fmt.Errorf("list document search versions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var versions []string
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			return nil, false, fmt.Errorf("scan document search version: %w", err)
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(versions) > 4096 {
		return nil, true, nil
	}
	return versions, false, nil
}

// DocumentSearchOccurrences maps hit versions to their current visible attachments.
// Each version returns at most limit+1 rows so callers can detect truncation.
func (s *Store) DocumentSearchOccurrences(ctx context.Context, destination string, scope *personscope.Scope, versionIDs []string, limit int) ([]DocumentSearchOccurrence, error) {
	var result []DocumentSearchOccurrence
	if len(versionIDs) == 0 {
		return result, nil
	}
	query, args := documentSearchRows(destination, scope)
	query = `WITH duplicates AS (
 SELECT d.version_id, a.id AS attachment_id, m.id AS message_id, cv.id AS conversation_id,
 COALESCE(a.filename,'') AS filename, ` + containingTitleMCVSQL + ` AS containing_title,
 COALESCE(m.sent_at,m.received_at,m.internal_date) AS occurred_at,
 ROW_NUMBER() OVER (PARTITION BY d.version_id, m.id ORDER BY a.id) AS duplicate_rank` + query + `
 AND d.state = 'delivered' AND d.version_id IN (?` + strings.Repeat(",?", len(versionIDs)-1) + `)
 ), ranked AS (SELECT *,
 ROW_NUMBER() OVER (PARTITION BY version_id ORDER BY occurred_at DESC NULLS LAST, attachment_id) AS occurrence_rank FROM duplicates WHERE duplicate_rank = 1
 ) SELECT version_id, attachment_id, message_id, conversation_id, filename, containing_title, occurred_at
 FROM ranked WHERE occurrence_rank <= ? ORDER BY occurred_at DESC NULLS LAST, attachment_id`
	for _, id := range versionIDs {
		args = append(args, id)
	}
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, s.Rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("list document search occurrences: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var o DocumentSearchOccurrence
		var date nullableTimestamp
		if err := rows.Scan(&o.VersionID, &o.AttachmentID, &o.MessageID, &o.ConversationID, &o.Filename, &o.ContainingTitle, &date); err != nil {
			return nil, fmt.Errorf("scan document search occurrence: %w", err)
		}
		o.OccurredAt = optionalTimestamp(date)
		result = append(result, o)
	}
	return result, rows.Err()
}
