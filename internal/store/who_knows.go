package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"net/mail"
	"slices"
	"strings"
	"time"
)

// TopicAuthor is one message with the contact that wrote it.
type TopicAuthor struct {
	MessageID int64
	// Counts is false for a message the owner sent, one with no sender, and
	// one an automated sender wrote. Contact is set only when Counts is true.
	Counts      bool
	ListMail    bool
	MessageType string
	SentAt      *time.Time
	Subject     string

	// SenderAuthenticated reports that the source sets the sender, rather
	// than the message header asserting it.
	SenderAuthenticated bool

	Contact TopicContact
}

// TopicContact is the identity family of a sender: every participant bound
// to its saved person, or its link cluster when it has none.
type TopicContact struct {
	// Key is the smallest participant ID in the family.
	Key int64
	// PersonID is zero when no member is bound to a saved person.
	PersonID int64
	// Name is the saved person's name, else the key participant's. Email is
	// the key participant's address.
	Name  string
	Email string
	// OneWay reports that no member is a to, cc, or bcc recipient of a
	// message the owner sent.
	OneWay bool
}

// PersonAskMeAbout is one current, user-declared ask-me-about value.
type PersonAskMeAbout struct {
	PersonID   int64
	PersonName string
	Value      string
}

// automatedPatterns are sender local parts that belong to systems rather than
// people. A local part matches when it equals a pattern or starts with one
// followed by "-", "+", or ".". Underscores count as hyphens.
var automatedPatterns = []string{
	"no-reply", "noreply", "do-not-reply", "donotreply", "notification",
	"notifications", "notify", "alert", "alerts", "newsletter", "newsletters",
	"digest", "mailer-daemon", "postmaster", "bounce", "bounces", "automated",
	"robot", "bot", "calendar-notification",
}

// automatedAddress reports whether an address looks like a system sender.
func automatedAddress(email string) bool {
	if a, err := mail.ParseAddress(email); err == nil {
		email = a.Address
	}
	local, _, _ := strings.Cut(strings.ToLower(email), "@")
	local, _, _ = strings.Cut(local, "+")
	local = strings.ReplaceAll(local, "_", "-")
	for _, pattern := range automatedPatterns {
		if local == pattern {
			return true
		}
		if strings.HasPrefix(local, pattern) && len(local) > len(pattern) &&
			strings.ContainsRune("-.", rune(local[len(pattern)])) {
			return true
		}
	}
	return false
}

// automatedParticipant is true when the producer marked participant alias p
// as automation: Slack namespaces bots as "<team>:bot:<id>" identifiers, and
// Chatwoot keeps bots and assistants under their own path segment.
func automatedParticipant(p string) string {
	return `EXISTS (
		SELECT 1 FROM participant_identifiers bi
		 WHERE bi.participant_id = ` + p + `.id AND (
		       (bi.identifier_type = 'slack' AND bi.identifier_value LIKE '%:bot:%')
		    OR (bi.identifier_type = 'chatwoot' AND (
		           bi.identifier_value LIKE '%/agent\_bot/%' ESCAPE '\'
		        OR bi.identifier_value LIKE '%/captain\_assistant/%' ESCAPE '\'))))`
}

