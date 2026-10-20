package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// TopicAuthor is one message with the participant that sent it and the
// durable person that participant is bound to, if any.
type TopicAuthor struct {
	MessageID   int64
	IsFromMe    bool
	OwnerLinked bool
	ListMail    bool
	Automated   bool
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

	// SenderAuthenticated reports that the source sets the sender, rather
	// than the message header asserting it.
	SenderAuthenticated bool
}

// PersonAskMeAbout is one current, user-declared ask-me-about value.
type PersonAskMeAbout struct {
	PersonID   int64
	PersonName string
	Value      string
}

// automatedAuthor is true when the producer marked the message's author as
// automation: Discord stores a flag in messages.metadata, Slack namespaces
// bots as "<team>:bot:<id>" participant identifiers, and Chatwoot keeps bots
// and assistants under their own path segment. Importers write no other
// marker.
func (s *Store) automatedAuthor() string {
	flag := "COALESCE(CASE WHEN json_valid(m.metadata) THEN json_extract(m.metadata, '$.automated') = 1 END, FALSE)"
	if s.IsPostgreSQL() {
		flag = "COALESCE(m.metadata ->> 'automated' = 'true', FALSE)"
	}
	return `(` + flag + ` OR EXISTS (
		         SELECT 1 FROM participant_identifiers bi
		          WHERE bi.participant_id = p.id AND (
		                (bi.identifier_type = 'slack' AND bi.identifier_value LIKE '%:bot:%')
		             OR (bi.identifier_type = 'chatwoot' AND (
		                    bi.identifier_value LIKE '%/agent\_bot/%' ESCAPE '\'
		                 OR bi.identifier_value LIKE '%/captain\_assistant/%' ESCAPE '\'))))) `
}

// ownerParticipantMatch is true when participant alias op is one of the
// archive owner's account identities, by address or by identifier, as the
// activity projector decides it.
func ownerParticipantMatch(op string) string {
	noPrimaryEmail := " AND (" + op + ".email_address IS NULL OR TRIM(" + op + ".email_address) = '')"
	return `EXISTS (SELECT 1 FROM account_identities ai
		         WHERE (` + ownerEmailMatch(op+".email_address", "ai.address") + `)
		            OR EXISTS (SELECT 1 FROM participant_identifiers oi
		                       WHERE oi.participant_id = ` + op + `.id
		                         AND ` + ownerIdentifierMatch("oi.identifier_type", "oi.identifier_value", "ai.address", noPrimaryEmail) + `))`
}

