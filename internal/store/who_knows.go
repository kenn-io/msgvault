package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"
	"time"
)

// TopicAuthor is one message with the participant that sent it.
type TopicAuthor struct {
	MessageID   int64
	IsFromMe    bool
	ListMail    bool
	Automated   bool
	MessageType string
	SentAt      *time.Time
	Subject     string

	// SenderID is zero when the message has no resolvable sender.
	SenderID    int64
	SenderEmail string
	SenderName  string

	// SenderAuthenticated reports that the source sets the sender, rather
	// than the message header asserting it.
	SenderAuthenticated bool
}

// AuthorIdentity is the identity family of a sender: every participant
// bound to its saved person, or its link cluster when it has none.
type AuthorIdentity struct {
	// Key is the smallest participant ID in the family.
	Key int64
	// PersonID is zero when no member is bound to a saved person.
	PersonID   int64
	PersonName string
	// Owner reports that a member is one of the owner's account identities.
	Owner bool
	// Addressed reports that the owner wrote to a member: as a recipient of
	// an owner message, or as a member of a conversation the owner wrote in.
	Addressed bool
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

// messageSender is the participant that sent message alias m: its
// sender_id, else its lowest 'from' recipient.
func messageSender(m string) string {
	return `COALESCE(` + m + `.sender_id, (
		       SELECT MIN(fr.participant_id) FROM message_recipients fr
		       WHERE fr.message_id = ` + m + `.id AND LOWER(fr.recipient_type) = 'from'
		         AND fr.participant_id IS NOT NULL))`
}

// TopicAuthorsContext reads the sender of each message at request time, so
// mail synced since the last activity build still counts. A message's sender
// is messages.sender_id, else its lowest 'from' recipient. Deleted messages
// are left out of the result.
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
		SELECT m.id, COALESCE(m.is_from_me, FALSE), COALESCE(m.list_id, '') <> '',
		       ` + s.automatedAuthor() + `, m.message_type,
		       m.sent_at, COALESCE(m.subject, ''), src.source_type,
		       p.id, COALESCE(p.email_address, ''), COALESCE(p.display_name, '')
		FROM messages m
		JOIN sources src ON src.id = m.source_id
		LEFT JOIN participants p ON p.id = ` + messageSender("m") + `
		WHERE m.id IN (` + directoryPlaceholders(len(messageIDs)) + `)
		  AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL`
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("read topic authors: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var slackIDs []int64
	for rows.Next() {
		var (
			a          TopicAuthor
			sentAt     sql.NullTime
			sender     sql.NullInt64
			sourceType string
		)
		if err := rows.Scan(&a.MessageID, &a.IsFromMe, &a.ListMail, &a.Automated, &a.MessageType,
			&sentAt, &a.Subject, &sourceType, &sender, &a.SenderEmail, &a.SenderName); err != nil {
			return nil, fmt.Errorf("scan topic author: %w", err)
		}
		if sentAt.Valid {
			a.SentAt = &sentAt.Time
		}
		a.SenderID = sender.Int64
		a.SenderAuthenticated = personSweepSourceAuthenticatesSender(sourceType)
		out[a.MessageID] = a
		if a.MessageType == slackMessageType && !a.Automated {
			slackIDs = append(slackIDs, a.MessageID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read topic authors: %w", err)
	}
	if err := s.markSlackBotPayloads(ctx, out, slackIDs); err != nil {
		return nil, err
	}
	return out, nil
}

// slackMessageType is the message_type the Slack importer writes.
const slackMessageType = "slack"

// markSlackBotPayloads marks Slack messages whose archived payload carries a
// bot_id or the bot_message subtype. The importer files a message carrying
// both "user" and "bot_id" under the user, so only the payload shows it. An
// unreadable payload leaves the message counted.
func (s *Store) markSlackBotPayloads(ctx context.Context, authors map[int64]TopicAuthor, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	raws, err := s.GetMessageRawsContext(ctx, ids)
	if err != nil {
		return fmt.Errorf("read Slack payloads: %w", err)
	}
	for id, raw := range raws {
		var payload struct {
			BotID   string `json:"bot_id"`
			Subtype string `json:"subtype"`
		}
		if json.Unmarshal(raw, &payload) == nil && (payload.BotID != "" || payload.Subtype == "bot_message") {
			a := authors[id]
			a.Automated = true
			authors[id] = a
		}
	}
	return nil
}

// AuthorIdentitiesContext groups senders into identity families and maps
// each sender to its family's identity. Promotion, linking, and merging bind
// a whole link cluster to at most one person, so a saved sender's family is
// every participant bound to its person, and an unsaved sender's family is its
// link cluster.
func (s *Store) AuthorIdentitiesContext(ctx context.Context, senderIDs []int64) (map[int64]AuthorIdentity, error) {
	out := map[int64]AuthorIdentity{}
	if len(senderIDs) == 0 {
		return out, nil
	}
	clusters, err := s.ParticipantClustersContext(ctx)
	if err != nil {
		return nil, err
	}
	families, err := s.identityFamiliesContext(ctx, senderIDs, clusters)
	if err != nil {
		return nil, err
	}
	owners, err := s.ownerFamiliesContext(ctx, clusters)
	if err != nil {
		return nil, err
	}
	byKey, err := s.authorFlagsContext(ctx, families, owners)
	if err != nil {
		return nil, err
	}
	for _, id := range senderIDs {
		out[id] = byKey[families[id].Key]
	}
	return out, nil
}

// identityFamiliesContext maps every member of each listed participant's
// identity family to that family's identity.
func (s *Store) identityFamiliesContext(
	ctx context.Context, ids []int64, clusters map[int64]int64,
) (map[int64]AuthorIdentity, error) {
	families, err := s.authorPersonsContext(ctx, ids)
	if err != nil {
		return nil, err
	}
	unsavedRoots := map[int64]bool{}
	for _, id := range ids {
		if _, saved := families[id]; !saved {
			root := cmp.Or(clusters[id], id)
			unsavedRoots[root] = true
			families[id] = AuthorIdentity{Key: root}
		}
	}
	for member, root := range clusters {
		if unsavedRoots[root] {
			families[member] = AuthorIdentity{Key: root}
		}
	}
	return families, nil
}

// ownerFamiliesContext returns every member of the identity families of the
// owner's account identities. A linked or bound alias that is not itself an
// account identity is the owner too.
func (s *Store) ownerFamiliesContext(ctx context.Context, clusters map[int64]int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.id FROM participants p WHERE `+ownerParticipantMatch("p"))
	if err != nil {
		return nil, fmt.Errorf("read owner participants: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan owner participant: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read owner participants: %w", err)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	families, err := s.identityFamiliesContext(ctx, ids, clusters)
	if err != nil {
		return nil, err
	}
	return slices.Sorted(maps.Keys(families)), nil
}

