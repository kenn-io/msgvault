package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"
)

const (
	// MaxTopicAuthorMessages bounds one TopicAuthorsContext read.
	MaxTopicAuthorMessages = 1000
	// MaxAskMeAboutPeople bounds one AskMeAboutValuesContext read.
	MaxAskMeAboutPeople = 500
)

// TopicAuthor is one message with the participant that sent it and the
// durable person that participant is bound to, if any.
type TopicAuthor struct {
	MessageID   int64
	IsFromMe    bool
	MessageType string
	SentAt      *time.Time
	Subject     string
	Snippet     string

	// SenderID is zero when the message has no resolvable sender.
	SenderID    int64
	SenderEmail string
	SenderName  string
	PersonID    *int64
	PersonName  string
	OwnerLinked bool
}

// PersonAskMeAbout is one current, user-declared ask-me-about value.
type PersonAskMeAbout struct {
	PersonID int64
	Value    string
}

// TopicAuthorsContext reads the sender of each message at request time, so
// mail synced since the last activity build still counts. A message's sender
// is messages.sender_id, else its lowest 'from' recipient. OwnerLinked
// reports a sender, or any participant bound to the sender's person, that
// matches one of the archive owner's account identities. Deleted messages
// are left out of the result.
func (s *Store) TopicAuthorsContext(ctx context.Context, messageIDs []int64) (map[int64]TopicAuthor, error) {
	out := map[int64]TopicAuthor{}
	if len(messageIDs) == 0 {
		return out, nil
	}
	if len(messageIDs) > MaxTopicAuthorMessages {
		return nil, fmt.Errorf("topic authors: at most %d message IDs", MaxTopicAuthorMessages)
	}
	args := make([]any, len(messageIDs))
	for i, id := range messageIDs {
		args[i] = id
	}
	const emailGuard = " AND (op.email_address IS NULL OR TRIM(op.email_address) = '')"
	query := `
		SELECT m.id, COALESCE(m.is_from_me, FALSE), m.message_type,
		       m.sent_at, COALESCE(m.subject, ''), COALESCE(m.snippet, ''),
		       p.id, COALESCE(p.email_address, ''), COALESCE(p.display_name, ''),
		       pp.person_id, COALESCE(per.display_name, ''),
		       EXISTS (
		         SELECT 1 FROM participants op
		         WHERE (op.id = p.id OR op.id IN (
		                 SELECT bound.participant_id FROM person_participants bound
		                 WHERE bound.person_id = pp.person_id))
		           AND EXISTS (
		             SELECT 1 FROM account_identities ai
		             WHERE (` + ownerEmailMatch("op.email_address", "ai.address") + `)
		                OR EXISTS (
		                  SELECT 1 FROM participant_identifiers pi
		                  WHERE pi.participant_id = op.id
		                    AND ` + ownerIdentifierMatch("pi.identifier_type", "pi.identifier_value", "ai.address", emailGuard) + `))
		       )
		FROM messages m
		LEFT JOIN participants p ON p.id = COALESCE(m.sender_id, (
		       SELECT MIN(mr.participant_id) FROM message_recipients mr
		       WHERE mr.message_id = m.id AND LOWER(mr.recipient_type) = 'from'
		         AND mr.participant_id IS NOT NULL))
		LEFT JOIN person_participants pp ON pp.participant_id = p.id
		LEFT JOIN persons per ON per.id = pp.person_id
		WHERE m.id IN (` + directoryPlaceholders(len(messageIDs)) + `)
		  AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL`
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("read topic authors: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			a        TopicAuthor
			sentAt   sql.NullTime
			sender   sql.NullInt64
			personID sql.NullInt64
		)
		if err := rows.Scan(&a.MessageID, &a.IsFromMe, &a.MessageType,
			&sentAt, &a.Subject, &a.Snippet,
			&sender, &a.SenderEmail, &a.SenderName,
			&personID, &a.PersonName, &a.OwnerLinked); err != nil {
			return nil, fmt.Errorf("scan topic author: %w", err)
		}
		if sentAt.Valid {
			a.SentAt = &sentAt.Time
		}
		a.SenderID = sender.Int64
		if personID.Valid {
			a.PersonID = &personID.Int64
		}
		out[a.MessageID] = a
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read topic authors: %w", err)
	}
	return out, nil
}

// AskMeAboutValuesContext returns the current ask-me-about values the user
// typed or an address-book import supplied for the given people. Inferred and
// superseded values are left out.
func (s *Store) AskMeAboutValuesContext(ctx context.Context, personIDs []int64) ([]PersonAskMeAbout, error) {
	if len(personIDs) == 0 {
		return nil, nil
	}
	if len(personIDs) > MaxAskMeAboutPeople {
		return nil, fmt.Errorf("ask-me-about values: at most %d person IDs", MaxAskMeAboutPeople)
	}
	args := []any{AttributeUniversalIDAskMeAbout}
	for _, id := range slices.Compact(slices.Sorted(slices.Values(personIDs))) {
		args = append(args, id)
	}
	query := `
		SELECT v.person_id, v.value_text
		FROM person_attribute_values v
		JOIN attribute_definitions d ON d.id = v.definition_id
		WHERE d.universal_id = ? AND v.active_until IS NULL AND v.superseded_at IS NULL
		  AND v.source IN ('user', 'carddav_import', 'vcard_import')
		  AND v.person_id IN (` + directoryPlaceholders(len(args)-1) + `)
		ORDER BY v.person_id, v.ordinal`
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("read ask-me-about values: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []PersonAskMeAbout
	for rows.Next() {
		var v PersonAskMeAbout
		if err := rows.Scan(&v.PersonID, &v.Value); err != nil {
			return nil, fmt.Errorf("scan ask-me-about value: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read ask-me-about values: %w", err)
	}
	return out, nil
}