// TopicAuthorsContext reads the sender of each message at request time, so
// mail synced since the last activity build still counts. A message's sender
// is messages.sender_id, else its lowest 'from' recipient. OwnerLinked reports
// that the sender, or any participant bound to the sender's person, is one of
// the owner's account identities. Deleted messages are left out of the result.
func (s *Store) TopicAuthorsContext(ctx context.Context, messageIDs []int64) (map[int64]TopicAuthor, error) {
	out := map[int64]TopicAuthor{}
	if len(messageIDs) == 0 {
		return out, nil
	}
	args := make([]any, len(messageIDs))
	for i, id := range messageIDs {
		args[i] = id
	}
	query := `
		SELECT m.id, COALESCE(m.is_from_me, FALSE),
		       EXISTS (SELECT 1 FROM participants op
		                WHERE (op.id = p.id OR op.id IN (
		                        SELECT bound.participant_id FROM person_participants bound
		                         WHERE bound.person_id = pp.person_id))
		                  AND ` + ownerParticipantMatch("op") + `),
		       COALESCE(m.list_id, '') <> '',
		       ` + s.automatedAuthor() + `, m.message_type,
		       m.sent_at, COALESCE(m.subject, ''), COALESCE(m.snippet, ''), src.source_type,
		       p.id, COALESCE(p.email_address, ''), COALESCE(p.display_name, ''),
		       pp.person_id, COALESCE(per.display_name, '')
		FROM messages m
		JOIN sources src ON src.id = m.source_id
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
			a          TopicAuthor
			sentAt     sql.NullTime
			sender     sql.NullInt64
			personID   sql.NullInt64
			sourceType string
		)
		if err := rows.Scan(&a.MessageID, &a.IsFromMe, &a.OwnerLinked, &a.ListMail, &a.Automated, &a.MessageType,
			&sentAt, &a.Subject, &a.Snippet, &sourceType,
			&sender, &a.SenderEmail, &a.SenderName,
			&personID, &a.PersonName); err != nil {
			return nil, fmt.Errorf("scan topic author: %w", err)
		}
		if sentAt.Valid {
			a.SentAt = &sentAt.Time
		}
		a.SenderID = sender.Int64
		if personID.Valid {
			a.PersonID = &personID.Int64
		}
		a.SenderAuthenticated = personSweepSourceAuthenticatesSender(sourceType)
		out[a.MessageID] = a
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read topic authors: %w", err)
	}
	return out, nil
}

// OwnerAddressedContext returns the given participants the archive owner has
// written to: the owner sent a message that lists the participant, or one
// of the other participants bound to the same person, as a recipient or as a
// member of its conversation. A participant the owner never addressed is a
// one-way sender.
func (s *Store) OwnerAddressedContext(ctx context.Context, participantIDs []int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	if len(participantIDs) == 0 {
		return out, nil
	}
	args := make([]any, 0, 2*len(participantIDs))
	for range 2 {
		for _, id := range participantIDs {
			args = append(args, id)
		}
	}
	list := directoryPlaceholders(len(participantIDs))
	query := `
		WITH family(asked_id, member_id) AS (
		  SELECT p.id, p.id FROM participants p WHERE p.id IN (` + list + `)
		  UNION
		  SELECT mine.participant_id, sibling.participant_id FROM person_participants mine
		    JOIN person_participants sibling ON sibling.person_id = mine.person_id
		   WHERE mine.participant_id IN (` + list + `)
		), addressed(participant_id) AS (
		  SELECT mr.participant_id FROM message_recipients mr
		    JOIN messages m ON m.id = mr.message_id
		   WHERE m.is_from_me AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL
		     AND LOWER(mr.recipient_type) IN ('to', 'cc', 'bcc')
		     AND mr.participant_id IN (SELECT member_id FROM family)
		  UNION
		  SELECT cp.participant_id FROM conversation_participants cp
		    JOIN messages m ON m.conversation_id = cp.conversation_id
		   WHERE m.is_from_me AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL
		     AND cp.participant_id IN (SELECT member_id FROM family)
		)
		SELECT DISTINCT asked_id FROM family
		 WHERE member_id IN (SELECT participant_id FROM addressed)`
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("read owner-addressed participants: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan owner-addressed participant: %w", err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read owner-addressed participants: %w", err)
	}
	return out, nil
}

// AskMeAboutValuesContext returns the current ask-me-about values the user
// typed or an address-book import supplied that contain at least one of the
// given lower-case words, so the caller can match whole topics in Go without
// reading every value. Inferred and superseded values are left out, and so
// are people bound to one of the owner's account identities. A word with
// non-ASCII letters cannot be matched case-insensitively in SQL, so it turns
// the word filter off.
func (s *Store) AskMeAboutValuesContext(ctx context.Context, words []string) ([]PersonAskMeAbout, error) {
	if len(words) == 0 {
		return nil, nil
	}
	args := []any{AttributeUniversalIDAskMeAbout}
	var likes []string
	filter := true
	escape := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	for _, word := range words {
		if strings.IndexFunc(word, func(r rune) bool { return r > unicode.MaxASCII }) >= 0 {
			filter = false
			break
		}
		likes = append(likes, `LOWER(v.value_text) LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escape.Replace(word)+"%")
	}
	wordFilter := ""
	if filter {
		wordFilter = "AND (" + strings.Join(likes, " OR ") + ")"
	} else {
		args = args[:1]
	}
	query := `
		SELECT v.person_id, COALESCE(per.display_name, ''), v.value_text
		FROM person_attribute_values v
		JOIN attribute_definitions d ON d.id = v.definition_id
		JOIN persons per ON per.id = v.person_id
		WHERE d.universal_id = ? AND v.active_until IS NULL AND v.superseded_at IS NULL
		  AND v.source IN ('user', 'carddav_import', 'vcard_import')
		  ` + wordFilter + `
		  AND NOT EXISTS (
		    SELECT 1 FROM person_participants bound
		      JOIN participants op ON op.id = bound.participant_id
		     WHERE bound.person_id = v.person_id AND ` + ownerParticipantMatch("op") + `)
		ORDER BY v.person_id, v.ordinal`
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("read ask-me-about values: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []PersonAskMeAbout
	for rows.Next() {
		var v PersonAskMeAbout
		if err := rows.Scan(&v.PersonID, &v.PersonName, &v.Value); err != nil {
			return nil, fmt.Errorf("scan ask-me-about value: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read ask-me-about values: %w", err)
	}
	return out, nil
}