// authorPersonsContext maps every participant bound to a sender's saved
// person to that person's identity, keyed by its smallest participant ID.
func (s *Store) authorPersonsContext(ctx context.Context, senderIDs []int64) (map[int64]AuthorIdentity, error) {
	args := make([]any, len(senderIDs))
	for i, id := range senderIDs {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(`
		SELECT pp.participant_id, pp.person_id, COALESCE(per.display_name, '')
		FROM person_participants pp
		JOIN persons per ON per.id = pp.person_id
		WHERE pp.person_id IN (SELECT person_id FROM person_participants
		                       WHERE participant_id IN (`+directoryPlaceholders(len(senderIDs))+`))
		ORDER BY pp.person_id, pp.participant_id`), args...)
	if err != nil {
		return nil, fmt.Errorf("read author persons: %w", err)
	}
	defer func() { _ = rows.Close() }()
	families := map[int64]AuthorIdentity{}
	var current AuthorIdentity
	for rows.Next() {
		var member int64
		var identity AuthorIdentity
		if err := rows.Scan(&member, &identity.PersonID, &identity.PersonName); err != nil {
			return nil, fmt.Errorf("scan author person: %w", err)
		}
		if identity.PersonID != current.PersonID {
			current = identity
			current.Key = member
		}
		families[member] = current
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read author persons: %w", err)
	}
	return families, nil
}

