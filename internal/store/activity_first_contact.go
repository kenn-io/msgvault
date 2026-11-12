package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

type FirstContactKind string

const (
	FirstContactEmailIntro      FirstContactKind = "email_intro"
	FirstContactOwnerOutreach   FirstContactKind = "owner_outreach"
	FirstContactInboundOutreach FirstContactKind = "inbound_outreach"
	FirstContactGroupShared     FirstContactKind = "group_shared"
	FirstContactMeeting         FirstContactKind = "meeting"
	FirstContactObserved        FirstContactKind = "observed"
)

type FirstContactIntroducer struct {
	ParticipantID int64  `json:"participant_id"`
	DisplayName   string `json:"display_name"`
	PersonID      *int64 `json:"person_id,omitzero" nullable:"false"`
}

// ClassifyFirstContact describes a person's earliest linked event using current identities.
func ClassifyFirstContact(candidate ActivityCandidate, personID int64, maxDirectCounterparts int) (FirstContactKind, *FirstContactIntroducer) {
	if IsMeetingMessageType(candidate.MessageType) {
		return FirstContactMeeting, nil
	}
	counterparts := collapseActivityCounterparts(candidate.Counterparts)
	ownerLinked := activityOwnerLinked(counterparts)
	classification := ClassifyActivityCandidate(candidate, maxDirectCounterparts)
	if classification.Direction == DirectionObserved {
		return FirstContactObserved, nil
	}
	switch candidate.ConversationType {
	case "group_chat", "channel":
		return FirstContactGroupShared, nil
	}
	email := candidate.ConversationType == "email_thread"
	direct := candidate.ConversationType == "direct_chat"
	if !email && !direct {
		return FirstContactObserved, nil
	}
	var sender, ownerSender *ActivityCounterpart
	addressed, introAddressed, targetSender := false, false, false
	others := map[audienceKey]struct{}{}
	for i := range counterparts {
		c := &counterparts[i]
		isTarget := c.PersonID != nil && *c.PersonID == personID
		if c.RecipientType == recipientTypeFrom {
			if sender == nil {
				sender = c
			}
			if ownerLinked(*c) {
				ownerSender = c
			}
			if isTarget {
				targetSender = true
			}
			continue
		}
		recipient := c.RecipientType == "to" || c.RecipientType == "cc" || c.RecipientType == "bcc" || (direct && c.RecipientType == "member")
		if isTarget && recipient {
			addressed = true
			if c.RecipientType == "to" || c.RecipientType == "cc" {
				introAddressed = true
			}
		}
		if recipient && !isTarget && !ownerLinked(*c) {
			others[activityAudienceKey(*c)] = struct{}{}
		}
	}
	introducer := func(c *ActivityCounterpart) *FirstContactIntroducer {
		if c == nil {
			return nil
		}
		return &FirstContactIntroducer{ParticipantID: c.ParticipantID, PersonID: c.PersonID}
	}
	if email && classification.Direction == DirectionInbound && sender != nil && !ownerLinked(*sender) && !targetSender && introAddressed && len(others) <= 4 {
		return FirstContactEmailIntro, introducer(sender)
	}
	if classification.Direction == DirectionOutbound && addressed && !classification.Broadcast {
		if ownerSender != nil {
			return FirstContactOwnerOutreach, introducer(ownerSender)
		}
		return FirstContactOwnerOutreach, nil
	}
	if classification.Direction == DirectionInbound && targetSender && !classification.Broadcast {
		return FirstContactInboundOutreach, nil
	}
	return FirstContactObserved, nil
}

// firstContactWinnersTx captures winners before their links are removed.
func (s *Store) firstContactWinnersTx(ctx context.Context, tx *loggedTx, messageID int64) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT aep.person_id FROM activity_event_persons aep
		JOIN person_first_contacts pfc ON pfc.person_id = aep.person_id
		WHERE aep.message_id = ? AND pfc.first_seen_message_id = aep.message_id
		ORDER BY aep.person_id`, messageID)
	if err != nil {
		return nil, fmt.Errorf("find first-contact winners: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) notePersonFirstContactTx(ctx context.Context, tx *loggedTx, personID int64, event ActivityEvent) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO person_first_contacts (person_id, first_seen_at, first_seen_message_id)
 VALUES (?, ?, ?)
 ON CONFLICT(person_id) DO UPDATE SET first_seen_at = excluded.first_seen_at,
 first_seen_message_id = excluded.first_seen_message_id
 WHERE excluded.first_seen_at < person_first_contacts.first_seen_at OR
 (excluded.first_seen_at = person_first_contacts.first_seen_at AND excluded.first_seen_message_id < person_first_contacts.first_seen_message_id) OR
 NOT EXISTS (SELECT 1 FROM activity_event_persons aep WHERE aep.person_id = person_first_contacts.person_id AND aep.message_id = person_first_contacts.first_seen_message_id)`, personID, event.OccurredAt.UTC(), event.MessageID)
	if err != nil {
		return fmt.Errorf("note first contact for person %d: %w", personID, err)
	}
	return nil
}

