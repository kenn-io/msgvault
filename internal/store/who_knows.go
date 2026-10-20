package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"maps"
	"net/mail"
	"slices"
	"strconv"
	"strings"
	"time"
)

// TopicAuthor is one message with the contact that wrote it.
type TopicAuthor struct {
	MessageID int64
	// Counts is false for a message the owner sent, one with no sender, one
	// an automated sender wrote, and a Discord system message. Contact is set
	// only when Counts is true.
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
	// A dot often separates a person's name, as in alert.sam, so only a
	// hyphen marks a system sender's suffix.
	for _, pattern := range automatedPatterns {
		if local == pattern || strings.HasPrefix(local, pattern+"-") {
			return true
		}
	}
	return false
}

// automatedParticipant is true when the producer marked participant alias p
// as automation: Slack namespaces bots as "<team>:bot:<id>" identifiers,
// Chatwoot keeps bots and assistants under their own path segment, and
// Discord files webhooks and applications under their own identifier types.
func automatedParticipant(p string) string {
	return `EXISTS (
		SELECT 1 FROM participant_identifiers bi
		 WHERE bi.participant_id = ` + p + `.id AND (
		       bi.identifier_type IN ('discord_webhook_id', 'discord_application_id', 'discord_automated_id')
		    OR (bi.identifier_type = 'slack' AND bi.identifier_value LIKE '%:bot:%')
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

// OwnerFamilyContext returns the archive owner's participants, in ID order:
// those matching an account identity by address or by identifier, as the
// activity projector decides it, their link clusters, and every participant
// bound to their persons. Each who-knows read takes this set, so a request
// reads it once.
func (s *Store) OwnerFamilyContext(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `WITH RECURSIVE owner_cluster(id) AS (
		SELECT p.id FROM account_identities ai
		JOIN participants p ON `+ownerEmailMatch("p.email_address", "ai.address")+`
		UNION
		SELECT pi.participant_id FROM account_identities ai
		JOIN participant_identifiers pi
		  ON `+ownerIdentifierMatch("pi.identifier_type", "pi.identifier_value", "ai.address", identifierWithoutPrimaryEmail)+`
		UNION
		SELECT CASE WHEN l.participant_a = oc.id THEN l.participant_b ELSE l.participant_a END
		FROM owner_cluster oc JOIN participant_links l ON l.participant_a = oc.id OR l.participant_b = oc.id
	), owner_family(id) AS (
		SELECT id FROM owner_cluster
		UNION
		SELECT bound.participant_id FROM owner_cluster oc
		JOIN person_participants pp ON pp.participant_id = oc.id
		JOIN person_participants bound ON bound.person_id = pp.person_id
	)
	SELECT id FROM owner_family ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("read owner participants: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan owner participant: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read owner participants: %w", err)
	}
	return out, nil
}

// inOwnerFamily is true when participant ID expression id is one of count
// placeholders bound to owner family IDs.
func inOwnerFamily(id string, count int) string {
	if count == 0 {
		return "FALSE"
	}
	return id + " IN (" + directoryPlaceholders(count) + ")"
}

// ownerParticipant recognizes an account identity or a participant that sent
// a message marked from me. The latter probes messages by sender_id.
func ownerParticipant(id string, count int) string {
	return "(" + inOwnerFamily(id, count) + ` OR EXISTS (
		SELECT 1 FROM messages om WHERE om.sender_id = ` + id + ` AND om.is_from_me))`
}

// messageSender is the participant that sent message alias m: its
// sender_id, else its lowest 'from' recipient.
func messageSender(m string) string {
	return `COALESCE(` + m + `.sender_id, (
		       SELECT MIN(fr.participant_id) FROM message_recipients fr
		       WHERE fr.message_id = ` + m + `.id AND LOWER(fr.recipient_type) = 'from'
		         AND fr.participant_id IS NOT NULL))`
}

// discordSystemMessage is true when message alias m is a Discord message
// whose type is a system event rather than one a member wrote.
func (s *Store) discordSystemMessage(m string) string {
	kind := "CASE WHEN json_valid(" + m + ".metadata) THEN json_extract(" + m + ".metadata, '$.discord_message_type') END"
	if s.IsPostgreSQL() {
		kind = "CAST(" + m + ".metadata ->> 'discord_message_type' AS BIGINT)"
	}
	authored := make([]string, len(DiscordAuthoredMessageTypes))
	for i, t := range DiscordAuthoredMessageTypes {
		authored[i] = strconv.Itoa(t)
	}
	return "COALESCE(CASE WHEN " + m + ".message_type = '" + discordMessageType + "' THEN " +
		kind + " NOT IN (" + strings.Join(authored, ", ") + ") END, FALSE)"
}

