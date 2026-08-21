package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
)

const VisualOriginalMediaInputKey = "original"

// VisualCandidateFilter selects one bounded page of messages. Pagination is
// message-based so duplicate attachment occurrences for one owner can never
// be split across pages.
type VisualCandidateFilter struct {
	AfterMessageID int64
	LimitMessages  int
	MessageIDs     []int64
	MessageTypes   []string
	// SourceIDs restricts candidates to these accounts; empty means every
	// account.
	SourceIDs []int64
}

type VisualCandidate struct {
	Owner                      VisualOwner
	RepresentativeAttachmentID int64
	OccurrenceCount            int64
	Filename                   string
	DeclaredMIME               string
	Size                       int64
	Width                      int64
	Height                     int64
	DurationMS                 int64
	Role                       AttachmentRole
	RoleSource                 AttachmentRoleSource
	MessageType                string
}

type VisualCandidateCounts struct {
	StandaloneOccurrences  int64
	UnknownRoleOccurrences int64
	IneligibleOccurrences  int64
	UnavailableOccurrences int64
}

type VisualCandidatePage struct {
	Candidates         []VisualCandidate
	Counts             VisualCandidateCounts
	NextAfterMessageID int64
	HasMore            bool
}

type VisualMessageContext struct {
	Subject     string
	Body        string
	MessageType string
	// ContentStamp is the message's content_changed_at CAS stamp read in the
	// same statement as the context columns; claims record it so commits can
	// refuse a document assembled from a superseded snapshot.
	ContentStamp string
}

// ListVisualCandidates returns source-authoritative standalone owners for a
// bounded message page. A hash match never reaches a different message, and
// the lowest attachment ID is the deterministic representative occurrence.
func (s *Store) ListVisualCandidates(
	ctx context.Context,
	filter VisualCandidateFilter,
) (VisualCandidatePage, error) {
	messageIDs, hasMore, err := s.visualCandidateMessagePage(ctx, filter)
	if err != nil || len(messageIDs) == 0 {
		return VisualCandidatePage{HasMore: hasMore}, err
	}
	page := VisualCandidatePage{
		Candidates:         make([]VisualCandidate, 0),
		NextAfterMessageID: messageIDs[len(messageIDs)-1],
		HasMore:            hasMore,
	}
	placeholders := make([]string, len(messageIDs))
	args := make([]any, len(messageIDs))
	for i, id := range messageIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(`
		SELECT a.id, a.message_id, COALESCE(a.filename, ''),
		       COALESCE(a.mime_type, ''), COALESCE(a.size, 0),
		       COALESCE(a.content_hash, ''), COALESCE(a.storage_path, ''),
		       COALESCE(a.width, 0), COALESCE(a.height, 0),
		       COALESCE(a.duration_ms, 0), a.attachment_role, a.role_source,
		       COALESCE(m.message_type, '')
		FROM attachments a
		JOIN messages m ON m.id = a.message_id
		WHERE a.message_id IN (`+strings.Join(placeholders, ",")+`)
		  AND `+LiveMessagesWhere("m", true)+`
		ORDER BY a.message_id, a.id`), args...)
	if err != nil {
		return VisualCandidatePage{}, fmt.Errorf("list visual candidate occurrences: %w", err)
	}
	defer func() { _ = rows.Close() }()

	byOwner := make(map[VisualOwner]int)
	for rows.Next() {
		var candidate VisualCandidate
		var contentHash, storagePath string
		if err := rows.Scan(
			&candidate.RepresentativeAttachmentID, &candidate.Owner.MessageID,
			&candidate.Filename, &candidate.DeclaredMIME, &candidate.Size,
			&contentHash, &storagePath, &candidate.Width, &candidate.Height,
			&candidate.DurationMS, &candidate.Role, &candidate.RoleSource,
			&candidate.MessageType,
		); err != nil {
			return VisualCandidatePage{}, fmt.Errorf("scan visual candidate occurrence: %w", err)
		}
		if candidate.Role == AttachmentRoleUnknown || !authoritativeVisualRoleSource(candidate.RoleSource) {
			page.Counts.UnknownRoleOccurrences++
			continue
		}
		if candidate.Role != AttachmentRoleStandalone {
			page.Counts.IneligibleOccurrences++
			continue
		}
		page.Counts.StandaloneOccurrences++
		hash, ok := canonicalVisualBlobHash(contentHash, storagePath)
		if !ok {
			page.Counts.UnavailableOccurrences++
			continue
		}
		candidate.Owner.BlobHash = hash
		candidate.Owner.MediaInputKey = VisualOriginalMediaInputKey
		candidate.OccurrenceCount = 1
		if index, exists := byOwner[candidate.Owner]; exists {
			page.Candidates[index].OccurrenceCount++
			continue
		}
		byOwner[candidate.Owner] = len(page.Candidates)
		page.Candidates = append(page.Candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return VisualCandidatePage{}, fmt.Errorf("iterate visual candidate occurrences: %w", err)
	}
	return page, nil
}