// automatedMessage is true when the producer marked message m or its sender
// p as automation. Discord stores a flag in messages.metadata. Importers
// write no other marker.
func (s *Store) automatedMessage(m, p string) string {
	flag := "COALESCE(CASE WHEN json_valid(" + m + ".metadata) THEN json_extract(" + m + ".metadata, '$.automated') = 1 END, FALSE)"
	if s.IsPostgreSQL() {
		flag = "COALESCE(" + m + ".metadata ->> 'automated' = 'true', FALSE)"
	}
	return "(" + flag + " OR " + automatedParticipant(p) + ")"
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

// ownerFamilyCTE defines owner_family for a WITH RECURSIVE clause: the
// owner's account identities, their link clusters, and every participant
// bound to their persons. It binds no placeholders.
func ownerFamilyCTE() string {
	return `owner_cluster(id) AS (
		SELECT p.id FROM participants p WHERE ` + ownerParticipantMatch("p") + `
		UNION
		SELECT CASE WHEN l.participant_a = oc.id THEN l.participant_b ELSE l.participant_a END
		FROM owner_cluster oc JOIN participant_links l ON l.participant_a = oc.id OR l.participant_b = oc.id
	), owner_family(id) AS (
		SELECT id FROM owner_cluster
		UNION
		SELECT bound.participant_id FROM owner_cluster oc
		JOIN person_participants pp ON pp.participant_id = oc.id
		JOIN person_participants bound ON bound.person_id = pp.person_id
	)`
}

// inOwnerFamily is true when participant ID expression id is in owner_family.
func inOwnerFamily(id string) string {
	return "EXISTS (SELECT 1 FROM owner_family o WHERE o.id = " + id + ")"
}

// messageSender is the participant that sent message alias m: its
// sender_id, else its lowest 'from' recipient.
func messageSender(m string) string {
	return `COALESCE(` + m + `.sender_id, (
		       SELECT MIN(fr.participant_id) FROM message_recipients fr
		       WHERE fr.message_id = ` + m + `.id AND LOWER(fr.recipient_type) = 'from'
		         AND fr.participant_id IS NOT NULL))`
}

func int64Args(ids []int64) []any {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return args
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
	query := `WITH RECURSIVE ` + ownerFamilyCTE() + `
		SELECT m.id, COALESCE(m.is_from_me, FALSE), COALESCE(m.list_id, '') <> '',
		       ` + s.automatedMessage("m", "p") + `, ` + inOwnerFamily("p.id") + `,
		       m.message_type, m.sent_at, COALESCE(m.subject, ''), src.source_type,
		       p.id, COALESCE(p.email_address, '')
		FROM messages m
		JOIN sources src ON src.id = m.source_id
		LEFT JOIN participants p ON p.id = ` + messageSender("m") + `
		WHERE m.id IN (` + directoryPlaceholders(len(messageIDs)) + `)
		  AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL`
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(query), int64Args(messageIDs)...)
	if err != nil {
		return nil, fmt.Errorf("read topic authors: %w", err)
	}
	defer func() { _ = rows.Close() }()
	senders := map[int64]int64{}
	var slackIDs []int64
	for rows.Next() {
		var (
			a                             TopicAuthor
			fromMe, automated, ownerAlias bool
			sentAt                        sql.NullTime
			sender                        sql.NullInt64
			sourceType, email             string
		)
		if err := rows.Scan(&a.MessageID, &fromMe, &a.ListMail, &automated, &ownerAlias, &a.MessageType,
			&sentAt, &a.Subject, &sourceType, &sender, &email); err != nil {
			return nil, fmt.Errorf("scan topic author: %w", err)
		}
		if sentAt.Valid {
			a.SentAt = &sentAt.Time
		}
		a.SenderAuthenticated = personSweepSourceAuthenticatesSender(sourceType)
		a.Counts = !fromMe && sender.Valid && !automated && !ownerAlias && !automatedAddress(email)
		out[a.MessageID] = a
		if a.Counts {
			senders[a.MessageID] = sender.Int64
			if a.MessageType == slackMessageType {
				slackIDs = append(slackIDs, a.MessageID)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read topic authors: %w", err)
	}
	if err := s.dropSlackBotPayloads(ctx, out, senders, slackIDs); err != nil {
		return nil, err
	}
	contacts, err := s.topicContactsContext(ctx, senders)
	if err != nil {
		return nil, err
	}
	for id, sender := range senders {
		a := out[id]
		a.Contact = contacts[sender]
		out[id] = a
	}
	return out, nil
}

// slackMessageType is the message_type the Slack importer writes.
const slackMessageType = "slack"

// dropSlackBotPayloads stops counting Slack messages whose archived payload
// carries a bot_id or the bot_message subtype. The importer files a message
// carrying both "user" and "bot_id" under the user, so only the payload
// shows it. An unreadable payload leaves the message counted.
func (s *Store) dropSlackBotPayloads(ctx context.Context, authors map[int64]TopicAuthor, senders map[int64]int64, ids []int64) error {
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
			a.Counts = false
			authors[id] = a
			delete(senders, id)
		}
	}
	return nil
}

