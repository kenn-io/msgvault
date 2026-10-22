package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type PersonDeclaredTopic struct {
	PersonID int64
	Value    string
}

// PeopleDeclaredTopics returns one overflow sentinel after the 20,000-value bound.
func (s *Store) PeopleDeclaredTopics(ctx context.Context) ([]PersonDeclaredTopic, error) {
	query := `
		SELECT v.person_id,v.value_text
		FROM person_attribute_values v
		JOIN attribute_definitions d ON d.id=v.definition_id
		WHERE d.universal_id=? AND v.active_until IS NULL AND v.superseded_at IS NULL
		  AND v.source IN ('user','carddav_import','vcard_import')
		ORDER BY v.person_id,v.ordinal LIMIT 20001`
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(query), AttributeUniversalIDAskMeAbout)
	if err != nil {
		return nil, fmt.Errorf("read declared topics: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []PersonDeclaredTopic{}
	for rows.Next() {
		var v PersonDeclaredTopic
		if err := rows.Scan(&v.PersonID, &v.Value); err != nil {
			return nil, fmt.Errorf("scan declared topic: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

type PeopleProfile struct {
	ParticipantIDs                                     []int64
	ParticipantEmails                                  map[int64]string
	PersonID                                           int64
	VCardUID, DisplayName                              string
	IsOwner                                            bool
	ContactPresent                                     bool
	ContactStateFresh                                  bool
	LastContactAt, LastOutboundAt, DirtyAt, ComputedAt *time.Time
	InteractionCount                                   int64
}
type PeopleEmployment struct {
	PersonID, OrganizationID                   int64
	Name, NameNormalized, PrimaryDomain, Title string
}

func peopleIDArgs(ids []int64) (string, []any, error) {
	if len(ids) > 500 {
		return "", nil, errors.New("people profiles: at most 500 IDs")
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		if id <= 0 {
			return "", nil, errors.New("people profiles: IDs must be positive")
		}
		args[i] = id
	}
	return directoryPlaceholders(len(ids)), args, nil
}

// PeopleProfiles reads candidate identity and contact state in a bounded batch.
func (s *Store) PeopleProfiles(ctx context.Context, ids []int64) (map[int64]PeopleProfile, error) {
	out := map[int64]PeopleProfile{}
	if len(ids) == 0 {
		return out, nil
	}
	ph, args, err := peopleIDArgs(ids)
	if err != nil {
		return nil, err
	}
	participantMatch := "((" + ownerEmailMatch("part.email_address", "ai.address") + ") OR EXISTS (SELECT 1 FROM participant_identifiers pi WHERE pi.participant_id=part.id AND " + ownerIdentifierMatch("pi.identifier_type", "pi.identifier_value", "ai.address", " AND (part.email_address IS NULL OR TRIM(part.email_address) = '')") + "))"
	populatedFrom := `EXISTS (SELECT 1 FROM message_recipients env WHERE env.message_id=m.id AND LOWER(env.recipient_type)='from' AND env.email_address IS NOT NULL AND TRIM(env.email_address)<>'')`
	nativeFromOwner := `(LOWER(mr.recipient_type)='from' AND m.source_is_from_me=TRUE AND ((m.sender_id IS NOT NULL AND mr.participant_id=m.sender_id) OR (m.sender_id IS NULL AND (SELECT COUNT(DISTINCT native_from.participant_id) FROM message_recipients native_from WHERE native_from.message_id=m.id AND LOWER(native_from.recipient_type)='from' AND native_from.participant_id IS NOT NULL)=1)))`
	identityOwner := `EXISTS (SELECT 1 FROM account_identities ai WHERE ai.source_id=m.source_id AND ((` + ownerEmailMatch("mr.email_address", "ai.address") + `) OR ((mr.email_address IS NULL OR TRIM(mr.email_address)='') AND (LOWER(mr.recipient_type)<>'from' OR NOT ` + populatedFrom + `) AND ` + participantMatch + `)))`
	// Resolve ownership only through a bound participant's source-associated edges.
	// Explicit envelope addresses override participant aliases, as in attribution.
	owner := `EXISTS (SELECT 1 FROM person_participants binding JOIN participants part ON part.id=binding.participant_id WHERE binding.person_id=p.id AND (
 EXISTS (SELECT 1 FROM messages m WHERE m.sender_id=part.id AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL AND (
 m.source_is_from_me=TRUE OR EXISTS (SELECT 1 FROM account_identities ai WHERE ai.source_id=m.source_id AND (
 EXISTS (SELECT 1 FROM message_recipients mr WHERE mr.message_id=m.id AND mr.participant_id=part.id AND LOWER(mr.recipient_type)='from' AND ` + ownerEmailMatch("mr.email_address", "ai.address") + `)
 OR (NOT ` + populatedFrom + ` AND ` + participantMatch + `)))))
 OR EXISTS (SELECT 1 FROM message_recipients mr JOIN messages m ON m.id=mr.message_id WHERE mr.participant_id=part.id AND LOWER(mr.recipient_type) IN ('from','to','cc','bcc') AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL
 AND (LOWER(mr.recipient_type)<>'from' OR NOT COALESCE(m.source_is_from_me,FALSE) OR ` + nativeFromOwner + ` OR (m.sender_id IS NULL AND ` + identityOwner + `))
 AND (` + nativeFromOwner + ` OR ` + identityOwner + `))
 OR EXISTS (SELECT 1 FROM conversation_participants cp JOIN conversations c ON c.id=cp.conversation_id WHERE cp.participant_id=part.id AND c.conversation_type IN ('direct_chat','group_chat','channel','meeting') AND EXISTS (
 SELECT 1 FROM account_identities ai WHERE ai.source_id=c.source_id AND ` + participantMatch + `) AND EXISTS (
 SELECT 1 FROM messages m WHERE m.conversation_id=c.id AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL
 AND NOT EXISTS (SELECT 1 FROM message_recipients mr WHERE mr.message_id=m.id AND mr.participant_id=part.id AND LOWER(mr.recipient_type) IN ('from','to','cc','bcc'))
 AND NOT EXISTS (SELECT 1 FROM messages native_message WHERE native_message.conversation_id=c.id AND native_message.deleted_at IS NULL AND native_message.deleted_from_source_at IS NULL AND native_message.source_is_from_me=TRUE AND (
 native_message.sender_id IS NOT NULL AND native_message.sender_id<>part.id
 OR native_message.sender_id IS NULL AND EXISTS (SELECT 1 FROM message_recipients native_from WHERE native_from.message_id=native_message.id AND LOWER(native_from.recipient_type)='from' AND native_from.participant_id IS NOT NULL AND native_from.participant_id<>part.id)))))
 ))`
	q := `SELECT p.id,p.vcard_uid,COALESCE(p.display_name,''),` + owner + `,
	 pcs.person_id,pcs.last_contact_at,pcs.last_outbound_at,pcs.dirty_at,pcs.computed_at,COALESCE(pcs.interaction_count,0),pcs.identity_revision,pcs.account_identity_revision,
	 members.participant_id,NULLIF(TRIM(member_participants.email_address),'')
 FROM persons p LEFT JOIN person_contact_state pcs ON pcs.person_id=p.id LEFT JOIN person_participants members ON members.person_id=p.id
 LEFT JOIN participants member_participants ON member_participants.id=members.participant_id
 WHERE p.id IN (` + ph + `) ORDER BY p.id, members.participant_id`
	err = s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		currentRevisions, globalStale, err := s.contactStateFreshnessTxContext(ctx, tx)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, s.dialect.Rebind(q), args...)
		if err != nil {
			return fmt.Errorf("read people profiles: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var p PeopleProfile
			var contactID, participantID *int64
			var identityRevision, accountIdentityRevision sql.NullInt64
			var participantEmail sql.NullString
			if err := rows.Scan(
				&p.PersonID, &p.VCardUID, &p.DisplayName, &p.IsOwner,
				&contactID, &p.LastContactAt, &p.LastOutboundAt, &p.DirtyAt,
				&p.ComputedAt, &p.InteractionCount, &identityRevision,
				&accountIdentityRevision, &participantID, &participantEmail,
			); err != nil {
				return fmt.Errorf("scan people profile: %w", err)
			}
			p.ContactPresent = contactID != nil
			p.ContactStateFresh = p.ContactPresent && p.DirtyAt == nil &&
				identityRevision.Valid && accountIdentityRevision.Valid &&
				identityRevision.Int64 == currentRevisions.IdentityRevision &&
				accountIdentityRevision.Int64 == currentRevisions.AccountIdentityRevision &&
				!globalStale
			if existing, ok := out[p.PersonID]; ok {
				p.ParticipantIDs = existing.ParticipantIDs
				p.ParticipantEmails = existing.ParticipantEmails
			} else {
				p.ParticipantIDs = []int64{}
				p.ParticipantEmails = map[int64]string{}
			}
			if participantID != nil {
				p.ParticipantIDs = append(p.ParticipantIDs, *participantID)
				if participantEmail.Valid {
					p.ParticipantEmails[*participantID] = participantEmail.String
				}
			}
			out[p.PersonID] = p
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
func (s *Store) PeopleEmployments(ctx context.Context, ids []int64) ([]PeopleEmployment, error) {
	out := []PeopleEmployment{}
	if len(ids) == 0 {
		return out, nil
	}
	ph, args, err := peopleIDArgs(ids)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(`SELECT e.person_id,o.id,o.name,o.name_normalized,COALESCE(o.primary_domain,''),COALESCE(e.title,'') FROM employments e JOIN organizations o ON o.id=e.organization_id WHERE e.person_id IN (`+ph+`) AND e.is_current=TRUE AND o.merged_into_id IS NULL AND o.retired_at IS NULL ORDER BY e.person_id,o.id,e.id`), args...)
	if err != nil {
		return nil, fmt.Errorf("read people employments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var e PeopleEmployment
		if err := rows.Scan(&e.PersonID, &e.OrganizationID, &e.Name, &e.NameNormalized, &e.PrimaryDomain, &e.Title); err != nil {
			return nil, fmt.Errorf("scan people employment: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
