package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const maxIntroCounterparts = 4

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
	direct := candidate.ConversationType == string(MessageExportConversationDirectChat)
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
	if email && classification.Direction == DirectionInbound && sender != nil && !ownerLinked(*sender) && !targetSender && introAddressed && len(others) <= maxIntroCounterparts {
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

// earliestFirstContactTx reads the earliest linked event in the current snapshot.
func earliestFirstContactTx(ctx context.Context, tx *loggedTx, personID int64) (ActivityEvent, sql.NullString, error) {
	var event ActivityEvent
	var refKind sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT ae.message_id, ae.occurred_at, ae.conversation_id, ae.ref_kind
 FROM activity_event_persons aep JOIN activity_events ae ON ae.message_id = aep.message_id
 WHERE aep.person_id = ? ORDER BY ae.occurred_at, ae.message_id LIMIT 1`, personID).Scan(&event.MessageID, &event.OccurredAt, &event.ConversationID, &refKind)
	return event, refKind, err
}

func (s *Store) populateFirstContactTx(ctx context.Context, tx *loggedTx, state *ContactState) error {
	event, refKind, err := earliestFirstContactTx(ctx, tx, state.PersonID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read first contact: %w", err)
	}
	messageID := event.MessageID
	occurredAt := sql.NullTime{Time: event.OccurredAt, Valid: true}
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
		err = tx.QueryRowContext(ctx, `SELECT COALESCE(NULLIF(TRIM(person.display_name), ''), NULLIF(TRIM(`+participantSenderNameSQL+`), ''), NULLIF(TRIM(p.email_address), ''), NULLIF(TRIM(p.phone_number), ''), '') FROM participants p
  LEFT JOIN persons person ON person.id = ?
  LEFT JOIN message_recipients mr ON mr.participant_id = p.id AND mr.message_id = ? AND mr.recipient_type = 'from'
  WHERE p.id = ? ORDER BY mr.id LIMIT 1`, state.Introducer.PersonID, messageID, state.Introducer.ParticipantID).Scan(&state.Introducer.DisplayName)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("load first-contact introducer: %w", err)
		}
	}
	return nil
}
