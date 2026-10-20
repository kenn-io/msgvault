package store

import (
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

// AuthorIdentity is the identity family of a sender: its link cluster plus
// every participant bound to the same saved person. Senders whose families
// share a member share one identity.
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
		LEFT JOIN participants p ON p.id = COALESCE(m.sender_id, (
		       SELECT MIN(mr.participant_id) FROM message_recipients mr
		       WHERE mr.message_id = m.id AND LOWER(mr.recipient_type) = 'from'
		         AND mr.participant_id IS NOT NULL))
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

// AuthorIdentitiesContext groups senders into identity families. A family is
// a sender's link cluster plus every participant bound to the saved person of
// a member, and senders that share a member share a family. Each sender maps
// to its family's identity.
func (s *Store) AuthorIdentitiesContext(ctx context.Context, senderIDs []int64) (map[int64]AuthorIdentity, error) {
	out := map[int64]AuthorIdentity{}
	if len(senderIDs) == 0 {
		return out, nil
	}
	clusters, err := s.ParticipantClustersContext(ctx)
	if err != nil {
		return nil, err
	}
	clusterMembers := map[int64][]int64{}
	for member, root := range clusters {
		clusterMembers[root] = append(clusterMembers[root], member)
	}
	// Union-find whose root is always the smallest member, so it is the key.
	parent := map[int64]int64{}
	var find func(int64) int64
	find = func(id int64) int64 {
		p, ok := parent[id]
		if !ok {
			parent[id] = id
			return id
		}
		if p == id {
			return id
		}
		root := find(p)
		parent[id] = root
		return root
	}
	union := func(a, b int64) {
		ra, rb := find(a), find(b)
		parent[max(ra, rb)] = min(ra, rb)
	}
	for _, id := range senderIDs {
		find(id)
		if root, ok := clusters[id]; ok {
			for _, member := range clusterMembers[root] {
				union(id, member)
			}
		}
	}

	members := slices.Sorted(maps.Keys(parent))
	args := make([]any, len(members))
	for i, id := range members {
		args[i] = id
	}
	type boundPerson struct {
		id   int64
		name string
	}
	persons := map[int64]boundPerson{} // family member to its saved person
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(`
		SELECT mine.participant_id, sibling.participant_id, mine.person_id, COALESCE(per.display_name, '')
		FROM person_participants mine
		JOIN person_participants sibling ON sibling.person_id = mine.person_id
		JOIN persons per ON per.id = mine.person_id
		WHERE mine.participant_id IN (`+directoryPlaceholders(len(members))+`)`), args...)
	if err != nil {
		return nil, fmt.Errorf("read author persons: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var mine, sibling int64
		var person boundPerson
		if err := rows.Scan(&mine, &sibling, &person.id, &person.name); err != nil {
			return nil, fmt.Errorf("scan author person: %w", err)
		}
		union(mine, sibling)
		persons[sibling] = person
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read author persons: %w", err)
	}

	members = slices.Sorted(maps.Keys(parent))
	args = make([]any, len(members))
	for i, id := range members {
		args[i] = id
	}
	ownerWrote := func(where string) string {
		return `EXISTS (SELECT 1 FROM ` + where + `
		         AND m.is_from_me AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL)`
	}
	families := map[int64]AuthorIdentity{}
	flags, err := s.db.QueryContext(ctx, s.dialect.Rebind(`
		SELECT p.id, `+ownerParticipantMatch("p")+`,
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
		var owner, addressed bool
		if err := flags.Scan(&id, &owner, &addressed); err != nil {
			return nil, fmt.Errorf("scan author identity: %w", err)
		}
		key := find(id)
		family := families[key]
		family.Key = key
		family.Owner = family.Owner || owner
		family.Addressed = family.Addressed || addressed
		families[key] = family
	}
	if err := flags.Err(); err != nil {
		return nil, fmt.Errorf("read author identities: %w", err)
	}
	// Members are sorted, so a family that spans two people keeps the one
	// bound to its smallest member.
	for _, id := range members {
		family := families[find(id)]
		if person, ok := persons[id]; ok && family.PersonID == 0 {
			family.PersonID, family.PersonName = person.id, person.name
			families[find(id)] = family
		}
	}
	for _, id := range senderIDs {
		family := families[find(id)]
		family.Key = find(id)
		out[id] = family
	}
	return out, nil
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
