package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"go.kenn.io/msgvault/internal/personscope"
	"slices"
	"strings"
	"time"
)

const MaxRosterEdges = 64

type RoleEdge struct {
	ParticipantID             int64
	PersonID                  *int64
	Role                      personscope.Role
	EmailAddress, DisplayName string
	IsOwner                   bool
}
type MessageRoles struct {
	MessageID                     int64
	SourceMessageID               string
	SourceID                      int64
	SourceType                    string
	ConversationID                int64
	ConversationType, MessageType string
	SentAt                        *time.Time
	IsFromMe                      bool
	ListID, Subject, Snippet      string
	Edges                         []RoleEdge
}
type PeopleRoles struct {
	Messages            map[int64]MessageRoles
	LargeRoomMessageIDs []int64
}

// PeopleRolesForMessages reads metadata and at most 64 role edges per message.
// Large rooms contribute explicit senders/recipients, but no inferred roster.
func (s *Store) PeopleRolesForMessages(ctx context.Context, ids []int64) (PeopleRoles, error) {
	return s.PeopleRolesForMessagesWithRosterLimit(ctx, ids, MaxRosterEdges)
}

// PeopleRolesForMessagesWithRosterLimit reads roles using a caller-selected
// edge limit no greater than MaxRosterEdges. The default method preserves the
// full supported bound.
func (s *Store) PeopleRolesForMessagesWithRosterLimit(ctx context.Context, ids []int64, maxRosterEdges int) (PeopleRoles, error) {
	result := PeopleRoles{Messages: map[int64]MessageRoles{}, LargeRoomMessageIDs: []int64{}}
	if maxRosterEdges < 1 || maxRosterEdges > MaxRosterEdges {
		return result, fmt.Errorf("people roles: roster edge limit must be between 1 and %d", MaxRosterEdges)
	}
	if len(ids) > 1000 {
		return result, errors.New("people roles: at most 1000 message IDs")
	}
	if len(ids) == 0 {
		return result, nil
	}
	ids = slices.Clone(ids)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if ids[0] <= 0 {
		return result, errors.New("people roles: message IDs must be positive")
	}
	values := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		values[i] = "(CAST(? AS BIGINT))"
		args[i] = id
	}
	ownerMatch := `EXISTS (SELECT 1 FROM account_identities ai WHERE ai.source_id = m.source_id AND (
 (` + ownerEmailMatch("e.envelope_address", "ai.address") + `) OR (
 (e.envelope_address IS NULL OR TRIM(e.envelope_address) = '')
 AND (e.role <> 'from' OR NOT EXISTS (SELECT 1 FROM message_recipients env WHERE env.message_id=m.id AND LOWER(env.recipient_type)='from' AND env.email_address IS NOT NULL AND TRIM(env.email_address)<>''))
 AND ((` + ownerEmailMatch("p.email_address", "ai.address") + `) OR EXISTS (
 SELECT 1 FROM participant_identifiers pi WHERE pi.participant_id=p.id AND ` + ownerIdentifierMatch("pi.identifier_type", "pi.identifier_value", "ai.address", " AND (p.email_address IS NULL OR TRIM(p.email_address) = '')") + `)))))`
	q := `WITH selected(id) AS (VALUES ` + strings.Join(values, ",") + `),
 selected_messages AS (
 SELECT m.*, COALESCE(c.conversation_type,'') AS conversation_type,
 (SELECT COUNT(*) FROM (SELECT 1 FROM conversation_participants cp WHERE cp.conversation_id=m.conversation_id LIMIT %d) bounded_roster) AS roster_count
 FROM selected JOIN messages m ON m.id=selected.id JOIN conversations c ON c.id=m.conversation_id
 WHERE m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL
 ), direct_edges AS (
 SELECT mr.message_id,mr.participant_id,LOWER(mr.recipient_type) AS role, NULLIF(TRIM(mr.email_address),'') AS envelope_address
 FROM selected_messages m JOIN message_recipients mr ON mr.message_id=m.id
 WHERE LOWER(mr.recipient_type) IN ('from','to','cc','bcc')
 UNION ALL
 SELECT m.id,m.sender_id,'from',
 (SELECT MIN(NULLIF(TRIM(mr.email_address),'')) FROM message_recipients mr WHERE mr.message_id=m.id AND mr.participant_id=m.sender_id AND LOWER(mr.recipient_type)='from')
 FROM selected_messages m WHERE m.sender_id IS NOT NULL
 ), raw_edges AS (
 SELECT * FROM direct_edges
 UNION ALL
 SELECT m.id,cp.participant_id,CASE WHEN LOWER(m.conversation_type)='direct_chat' THEN 'to' ELSE 'conversation_member' END,CAST(NULL AS TEXT)
 FROM selected_messages m JOIN conversation_participants cp ON cp.conversation_id=m.conversation_id
 WHERE m.roster_count <= %d
 AND (LOWER(m.conversation_type) IN ('group_chat','channel','meeting') OR
 (LOWER(m.conversation_type)='direct_chat' AND (m.is_from_me=TRUE OR m.sender_id IS NOT NULL OR EXISTS (SELECT 1 FROM direct_edges d WHERE d.message_id=m.id AND d.role='from'))))
	 AND NOT EXISTS (SELECT 1 FROM direct_edges d WHERE d.message_id=m.id AND d.participant_id=cp.participant_id)
 ), unique_edges AS (
 SELECT e.message_id,e.participant_id,e.role,MAX(e.envelope_address) AS envelope_address,MAX(CASE WHEN ` + ownerMatch + ` THEN 1 ELSE 0 END) AS identity_owner
 FROM raw_edges e JOIN selected_messages m ON m.id=e.message_id JOIN participants p ON p.id=e.participant_id GROUP BY e.message_id,e.participant_id,e.role
 ), bounded_edges AS (
 SELECT e.*,ROW_NUMBER() OVER (PARTITION BY message_id ORDER BY CASE role WHEN 'from' THEN 0 WHEN 'to' THEN 1 ELSE 2 END,participant_id,role) AS edge_number FROM unique_edges e
 )
 SELECT m.id,COALESCE(m.source_message_id,''),m.source_id,s.source_type,m.conversation_id,m.conversation_type,COALESCE(NULLIF(m.message_type,''),'email'),
	 m.sent_at,m.received_at,m.internal_date,(COALESCE(m.is_from_me,FALSE) OR m.identity_is_from_me),
 COALESCE(m.list_id,''),COALESCE(m.subject,''),COALESCE(m.snippet,''),m.roster_count,
 e.participant_id,pp.person_id,COALESCE(e.role,''),COALESCE(e.envelope_address,p.email_address,''),COALESCE(p.display_name,''),
	 CASE WHEN COALESCE(m.source_is_from_me,FALSE) AND e.role='from' THEN
	   CASE WHEN m.sender_id IS NOT NULL THEN e.participant_id=m.sender_id
	        ELSE ((SELECT COUNT(*) FROM unique_edges d WHERE d.message_id=m.id AND d.role='from')=1 OR e.identity_owner=1) END
	 ELSE e.identity_owner=1 END
 FROM selected_messages m JOIN sources s ON s.id=m.source_id
 LEFT JOIN bounded_edges e ON e.message_id=m.id AND e.edge_number<=%d
 LEFT JOIN participants p ON p.id=e.participant_id
 LEFT JOIN person_participants pp ON pp.participant_id=p.id
 ORDER BY m.id,e.edge_number`
	q = fmt.Sprintf(q, maxRosterEdges+1, maxRosterEdges, maxRosterEdges)
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(q), args...)
	if err != nil {
		return result, fmt.Errorf("read people roles: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var m MessageRoles
		var sent, received, internalDate sql.NullTime
		var participant, person sql.NullInt64
		var role string
		var edge RoleEdge
		var owner sql.NullBool
		var roster int
		if err := rows.Scan(&m.MessageID, &m.SourceMessageID, &m.SourceID, &m.SourceType, &m.ConversationID, &m.ConversationType, &m.MessageType, &sent, &received, &internalDate, &m.IsFromMe, &m.ListID, &m.Subject, &m.Snippet, &roster, &participant, &person, &role, &edge.EmailAddress, &edge.DisplayName, &owner); err != nil {
			return result, fmt.Errorf("scan people roles: %w", err)
		}
		if existing, ok := result.Messages[m.MessageID]; ok {
			m = existing
		} else {
			for _, at := range []sql.NullTime{sent, received, internalDate} {
				if at.Valid {
					v := at.Time
					m.SentAt = &v
					break
				}
			}
			m.Edges = []RoleEdge{}
			if roster > maxRosterEdges && slices.Contains([]string{"direct_chat", "group_chat", "channel", "meeting"}, m.ConversationType) {
				result.LargeRoomMessageIDs = append(result.LargeRoomMessageIDs, m.MessageID)
			}
		}
		if participant.Valid {
			edge.ParticipantID = participant.Int64
			edge.Role = personscope.Role(role)
			edge.IsOwner = owner.Valid && owner.Bool
			if person.Valid {
				v := person.Int64
				edge.PersonID = &v
			}
			if edge.IsOwner && edge.Role == personscope.RoleFrom {
				m.IsFromMe = true
			}
			m.Edges = append(m.Edges, edge)
		}
		result.Messages[m.MessageID] = m
	}
	if err := rows.Err(); err != nil {
		return result, fmt.Errorf("iterate people roles: %w", err)
	}
	return result, nil
}

// SourceAuthenticatesSender reports whether the source sets the sender identity.
// Email headers are assertions, unlike platform-authenticated chat senders.
func SourceAuthenticatesSender(sourceType string) bool {
	return personSweepSourceAuthenticatesSender(sourceType)
}
