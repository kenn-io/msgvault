package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/persontiers"
)

const personTierSignalPageSize = 500

// PersonTierReadiness describes whether the activity spine is sufficiently
// reconciled to authorize demotions. Promotions can still use incomplete signal.
type PersonTierReadiness struct {
	Ready   bool     `json:"ready"`
	Reasons []string `json:"reasons"`
}

func (s *Store) PersonTierReadinessContext(ctx context.Context) (PersonTierReadiness, error) {
	result := PersonTierReadiness{Reasons: []string{}}
	pending, err := s.ListActivityProjectionQueueContext(ctx, 1)
	if err != nil {
		return result, err
	}
	if len(pending) > 0 {
		result.Reasons = append(result.Reasons, "pending_activity")
	}
	timezone, err := s.ActivityTimezoneTransitionContext(ctx)
	if err != nil {
		return result, err
	}
	if timezone.Active {
		result.Reasons = append(result.Reasons, "timezone_transition")
	}
	direct, err := s.ActivityDirectLimitTransitionContext(ctx)
	if err != nil {
		return result, err
	}
	if direct.Active {
		result.Reasons = append(result.Reasons, "direct_limit_transition")
	}
	reconciled, known, err := s.ActivityReconciledRevisionsContext(ctx)
	if err != nil {
		return result, err
	}
	current, err := s.ContactRevisionsContext(ctx)
	if err != nil {
		return result, err
	}
	if !known || reconciled != current {
		result.Reasons = append(result.Reasons, "identity_revision_changed")
	}
	watermark, err := s.ActivityWatermarkContext(ctx)
	if err != nil {
		return result, err
	}
	var liveMax int64
	if err = s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM messages WHERE deleted_at IS NULL AND deleted_from_source_at IS NULL`).Scan(&liveMax); err != nil {
		return result, fmt.Errorf("read live messages for tier readiness: %w", err)
	}
	if watermark < liveMax {
		result.Reasons = append(result.Reasons, "watermark_behind")
	}
	result.Ready = len(result.Reasons) == 0
	return result, nil
}

// PersonTierSignalsContext reads one bounded page of resolved people. asOf is
// already in the projector timezone; windows include both boundary dates.
// Explanation counters describe the long window and can overlap.
func (s *Store) PersonTierSignalsContext(ctx context.Context, personIDs []int64, asOf time.Time, cfg persontiers.Config, maxDirect int) (map[int64]persontiers.Signals, error) {
	result := make(map[int64]persontiers.Signals, len(personIDs))
	if len(personIDs) == 0 {
		return result, nil
	}
	if len(personIDs) > personTierSignalPageSize || asOf.IsZero() {
		return nil, fmt.Errorf("%w: tier signals require at most 500 people and a nonzero as-of date", ErrInvalidActivityRequest)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidActivityRequest, err)
	}
	args := make([]any, 0, len(personIDs)+5)
	for _, id := range personIDs {
		if id <= 0 {
			return nil, fmt.Errorf("%w: person ids must be positive", ErrInvalidActivityRequest)
		}
		args = append(args, id)
		result[id] = persontiers.Signals{}
	}
	if maxDirect <= 0 {
		maxDirect = DefaultMaxDirectActivityCounterparts
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(personIDs)), ",")
	from := asOf.AddDate(0, 0, -(cfg.LongWindowDays - 1)).Format(time.DateOnly)
	short := asOf.AddDate(0, 0, -(cfg.ShortWindowDays - 1)).Format(time.DateOnly)
	through := asOf.Format(time.DateOnly)
	excludeList := 0
	if cfg.ExcludeListMessages {
		excludeList = 1
	}
	args = append(args, from, through, maxDirect, excludeList, short)
	query := personTierSignalQuery(marks)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read person tier signals: %w", err)
	}
	for rows.Next() {
		var id int64
		var signal persontiers.Signals
		if err = rows.Scan(&id, &signal.OutboundShort, &signal.OutboundLong, &signal.InboundShort, &signal.InboundLong, &signal.ActiveDaysShort, &signal.ActiveDaysLong, &signal.MeetingDaysLong, &signal.ChannelsLong, &signal.ExcludedListEvents, &signal.LargeAudienceInboundEvents, &signal.CoPresenceEvents, &signal.ObservedEvents); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan person tier signals: %w", err)
		}
		result[id] = signal
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate person tier signals: %w", err)
	}
	if err = rows.Close(); err != nil {
		return nil, fmt.Errorf("close person tier signals: %w", err)
	}
	// Recency deliberately keeps all direct evidence, including list traffic and
	// large audiences, and has no lower date boundary.
	lastArgs := append(slices.Clone(args[:len(personIDs)]), through)
	rows, err = s.db.QueryContext(ctx, `SELECT aep.person_id,MAX(ae.occurred_at)
 FROM activity_event_persons aep JOIN activity_events ae ON ae.message_id=aep.message_id
 JOIN messages m ON m.id=ae.message_id
 WHERE aep.person_id IN (`+marks+`) AND aep.local_date<=? AND aep.evidence='direct'
 AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL GROUP BY aep.person_id`, lastArgs...)
	if err != nil {
		return nil, fmt.Errorf("read person tier direct recency: %w", err)
	}
	for rows.Next() {
		var id int64
		var last nullableTimestamp
		if err = rows.Scan(&id, &last); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan person tier direct recency: %w", err)
		}
		signal := result[id]
		signal.LastDirectAt = nullableTimePointer(last)
		result[id] = signal
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate person tier direct recency: %w", err)
	}
	if err = rows.Close(); err != nil {
		return nil, fmt.Errorf("close person tier direct recency: %w", err)
	}
	if err = s.personTierBindingsContext(ctx, marks, args[:len(personIDs)], result); err != nil {
		return nil, err
	}
	return result, nil
}

// personTierOwnerMatch reuses the activity loader's per-type identity matching:
// primary email is authoritative over email aliases, and native IDs are exact.
// An account_identities row is confirmed by its presence; candidates live in a
// separate table. Empty sourceExpr checks identities in every source.
func personTierOwnerMatch(participantExpr, sourceExpr string) string {
	source := ""
	if sourceExpr != "" {
		source = "ai.source_id=" + sourceExpr + " AND "
	}
	return `EXISTS (SELECT 1 FROM participants op JOIN account_identities ai ON ` + source + `(
 (` + ownerEmailMatch("op.email_address", "ai.address") + `) OR EXISTS (
 SELECT 1 FROM participant_identifiers oi WHERE oi.participant_id=op.id AND ` +
		ownerIdentifierMatch("oi.identifier_type", "oi.identifier_value", "ai.address", " AND (op.email_address IS NULL OR TRIM(op.email_address)='')") + `)) WHERE op.id=` + participantExpr + `)`
}

// personTierResolvedOwnerMatch extends source ownership through linked
// participants and participants bound to the same saved person. Chat
// conversations often retain aliases while account_identities names one.
func personTierResolvedOwnerMatch(participantExpr, sourceExpr string) string {
	return `EXISTS (` + personTierResolvedNodesCTE(participantExpr) + `
 SELECT 1 FROM resolved_nodes linked
 WHERE linked.node_kind=1 AND ` + personTierOwnerMatch("linked.node_id", sourceExpr) + `)`
}

func personTierResolvedParticipantMatch(participantExpr, rootExpr string) string {
	return `EXISTS (` + personTierResolvedNodesCTE(rootExpr) + `
 SELECT 1 FROM resolved_nodes WHERE node_kind=1 AND node_id=` + participantExpr + `)`
}

// personTierResolvedNodesCTE follows confirmed links and saved-person bindings
// through one recursive term. Person nodes keep participant and person IDs in
// separate namespaces without building every pair of participants on a person.
func personTierResolvedNodesCTE(rootExpr string) string {
	return `WITH RECURSIVE identity_edges(from_kind,from_id,to_kind,to_id) AS (
	 SELECT 1,links.participant_a,1,links.participant_b FROM participant_links links
	 UNION
	 SELECT 1,links.participant_b,1,links.participant_a FROM participant_links links
	 UNION
	 SELECT 1,bound.participant_id,2,bound.person_id FROM person_participants bound
	 UNION
	 SELECT 2,bound.person_id,1,bound.participant_id FROM person_participants bound
	), resolved_nodes(node_kind,node_id) AS (
	 SELECT 1,` + rootExpr + `
	 UNION
	 SELECT edges.to_kind,edges.to_id
	 FROM identity_edges edges
	 JOIN resolved_nodes resolved
	  ON edges.from_kind=resolved.node_kind AND edges.from_id=resolved.node_id
	) `
}

// personTierRecipientOwnerMatch checks the archived envelope address and the
// recipient's confirmed participant/person identity. Linked owner aliases stay
// owned even when an envelope snapshot carries a different or missing address.
func personTierRecipientOwnerMatch(participantExpr, envelopeExpr, sourceExpr string) string {
	return `(EXISTS (SELECT 1 FROM account_identities ai WHERE ai.source_id=` + sourceExpr + ` AND ` +
		ownerEmailMatch(envelopeExpr, "ai.address") + `) OR ` +
		personTierResolvedOwnerMatch(participantExpr, sourceExpr) + `)`
}

// personTierResolvedArchivedOwnerMatch extends source ownership through linked
// participants and saved-person bindings, including archived envelope addresses
// after a participant merge replaces the owner's current email.
func personTierResolvedArchivedOwnerMatch(participantExpr, conversationExpr, sourceExpr string) string {
	return `EXISTS (` + personTierResolvedNodesCTE(participantExpr) + `
 SELECT 1 FROM resolved_nodes linked
 WHERE linked.node_kind=1 AND (
  ` + personTierOwnerMatch("linked.node_id", sourceExpr) + ` OR EXISTS (
   SELECT 1 FROM message_recipients archived_owner
   WHERE archived_owner.participant_id=linked.node_id
    AND archived_owner.recipient_type IN ('from','to','cc','bcc')
    AND EXISTS (
     SELECT 1 FROM messages archived_message
     WHERE archived_message.id=archived_owner.message_id
      AND archived_message.conversation_id=` + conversationExpr + `
      AND archived_message.source_id=` + sourceExpr + `
    )
    AND EXISTS (
     SELECT 1 FROM account_identities archived_identity
     WHERE archived_identity.source_id=` + sourceExpr + `
      AND ` + ownerEmailMatch("archived_owner.email_address", "archived_identity.address") + `
    )
  )
 )
 )`
}

func personTierSignalQuery(marks string) string {
	return `WITH page_events AS (
 SELECT aep.person_id,aep.role,aep.evidence,aep.local_date,ae.message_id,ae.ref_kind,
 ae.direction,ae.channel,m.conversation_id,m.source_id,m.list_id
 FROM activity_event_persons aep
 JOIN activity_events ae ON ae.message_id=aep.message_id
 JOIN messages m ON m.id=ae.message_id
 WHERE aep.person_id IN (` + marks + `) AND aep.local_date BETWEEN ? AND ?
 AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL
 ), page_inbound AS (
 SELECT message_id,channel,conversation_id,source_id FROM page_events
 WHERE evidence='direct' AND ref_kind='message' AND direction='inbound' AND role='sender'
 GROUP BY message_id,channel,conversation_id,source_id
 ), email_owner_participants AS (
 -- Resolve owner status from every envelope row before counting participant
 -- snapshots; participant merges can retain owner and alias addresses together.
 SELECT i.message_id,owner_envelope.participant_id
 FROM page_inbound i JOIN message_recipients owner_envelope ON owner_envelope.message_id=i.message_id
 WHERE owner_envelope.recipient_type IN ('from','to','cc','bcc')
  AND ` + personTierRecipientOwnerMatch("owner_envelope.participant_id", "owner_envelope.email_address", "i.source_id") + `
 GROUP BY i.message_id,owner_envelope.participant_id
 ), chat_conversations AS (
 SELECT conversation_id,source_id FROM page_inbound WHERE channel='chat' GROUP BY conversation_id,source_id
	 ), audience_members AS (
	 -- Audience limits count participant IDs, even when aliases share a person.
	 SELECT 'chat' AS audience_kind,c.conversation_id AS scope_id,c.source_id,cp.participant_id
	FROM chat_conversations c JOIN conversation_participants cp ON cp.conversation_id=c.conversation_id
	WHERE NOT ` + personTierResolvedArchivedOwnerMatch("cp.participant_id", "c.conversation_id", "c.source_id") + `
	UNION ALL
	SELECT 'email',i.message_id,i.source_id,mr.participant_id
	FROM page_inbound i JOIN message_recipients mr ON mr.message_id=i.message_id
	WHERE mr.recipient_type IN ('to','cc','bcc')
	 AND NOT EXISTS (
	  SELECT 1 FROM email_owner_participants owner
	  WHERE owner.message_id=i.message_id AND owner.participant_id=mr.participant_id
	 )
 ), chat_audiences AS (
	SELECT scope_id AS conversation_id,source_id,COUNT(DISTINCT participant_id) AS audience
	FROM audience_members WHERE audience_kind='chat'
	GROUP BY scope_id,source_id
 ), email_audiences AS (
	SELECT scope_id AS message_id,COUNT(DISTINCT participant_id) AS audience
	FROM audience_members WHERE audience_kind='email'
	GROUP BY scope_id
 ), large_audience AS (
 SELECT i.message_id FROM page_inbound i
	LEFT JOIN chat_audiences c ON c.conversation_id=i.conversation_id AND c.source_id=i.source_id
	LEFT JOIN email_audiences e ON e.message_id=i.message_id
	WHERE CASE WHEN i.channel='chat' THEN COALESCE(c.audience,0) ELSE COALESCE(e.audience,0) END > ?
 ), excluded AS (
 SELECT e.*,CASE WHEN e.list_id IS NOT NULL AND ?=1 THEN 1 ELSE 0 END AS excluded_list,
 CASE WHEN l.message_id IS NOT NULL THEN 1 ELSE 0 END AS large_inbound
 FROM page_events e LEFT JOIN large_audience l ON l.message_id=e.message_id
 ), counted AS (
 SELECT e.*,CASE WHEN evidence='direct' AND excluded_list=0 AND direction IN ('inbound','outbound') AND (
 (ref_kind='meeting' AND role IN ('attendee','organizer')) OR
 (ref_kind='message' AND direction='inbound' AND role='sender' AND large_inbound=0) OR
 (ref_kind='message' AND direction='outbound' AND role IN ('addressed','member'))
 ) THEN 1 ELSE 0 END AS counts,CASE WHEN local_date>=? THEN 1 ELSE 0 END AS short_window FROM excluded e
 ) SELECT person_id,
 SUM(CASE WHEN counts=1 AND ref_kind='message' AND direction='outbound' AND short_window=1 THEN 1 ELSE 0 END),
 SUM(CASE WHEN counts=1 AND ref_kind='message' AND direction='outbound' THEN 1 ELSE 0 END),
 SUM(CASE WHEN counts=1 AND ref_kind='message' AND direction='inbound' AND short_window=1 THEN 1 ELSE 0 END),
 SUM(CASE WHEN counts=1 AND ref_kind='message' AND direction='inbound' THEN 1 ELSE 0 END),
 COUNT(DISTINCT CASE WHEN counts=1 AND short_window=1 THEN local_date END),
 COUNT(DISTINCT CASE WHEN counts=1 THEN local_date END),
 COUNT(DISTINCT CASE WHEN counts=1 AND ref_kind='meeting' THEN local_date END),
 COUNT(DISTINCT CASE WHEN counts=1 THEN channel END),
 SUM(excluded_list),SUM(large_inbound),
 SUM(CASE WHEN evidence='co_presence' THEN 1 ELSE 0 END),
 SUM(CASE WHEN direction='observed' THEN 1 ELSE 0 END)
 FROM counted GROUP BY person_id`
}

func (s *Store) personTierBindingsContext(ctx context.Context, marks string, args []any, signals map[int64]persontiers.Signals) error {
	rows, err := s.db.QueryContext(ctx, `SELECT pp.person_id,COALESCE(p.email_address,''),COALESCE(p.phone_number,''),
 COALESCE(pi.identifier_type,''),COALESCE(pi.identifier_value,''),`+personTierOwnerMatch("p.id", "")+`
 FROM person_participants pp JOIN participants p ON p.id=pp.participant_id
 LEFT JOIN participant_identifiers pi ON pi.participant_id=p.id WHERE pp.person_id IN (`+marks+`)
 ORDER BY pp.person_id,p.id,pi.identifier_type,pi.identifier_value`, args...)
	if err != nil {
		return fmt.Errorf("read person tier bindings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var email, phone, kind, value string
		var owner bool
		if err = rows.Scan(&id, &email, &phone, &kind, &value, &owner); err != nil {
			return fmt.Errorf("scan person tier bindings: %w", err)
		}
		signal := signals[id]
		signal.Owner = signal.Owner || owner
		signal.HasPhoneOrChat = signal.HasPhoneOrChat || strings.TrimSpace(phone) != "" || (kind != "" && kind != "email" && strings.TrimSpace(value) != "")
		emails := []string{email}
		if kind == "email" {
			emails = append(emails, value)
		}
		for _, address := range emails {
			address = strings.ToLower(strings.TrimSpace(address))
			if address != "" && !slices.Contains(signal.Emails, address) {
				signal.Emails = append(signal.Emails, address)
			}
		}
		slices.Sort(signal.Emails)
		signals[id] = signal
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("iterate person tier bindings: %w", err)
	}
	if err = rows.Close(); err != nil {
		return fmt.Errorf("close person tier bindings: %w", err)
	}
	return nil
}