// DiscordAuthoredMessageTypes are the Discord message types a member writes:
// default, reply, slash command, and context menu command. Every other type
// is a system event, such as a channel rename or a thread start.
var DiscordAuthoredMessageTypes = []int{0, 19, 20, 23}

// TopicAuthorsContext reads the sender of each message at request time, so
// mail synced since the last activity build still counts. A message's sender
// is messages.sender_id, else its lowest 'from' recipient. A family is the
// owner's when any member is in owner, the OwnerFamilyContext set, or sent a
// message marked from me. Deleted messages are left out of the result.
func (s *Store) TopicAuthorsContext(ctx context.Context, messageIDs, owner []int64) (map[int64]TopicAuthor, error) {
	out := map[int64]TopicAuthor{}
	if len(messageIDs) == 0 {
		return out, nil
	}
	query := `SELECT m.id, COALESCE(m.is_from_me, FALSE), COALESCE(m.list_id, '') <> '',
		       ` + s.automatedMessage("m", "p") + `, ` + s.discordSystemMessage("m") + `,
		       COALESCE(m.message_type, ''), m.sent_at, COALESCE(m.subject, ''), src.source_type,
		       p.id, COALESCE(p.email_address, '')
		FROM messages m
		JOIN sources src ON src.id = m.source_id
		LEFT JOIN participants p ON p.id = ` + messageSender("m") + `
		WHERE m.id IN (` + directoryPlaceholders(len(messageIDs)) + `)
		  AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL`
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(query), appendInt64Args(nil, messageIDs)...)
	if err != nil {
		return nil, fmt.Errorf("read topic authors: %w", err)
	}
	defer func() { _ = rows.Close() }()
	senders := map[int64]int64{}
	var slackIDs []int64
	for rows.Next() {
		var (
			a                         TopicAuthor
			fromMe, automated, system bool
			sentAt                    sql.NullTime
			sender                    sql.NullInt64
			sourceType, email         string
		)
		if err := rows.Scan(&a.MessageID, &fromMe, &a.ListMail, &automated, &system, &a.MessageType,
			&sentAt, &a.Subject, &sourceType, &sender, &email); err != nil {
			return nil, fmt.Errorf("scan topic author: %w", err)
		}
		if sentAt.Valid {
			a.SentAt = &sentAt.Time
		}
		a.SenderAuthenticated = personSweepSourceAuthenticatesSender(sourceType)
		a.Counts = !fromMe && sender.Valid && !automated && !system &&
			!slices.Contains(owner, sender.Int64) && !automatedAddress(email)
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
	if err := s.dropSlackNonAuthoredPayloads(ctx, out, senders, slackIDs); err != nil {
		return nil, err
	}
	contacts, err := s.topicContactsContext(ctx, senders, owner)
	if err != nil {
		return nil, err
	}
	for id, sender := range senders {
		a := out[id]
		a.Contact, a.Counts = contacts[sender]
		out[id] = a
	}
	return out, nil
}

// slackMessageType and discordMessageType are the message_type values the
// Slack and Discord importers write.
const (
	slackMessageType   = "slack"
	discordMessageType = "discord"
)

// dropSlackNonAuthoredPayloads stops counting Slack bot messages and generated
// events. The importer files these under the user when the payload carries a
// "user" field, so only the payload shows it. An unreadable payload leaves
// the message counted.
func (s *Store) dropSlackNonAuthoredPayloads(ctx context.Context, authors map[int64]TopicAuthor, senders map[int64]int64, ids []int64) error {
	excluded, err := s.slackNonAuthoredPayloadsContext(ctx, ids)
	if err != nil {
		return err
	}
	for id := range excluded {
		a := authors[id]
		a.Counts = false
		authors[id] = a
		delete(senders, id)
	}
	return nil
}