// earliestFirstContactTx is shared by writer repair and read-only fallback.
func earliestFirstContactTx(ctx context.Context, tx *loggedTx, personID int64) (ActivityEvent, sql.NullString, error) {
	var event ActivityEvent
	var refKind sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT ae.message_id, ae.occurred_at, ae.conversation_id, ae.ref_kind
 FROM activity_event_persons aep JOIN activity_events ae ON ae.message_id = aep.message_id
 WHERE aep.person_id = ? ORDER BY ae.occurred_at, ae.message_id LIMIT 1`, personID).Scan(&event.MessageID, &event.OccurredAt, &event.ConversationID, &refKind)
	return event, refKind, err
}

func (s *Store) recomputePersonFirstContactTx(ctx context.Context, tx *loggedTx, personID int64) error {
	event, _, err := earliestFirstContactTx(ctx, tx, personID)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, `DELETE FROM person_first_contacts WHERE person_id = ?`, personID)
		return err
	}
	if err != nil {
		return fmt.Errorf("find earliest contact for person %d: %w", personID, err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO person_first_contacts (person_id, first_seen_at, first_seen_message_id)
 VALUES (?, ?, ?) ON CONFLICT(person_id) DO UPDATE SET
 first_seen_at = excluded.first_seen_at, first_seen_message_id = excluded.first_seen_message_id`, personID, event.OccurredAt.UTC(), event.MessageID)
	if err != nil {
		return fmt.Errorf("recompute first contact for person %d: %w", personID, err)
	}
	return nil
}

func (s *Store) retractActivityEventTx(ctx context.Context, tx *loggedTx, messageID int64) error {
	ids, err := s.firstContactWinnersTx(ctx, tx, messageID)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM activity_events WHERE message_id = ?`, messageID); err != nil {
		return fmt.Errorf("retract activity event %d: %w", messageID, err)
	}
	for _, id := range ids {
		if err := s.recomputePersonFirstContactTx(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
}

// populateFirstContactTx validates the cached pointer in the same snapshot as
// the rest of contact state. Cascades can invalidate it without a writer pass.
func (s *Store) populateFirstContactTx(ctx context.Context, tx *loggedTx, state *ContactState) error {
	var messageID int64
	var occurredAt sql.NullTime
	var refKind sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT ae.message_id, ae.occurred_at, ae.ref_kind
 FROM person_first_contacts pfc JOIN activity_events ae ON ae.message_id = pfc.first_seen_message_id
 WHERE pfc.person_id = ? AND ae.occurred_at = pfc.first_seen_at
 AND EXISTS (SELECT 1 FROM activity_event_persons aep WHERE aep.person_id = pfc.person_id AND aep.message_id = ae.message_id)
 AND NOT EXISTS (SELECT 1 FROM activity_event_persons earlier_link JOIN activity_events earlier ON earlier.message_id = earlier_link.message_id
 WHERE earlier_link.person_id = pfc.person_id AND (earlier.occurred_at < ae.occurred_at OR (earlier.occurred_at = ae.occurred_at AND earlier.message_id < ae.message_id)))`, state.PersonID).Scan(&messageID, &occurredAt, &refKind)
	if errors.Is(err, sql.ErrNoRows) {
		var event ActivityEvent
		event, refKind, err = earliestFirstContactTx(ctx, tx, state.PersonID)
		if err == nil {
			messageID = event.MessageID
			occurredAt = sql.NullTime{Time: event.OccurredAt, Valid: true}
		}
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read first contact: %w", err)
	}
	state.FirstSeenAt = utcNullTimePointer(occurredAt)
	state.FirstSeenRef = activityStoredRef(refKind, sql.NullInt64{Int64: messageID, Valid: true})
	candidates, err := s.loadActivityCandidatesByIDQueryerContext(ctx, tx, []int64{messageID})
	if err != nil {
		return fmt.Errorf("load first-contact candidate: %w", err)
	}
	if len(candidates) == 0 {
		return nil
	}
	candidate := candidates[0]
	state.FirstContactKind, state.Introducer = ClassifyFirstContact(candidate, state.PersonID, candidate.DirectLimitTransition.Target)
	if state.Introducer != nil {
		state.Introducer.DisplayName = "-"
		err = tx.QueryRowContext(ctx, `SELECT COALESCE(NULLIF(TRIM(person.display_name), ''), NULLIF(TRIM(`+participantSenderNameSQL+`), ''), NULLIF(TRIM(p.email_address), ''), NULLIF(TRIM(p.phone_number), ''), '-') FROM participants p
  LEFT JOIN persons person ON person.id = ?
  LEFT JOIN message_recipients mr ON mr.participant_id = p.id AND mr.message_id = ? AND mr.recipient_type = 'from'
  WHERE p.id = ? ORDER BY mr.id LIMIT 1`, state.Introducer.PersonID, messageID, state.Introducer.ParticipantID).Scan(&state.Introducer.DisplayName)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("load first-contact introducer: %w", err)
		}
	}
	return nil
}