func (s *Store) visualCandidateMessagePage(
	ctx context.Context,
	filter VisualCandidateFilter,
) ([]int64, bool, error) {
	if filter.AfterMessageID < 0 {
		return nil, false, errors.New("visual candidate cursor cannot be negative")
	}
	messageTypes := normalizedVisualMessageTypes(filter.MessageTypes)
	sourceIDs, err := normalizedVisualSourceIDs(filter.SourceIDs)
	if err != nil {
		return nil, false, err
	}
	if len(filter.MessageIDs) > 0 {
		if filter.AfterMessageID != 0 || filter.LimitMessages != 0 {
			return nil, false, errors.New("explicit visual message IDs cannot be combined with pagination")
		}
		ids, err := normalizedPositiveIDs(filter.MessageIDs, 10_000)
		if err != nil {
			return nil, false, err
		}
		return s.selectVisualCandidateMessageIDs(ctx, ids, messageTypes, sourceIDs)
	}
	limit := filter.LimitMessages
	if limit == 0 {
		limit = 500
	}
	if limit < 1 || limit > 1000 {
		return nil, false, errors.New("visual candidate message limit must be between 1 and 1000")
	}
	args := []any{filter.AfterMessageID}
	where := `m.id > ? AND ` + LiveMessagesWhere("m", true) + `
		AND EXISTS (SELECT 1 FROM attachments a WHERE a.message_id = m.id)`
	if len(messageTypes) > 0 {
		where += " AND LOWER(TRIM(m.message_type)) IN (" + sqlPlaceholders(len(messageTypes)) + ")"
		for _, messageType := range messageTypes {
			args = append(args, messageType)
		}
	}
	if len(sourceIDs) > 0 {
		where += " AND m.source_id IN (" + sqlPlaceholders(len(sourceIDs)) + ")"
		for _, sourceID := range sourceIDs {
			args = append(args, sourceID)
		}
	}
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(`
		SELECT m.id FROM messages m WHERE `+where+` ORDER BY m.id LIMIT ?`), args...)
	if err != nil {
		return nil, false, fmt.Errorf("page visual candidate messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	ids := make([]int64, 0, limit+1)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, false, fmt.Errorf("scan visual candidate message: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("iterate visual candidate messages: %w", err)
	}
	hasMore := len(ids) > limit
	if hasMore {
		ids = ids[:limit]
	}
	return ids, hasMore, nil
}