// slackNonAuthoredPayloadsContext returns which Slack messages of ids have an
// archived bot or generated-event payload. Unreadable payloads are left out.
func (s *Store) slackNonAuthoredPayloadsContext(ctx context.Context, ids []int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	raws, err := s.GetMessageRawsContext(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("read Slack payloads: %w", err)
	}
	for id, raw := range raws {
		var payload struct {
			BotID   string `json:"bot_id"`
			Subtype string `json:"subtype"`
		}
		if json.Unmarshal(raw, &payload) != nil {
			continue
		}
		if payload.BotID != "" {
			out[id] = true
			continue
		}
		switch payload.Subtype {
		case "", "thread_broadcast", "file_share", "me_message":
			// These subtypes carry messages the user authored.
		default:
			out[id] = true
		}
	}
	return out, nil
}

// topicContactsContext maps each sender to its identity family. Promotion,
// linking, and merging bind a whole link cluster to at most one person, so a
// saved sender's family is every participant bound to its person, and an
// unsaved sender's family is its link cluster. The cluster walk starts from
// the listed senders only. Owner families are left out. The owner wrote a
// message when it is marked from me or its sender is in owner.
func (s *Store) topicContactsContext(ctx context.Context, bySender map[int64]int64, owner []int64) (map[int64]TopicContact, error) {
	out := map[int64]TopicContact{}
	senders := slices.Compact(slices.Sorted(maps.Values(bySender)))
	if len(senders) == 0 {
		return out, nil
	}
	query := `WITH RECURSIVE component(seed, id) AS (
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
		           AND (m.is_from_me OR ` + inOwnerFamily(messageSender("m"), len(owner)) + `))
		FROM keyed k
		JOIN participants kp ON kp.id = k.family_key
		LEFT JOIN seed_person sp ON sp.seed = k.seed
		LEFT JOIN persons per ON per.id = sp.person_id
		WHERE NOT EXISTS (
		  SELECT 1 FROM family f WHERE f.seed = k.seed AND ` + ownerParticipant("f.id", len(owner)) + `)`
	args := appendInt64Args(appendInt64Args(appendInt64Args(nil, senders), owner), owner)
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(query), args...)
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
// left out, and so are people bound to a participant in owner, the
// OwnerFamilyContext set, to a participant that sent a message marked from
// me, which finds the owner in an archive without account identities, or to
// an automated participant. The caller matches topics in Go, so matching is
// not limited to what SQL can case-fold.
func (s *Store) AskMeAboutValuesContext(ctx context.Context, owner []int64) ([]PersonAskMeAbout, bool, error) {
	args := []any{AttributeUniversalIDAskMeAbout}
	var declared []string
	for _, p := range AllProvenances {
		if p.IsDeclared() {
			declared = append(declared, "?")
			args = append(args, string(p))
		}
	}
	query := `SELECT v.person_id, COALESCE(per.display_name, ''), v.value_text
		FROM person_attribute_values v
		JOIN attribute_definitions d ON d.id = v.definition_id
		JOIN persons per ON per.id = v.person_id
		WHERE d.universal_id = ? AND v.active_until IS NULL AND v.superseded_at IS NULL
		  AND v.source IN (` + strings.Join(declared, ", ") + `)
		  AND NOT EXISTS (
		    SELECT 1 FROM person_participants bound
		      JOIN participants bp ON bp.id = bound.participant_id
		     WHERE bound.person_id = v.person_id
		       AND (` + ownerParticipant("bp.id", len(owner)) + ` OR ` + automatedParticipant("bp") + `))
		ORDER BY v.person_id, v.ordinal, v.id
		LIMIT ?`
	args = append(appendInt64Args(args, owner), DeclaredValueLimit+1)
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(query), args...)
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

// distinctPersons returns the person IDs of values, which are in person
// order, once each.
func distinctPersons(values []PersonAskMeAbout) []int64 {
	var persons []int64
	for _, v := range values {
		if len(persons) == 0 || persons[len(persons)-1] != v.PersonID {
			persons = append(persons, v.PersonID)
		}
	}
	return persons
}

// personsWithAutomatedAddressContext reports which persons of values, which
// are in person order, are bound to a participant whose address looks like a
// system sender.
func (s *Store) personsWithAutomatedAddressContext(ctx context.Context, values []PersonAskMeAbout) (map[int64]bool, error) {
	out := map[int64]bool{}
	persons := distinctPersons(values)
	if len(persons) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(`
		SELECT pp.person_id, COALESCE(p.email_address, '')
		FROM person_participants pp JOIN participants p ON p.id = pp.participant_id
		WHERE pp.person_id IN (`+directoryPlaceholders(len(persons))+`)`), appendInt64Args(nil, persons)...)
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