// topicContactsContext maps each sender to its identity family. Promotion,
// linking, and merging bind a whole link cluster to at most one person, so a
// saved sender's family is every participant bound to its person, and an
// unsaved sender's family is its link cluster. The cluster walk starts from
// the listed senders only.
func (s *Store) topicContactsContext(ctx context.Context, bySender map[int64]int64) (map[int64]TopicContact, error) {
	out := map[int64]TopicContact{}
	var senders []int64
	for _, id := range bySender {
		if _, seen := out[id]; !seen {
			out[id] = TopicContact{}
			senders = append(senders, id)
		}
	}
	if len(senders) == 0 {
		return out, nil
	}
	query := `WITH RECURSIVE ` + ownerFamilyCTE() + `,
		component(seed, id) AS (
			SELECT p.id, p.id FROM participants p WHERE p.id IN (` + directoryPlaceholders(len(senders)) + `)
			UNION
			SELECT c.seed, CASE WHEN l.participant_a = c.id THEN l.participant_b ELSE l.participant_a END
			FROM component c JOIN participant_links l ON l.participant_a = c.id OR l.participant_b = c.id
		), seed_person(seed, person_id) AS (
			SELECT c.seed, MIN(pp.person_id) FROM component c
			JOIN person_participants pp ON pp.participant_id = c.id
			GROUP BY c.seed
		), family(seed, id) AS (
			SELECT c.seed, c.id FROM component c
			WHERE NOT EXISTS (SELECT 1 FROM seed_person sp WHERE sp.seed = c.seed)
			UNION
			SELECT sp.seed, pp.participant_id FROM seed_person sp
			JOIN person_participants pp ON pp.person_id = sp.person_id
		), keyed(seed, family_key) AS (
			SELECT seed, MIN(id) FROM family GROUP BY seed
		)
		SELECT k.seed, k.family_key, COALESCE(sp.person_id, 0), COALESCE(per.display_name, ''),
		       COALESCE(kp.display_name, ''), COALESCE(kp.email_address, ''),
		       NOT EXISTS (
		         SELECT 1 FROM family f
		         JOIN message_recipients mr ON mr.participant_id = f.id
		         JOIN messages m ON m.id = mr.message_id
		         WHERE f.seed = k.seed AND LOWER(mr.recipient_type) IN ('to', 'cc', 'bcc')
		           AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL
		           AND (m.is_from_me OR ` + inOwnerFamily(messageSender("m")) + `))
		FROM keyed k
		JOIN participants kp ON kp.id = k.family_key
		LEFT JOIN seed_person sp ON sp.seed = k.seed
		LEFT JOIN persons per ON per.id = sp.person_id`
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(query), int64Args(senders)...)
	if err != nil {
		return nil, fmt.Errorf("read topic contacts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var seed int64
		var c TopicContact
		var personName string
		if err := rows.Scan(&seed, &c.Key, &c.PersonID, &personName, &c.Name, &c.Email, &c.OneWay); err != nil {
			return nil, fmt.Errorf("scan topic contact: %w", err)
		}
		if personName != "" {
			c.Name = personName
		}
		out[seed] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read topic contacts: %w", err)
	}
	return out, nil
}

// DeclaredValueLimit caps how many ask-me-about values one read returns.
const DeclaredValueLimit = 5000

// AskMeAboutValuesContext returns up to DeclaredValueLimit current
// ask-me-about values the user typed or an address-book import supplied, in
// person order, and whether more exist. Inferred and superseded values are
// left out, and so are people bound to an owner_family participant or to an
// automated participant. The caller matches topics in Go, so matching is not
// limited to what SQL can case-fold.
func (s *Store) AskMeAboutValuesContext(ctx context.Context) ([]PersonAskMeAbout, bool, error) {
	query := `WITH RECURSIVE ` + ownerFamilyCTE() + `
		SELECT v.person_id, COALESCE(per.display_name, ''), v.value_text
		FROM person_attribute_values v
		JOIN attribute_definitions d ON d.id = v.definition_id
		JOIN persons per ON per.id = v.person_id
		WHERE d.universal_id = ? AND v.active_until IS NULL AND v.superseded_at IS NULL
		  AND v.source IN ('user', 'carddav_import', 'vcard_import')
		  AND NOT EXISTS (
		    SELECT 1 FROM person_participants bound
		      JOIN participants bp ON bp.id = bound.participant_id
		     WHERE bound.person_id = v.person_id
		       AND (` + inOwnerFamily("bp.id") + ` OR ` + automatedParticipant("bp") + `))
		ORDER BY v.person_id, v.ordinal, v.id
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
	automated, err := s.personsWithAutomatedAddressContext(ctx, out)
	if err != nil {
		return nil, false, err
	}
	out = slices.DeleteFunc(out, func(v PersonAskMeAbout) bool { return automated[v.PersonID] })
	return out, truncated, nil
}

// personsWithAutomatedAddressContext reports which persons of values, which
// are in person order, are bound to a participant whose address looks like a
// system sender.
func (s *Store) personsWithAutomatedAddressContext(ctx context.Context, values []PersonAskMeAbout) (map[int64]bool, error) {
	out := map[int64]bool{}
	var persons []int64
	for _, v := range values {
		if len(persons) == 0 || persons[len(persons)-1] != v.PersonID {
			persons = append(persons, v.PersonID)
		}
	}
	if len(persons) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(`
		SELECT pp.person_id, COALESCE(p.email_address, '')
		FROM person_participants pp JOIN participants p ON p.id = pp.participant_id
		WHERE pp.person_id IN (`+directoryPlaceholders(len(persons))+`)`), int64Args(persons)...)
	if err != nil {
		return nil, fmt.Errorf("read declared person addresses: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var person int64
		var email string
		if err := rows.Scan(&person, &email); err != nil {
			return nil, fmt.Errorf("scan declared person address: %w", err)
		}
		out[person] = out[person] || automatedAddress(email)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read declared person addresses: %w", err)
	}
	return out, nil
}
