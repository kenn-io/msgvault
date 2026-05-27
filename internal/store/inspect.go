package store

import (
	"database/sql"
	"errors"
)

// inspectTimeLayout is the stable format used to render TIMESTAMP /
// TIMESTAMPTZ columns scanned out of either backend for test
// assertions. Date and clock components are spelled out separately so
// assertDateFallback's `strings.Contains` checks continue to match.
const inspectTimeLayout = "2006-01-02 15:04:05"

func formatInspectTime(t sql.NullTime) string {
	if !t.Valid {
		return ""
	}
	return t.Time.UTC().Format(inspectTimeLayout)
}

// MessageInspection contains detailed message data for test assertions.
type MessageInspection struct {
	SentAt               string
	InternalDate         string
	DeletedFromSourceAt  sql.NullTime
	ThreadSourceID       string
	BodyText             string
	RawDataExists        bool
	RecipientCounts      map[string]int    // recipient_type -> count
	RecipientDisplayName map[string]string // "type:email" -> display_name
}

// InspectMessage retrieves detailed message information for test assertions.
// This consolidates multiple schema-aware queries into a single method,
// keeping schema knowledge in the store package.
func (s *Store) InspectMessage(sourceMessageID string) (*MessageInspection, error) {
	insp := &MessageInspection{
		RecipientCounts:      make(map[string]int),
		RecipientDisplayName: make(map[string]string),
	}

	// Get basic message fields and thread info. Scan timestamps as
	// sql.NullTime — pgx decodes TIMESTAMPTZ into time.Time and refuses
	// the conversion to *sql.NullString that the old code attempted, so
	// the prior shape errored out on PostgreSQL before returning a row.
	var sentAt, internalDate sql.NullTime
	err := s.db.QueryRow(`
		SELECT m.sent_at, m.internal_date, m.deleted_from_source_at, c.source_conversation_id
		FROM messages m
		JOIN conversations c ON m.conversation_id = c.id
		WHERE m.source_message_id = ?
	`, sourceMessageID).Scan(&sentAt, &internalDate, &insp.DeletedFromSourceAt, &insp.ThreadSourceID)
	if err != nil {
		return nil, err
	}
	insp.SentAt = formatInspectTime(sentAt)
	insp.InternalDate = formatInspectTime(internalDate)

	// Get body text
	var bodyText sql.NullString
	err = s.db.QueryRow(`
		SELECT mb.body_text FROM message_bodies mb
		JOIN messages m ON m.id = mb.message_id
		WHERE m.source_message_id = ?
	`, sourceMessageID).Scan(&bodyText)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if bodyText.Valid {
		insp.BodyText = bodyText.String
	}

	// Check raw data existence
	var rawExists int
	err = s.db.QueryRow(`
		SELECT 1 FROM message_raw mr
		JOIN messages m ON m.id = mr.message_id
		WHERE m.source_message_id = ?
	`, sourceMessageID).Scan(&rawExists)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	insp.RawDataExists = err == nil

	// Get recipient counts by type
	rows, err := s.db.Query(`
		SELECT mr.recipient_type, COUNT(*) FROM message_recipients mr
		JOIN messages m ON mr.message_id = m.id
		WHERE m.source_message_id = ?
		GROUP BY mr.recipient_type
	`, sourceMessageID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var recipType string
		var count int
		if err := rows.Scan(&recipType, &count); err != nil {
			return nil, err
		}
		insp.RecipientCounts[recipType] = count
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Get recipient display names
	rows, err = s.db.Query(`
		SELECT mr.recipient_type, p.email_address, mr.display_name
		FROM message_recipients mr
		JOIN messages m ON mr.message_id = m.id
		JOIN participants p ON mr.participant_id = p.id
		WHERE m.source_message_id = ?
	`, sourceMessageID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var recipType, email, displayName string
		if err := rows.Scan(&recipType, &email, &displayName); err != nil {
			return nil, err
		}
		key := recipType + ":" + email
		insp.RecipientDisplayName[key] = displayName
	}

	return insp, rows.Err()
}

// InspectRecipientCount returns the count of recipients of a given type for a message.
func (s *Store) InspectRecipientCount(sourceMessageID, recipientType string) (int, error) {
	var count int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM message_recipients mr
		JOIN messages m ON mr.message_id = m.id
		WHERE m.source_message_id = ? AND mr.recipient_type = ?
	`, sourceMessageID, recipientType).Scan(&count)
	return count, err
}

// InspectDisplayName returns the display name for a recipient of a message.
func (s *Store) InspectDisplayName(sourceMessageID, recipientType, email string) (string, error) {
	var displayName string
	err := s.db.QueryRow(`
		SELECT mr.display_name FROM message_recipients mr
		JOIN messages m ON mr.message_id = m.id
		JOIN participants p ON mr.participant_id = p.id
		WHERE m.source_message_id = ? AND mr.recipient_type = ? AND p.email_address = ?
	`, sourceMessageID, recipientType, email).Scan(&displayName)
	return displayName, err
}

// InspectParticipantDisplayName returns the display_name from the participants table for an email.
func (s *Store) InspectParticipantDisplayName(email string) (string, error) {
	var displayName string
	err := s.db.QueryRow(
		"SELECT display_name FROM participants WHERE email_address = ?",
		email).Scan(&displayName)
	return displayName, err
}

// InspectDeletedFromSource checks whether a message has deleted_from_source_at set.
func (s *Store) InspectDeletedFromSource(sourceMessageID string) (bool, error) {
	var deletedAt sql.NullTime
	err := s.db.QueryRow(
		"SELECT deleted_from_source_at FROM messages WHERE source_message_id = ?",
		sourceMessageID).Scan(&deletedAt)
	if err != nil {
		return false, err
	}
	return deletedAt.Valid, nil
}

// InspectBodyText returns the body_text for a message.
func (s *Store) InspectBodyText(sourceMessageID string) (string, error) {
	var bodyText string
	err := s.db.QueryRow(`
		SELECT mb.body_text FROM message_bodies mb
		JOIN messages m ON m.id = mb.message_id
		WHERE m.source_message_id = ?`, sourceMessageID).Scan(&bodyText)
	return bodyText, err
}

// InspectRawDataExists checks that raw MIME data exists for a message.
func (s *Store) InspectRawDataExists(sourceMessageID string) (bool, error) {
	var rawData []byte
	err := s.db.QueryRow(`
		SELECT raw_data FROM message_raw mr
		JOIN messages m ON m.id = mr.message_id
		WHERE m.source_message_id = ?`, sourceMessageID).Scan(&rawData)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return len(rawData) > 0, nil
}

// InspectThreadSourceID returns the source_conversation_id for a message's thread.
func (s *Store) InspectThreadSourceID(sourceMessageID string) (string, error) {
	var threadSourceID string
	err := s.db.QueryRow(`
		SELECT c.source_conversation_id FROM conversations c
		JOIN messages m ON m.conversation_id = c.id
		WHERE m.source_message_id = ?
	`, sourceMessageID).Scan(&threadSourceID)
	return threadSourceID, err
}

// InspectAttachment returns the filename and mime_type for the first attachment (by ID) of a message.
func (s *Store) InspectAttachment(sourceMessageID string) (filename, mimeType string, err error) {
	err = s.db.QueryRow(`
		SELECT a.filename, a.mime_type FROM attachments a
		JOIN messages m ON a.message_id = m.id
		WHERE m.source_message_id = ?
		ORDER BY a.id
		LIMIT 1
	`, sourceMessageID).Scan(&filename, &mimeType)
	return
}

// InspectMessageDates returns sent_at and internal_date for a message,
// formatted as "2006-01-02 15:04:05" in UTC. Scanning directly into
// *string fails on PostgreSQL — pgx decodes TIMESTAMPTZ into time.Time
// and refuses the implicit conversion — so the read goes through
// sql.NullTime first.
func (s *Store) InspectMessageDates(sourceMessageID string) (sentAt, internalDate string, err error) {
	var sentAtT, internalDateT sql.NullTime
	err = s.db.QueryRow(
		"SELECT sent_at, internal_date FROM messages WHERE source_message_id = ?",
		sourceMessageID).Scan(&sentAtT, &internalDateT)
	if err != nil {
		return "", "", err
	}
	return formatInspectTime(sentAtT), formatInspectTime(internalDateT), nil
}