// authorFlagsContext marks each family that has a member in owners, or that
// the owner wrote to, and returns each family's identity by key. A message
// is the owner's when it is marked from me or an owner member sent it.
func (s *Store) authorFlagsContext(
	ctx context.Context, families map[int64]AuthorIdentity, owners []int64,
) (map[int64]AuthorIdentity, error) {
	members := slices.Sorted(maps.Keys(families))
	byKey := map[int64]AuthorIdentity{}
	for _, id := range members {
		byKey[families[id].Key] = families[id]
	}
	ownerSent := "m.is_from_me"
	if len(owners) > 0 {
		ownerSent = "(m.is_from_me OR " + messageSender("m") + " IN (" + directoryPlaceholders(len(owners)) + "))"
	}
	ownerWrote := func(where string) string {
		return `EXISTS (SELECT 1 FROM ` + where + `
		         AND ` + ownerSent + ` AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL)`
	}
	var args []any
	for _, ids := range [][]int64{owners, owners, members} {
		for _, id := range ids {
			args = append(args, id)
		}
	}
	flags, err := s.db.QueryContext(ctx, s.dialect.Rebind(`
		SELECT p.id,
		       `+ownerWrote(`message_recipients mr JOIN messages m ON m.id = mr.message_id
		         WHERE mr.participant_id = p.id AND LOWER(mr.recipient_type) IN ('to', 'cc', 'bcc')`)+`
		       OR `+ownerWrote(`conversation_participants cp JOIN messages m ON m.conversation_id = cp.conversation_id
		         WHERE cp.participant_id = p.id`)+`
		FROM participants p
		WHERE p.id IN (`+directoryPlaceholders(len(members))+`)`), args...)
	if err != nil {
		return nil, fmt.Errorf("read author identities: %w", err)
	}
	defer func() { _ = flags.Close() }()
	for flags.Next() {
		var id int64
		var addressed bool
		if err := flags.Scan(&id, &addressed); err != nil {
			return nil, fmt.Errorf("scan author identity: %w", err)
		}
		family := byKey[families[id].Key]
		family.Owner = family.Owner || slices.Contains(owners, id)
		family.Addressed = family.Addressed || addressed
		byKey[family.Key] = family
	}
	if err := flags.Err(); err != nil {
		return nil, fmt.Errorf("read author identities: %w", err)
	}
	return byKey, nil
}

// DeclaredValueLimit caps how many ask-me-about values one read returns.
const DeclaredValueLimit = 5000

// AskMeAboutValuesContext returns up to DeclaredValueLimit current
// ask-me-about values the user typed or an address-book import supplied, in
// person order, and whether more exist. Inferred and superseded values are
// left out, and so are people bound to one of the owner's account identities.
// The caller matches topics in Go, so matching is not limited to what SQL can
// case-fold.
func (s *Store) AskMeAboutValuesContext(ctx context.Context) ([]PersonAskMeAbout, bool, error) {
	query := `
		SELECT v.person_id, COALESCE(per.display_name, ''), v.value_text
		FROM person_attribute_values v
		JOIN attribute_definitions d ON d.id = v.definition_id
		JOIN persons per ON per.id = v.person_id
		WHERE d.universal_id = ? AND v.active_until IS NULL AND v.superseded_at IS NULL
		  AND v.source IN ('user', 'carddav_import', 'vcard_import')
		  AND NOT EXISTS (
		    SELECT 1 FROM person_participants bound
		      JOIN participants op ON op.id = bound.participant_id
		     WHERE bound.person_id = v.person_id AND ` + ownerParticipantMatch("op") + `)
		ORDER BY v.person_id, v.ordinal
		LIMIT ?`
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(query), AttributeUniversalIDAskMeAbout, DeclaredValueLimit+1)
	if err != nil {
		return nil, false, fmt.Errorf("read ask-me-about values: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []PersonAskMeAbout
	for rows.Next() {
		var v PersonAskMeAbout
		if err := rows.Scan(&v.PersonID, &v.PersonName, &v.Value); err != nil {
			return nil, false, fmt.Errorf("scan ask-me-about value: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("read ask-me-about values: %w", err)
	}
	truncated := len(out) > DeclaredValueLimit
	if truncated {
		out = out[:DeclaredValueLimit]
	}
	return out, truncated, nil
}