func (s *Store) selectVisualCandidateMessageIDs(
	ctx context.Context,
	ids []int64,
	messageTypes []string,
	sourceIDs []int64,
) ([]int64, bool, error) {
	args := make([]any, 0, len(ids)+len(messageTypes)+len(sourceIDs))
	for _, id := range ids {
		args = append(args, id)
	}
	where := "m.id IN (" + sqlPlaceholders(len(ids)) + ") AND " + LiveMessagesWhere("m", true) +
		" AND EXISTS (SELECT 1 FROM attachments a WHERE a.message_id = m.id)"
	if len(messageTypes) > 0 {
		where += " AND LOWER(TRIM(m.message_type)) IN (" + sqlPlaceholders(len(messageTypes)) + ")"
		for _, messageType := range messageTypes {
			args = append(args, messageType)
		}
	}
	if len(sourceIDs) > 0 {
		where += " AND m.source_id IN (" + sqlPlaceholders(len(sourceIDs)) + ")"
		for _, sourceID := range sourceIDs {
			args = append(args, sourceID)
		}
	}
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(
		"SELECT m.id FROM messages m WHERE "+where+" ORDER BY m.id"), args...)
	if err != nil {
		return nil, false, fmt.Errorf("select visual candidate messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	selected := make([]int64, 0, len(ids))
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, false, fmt.Errorf("scan selected visual candidate message: %w", err)
		}
		selected = append(selected, id)
	}
	return selected, false, rows.Err()
}

// GetVisualMessageContext reads message bodies only by their primary key. It
// deliberately remains separate from the candidate scan so list queries never
// scan or join the large message_bodies table.
func (s *Store) GetVisualMessageContext(ctx context.Context, messageID int64) (VisualMessageContext, error) {
	if messageID <= 0 {
		return VisualMessageContext{}, errors.New("visual message ID must be positive")
	}
	var result VisualMessageContext
	// The stamp is read in the same statement as the context columns it
	// covers, so a claim recording this stamp makes any later edit —
	// including one racing this read — fail the commit-time CAS.
	err := s.db.QueryRowContext(ctx, s.dialect.Rebind(`
		SELECT COALESCE(subject, ''), COALESCE(message_type, ''), `+visualContentStampExpr+`
		FROM messages WHERE id = ? AND `+LiveMessagesWhere("", true)), messageID).
		Scan(&result.Subject, &result.MessageType, &result.ContentStamp)
	if err != nil {
		return VisualMessageContext{}, fmt.Errorf("read visual message context: %w", err)
	}
	var bodyText, bodyHTML sql.NullString
	err = s.db.QueryRowContext(ctx, s.dialect.Rebind(`
		SELECT body_text, body_html FROM message_bodies WHERE message_id = ?`), messageID).
		Scan(&bodyText, &bodyHTML)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return VisualMessageContext{}, fmt.Errorf("read visual message body: %w", err)
	}
	result.Body = embeddingBodyValue(bodyText, bodyHTML)
	return result, nil
}

func canonicalVisualBlobHash(contentHash, storagePath string) (string, bool) {
	contentHash = strings.ToLower(strings.TrimSpace(contentHash))
	if contentHash == "" {
		return casPathHash(storagePath)
	}
	if len(contentHash) != 64 {
		return "", false
	}
	if _, err := hex.DecodeString(contentHash); err != nil {
		return "", false
	}
	return contentHash, true
}

// normalizedVisualSourceIDs deduplicates and orders the account scope.
func normalizedVisualSourceIDs(values []int64) ([]int64, error) {
	if len(values) == 0 {
		return nil, nil
	}
	return normalizedPositiveIDs(values, 10_000)
}

func normalizedVisualMessageTypes(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	slices.Sort(result)
	return result
}

func normalizedPositiveIDs(values []int64, limit int) ([]int64, error) {
	if len(values) > limit {
		return nil, fmt.Errorf("visual message ID count must not exceed %d", limit)
	}
	seen := make(map[int64]struct{}, len(values))
	result := make([]int64, 0, len(values))
	for _, value := range values {
		if value <= 0 {
			return nil, errors.New("visual message IDs must be positive")
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	slices.Sort(result)
	return result, nil
}

func sqlPlaceholders(count int) string {
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}
