package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const maxIntroCounterparts = 4

type FirstContactKind string

const (
	FirstContactEmailIntro FirstContactKind = "email_intro"
	FirstContactObserved   FirstContactKind = "observed"
)

type FirstContactIntroducer struct {
	ParticipantID int64  `json:"participant_id"`
	DisplayName   string `json:"display_name"`
	PersonID      *int64 `json:"person_id,omitzero" nullable:"false"`
}

// ClassifyFirstContact describes a person's earliest linked event using current identities.
func ClassifyFirstContact(candidate ActivityCandidate, personID int64, maxDirectCounterparts int) (FirstContactKind, *FirstContactIntroducer) {
	if IsMeetingMessageType(candidate.MessageType) || candidate.ConversationType != "email_thread" || ClassifyActivityCandidate(candidate, maxDirectCounterparts).Direction != DirectionInbound {
		return FirstContactObserved, nil
	}
	ownerLinked := activityOwnerLinked(candidate.Counterparts)
	var sender *ActivityCounterpart
	fromCount := 0
	addressed := false
	others := map[audienceKey]struct{}{}
	for i := range candidate.Counterparts {
		c := &candidate.Counterparts[i]
		isTarget := c.PersonID != nil && *c.PersonID == personID
		if c.RecipientType == recipientTypeFrom {
			fromCount++
			if candidate.SenderID == nil || *candidate.SenderID == c.ParticipantID {
				sender = c
			}
			continue
		}
		if isTarget && (c.RecipientType == "to" || c.RecipientType == "cc") {
			addressed = true
		}
		if (c.RecipientType == "to" || c.RecipientType == "cc" || c.RecipientType == "bcc") && !isTarget && !ownerLinked(*c) {
			others[activityAudienceKey(*c)] = struct{}{}
		}
	}
	if candidate.SenderID == nil && fromCount != 1 {
		sender = nil
	}
	if sender != nil && !ownerLinked(*sender) && (sender.PersonID == nil || *sender.PersonID != personID) && addressed && len(others) <= maxIntroCounterparts {
		return FirstContactEmailIntro, &FirstContactIntroducer{ParticipantID: sender.ParticipantID, PersonID: sender.PersonID}
	}
	return FirstContactObserved, nil
}

// earliestFirstContactTx reads the earliest linked event in the current snapshot.
func earliestFirstContactTx(ctx context.Context, tx *loggedTx, personID int64) (ActivityEvent, string, error) {
	const eligible = ` FROM activity_event_persons aep JOIN activity_events ae ON ae.message_id = aep.message_id
 WHERE aep.person_id = ? AND NOT (aep.role = 'member' AND ae.ref_kind <> 'meeting' AND EXISTS (SELECT 1 FROM conversations c WHERE c.id = ae.conversation_id AND c.conversation_type IN ('group_chat', 'channel')))`
	var event ActivityEvent
	var refKind, firstDate string
	err := tx.QueryRowContext(ctx, `SELECT aep.local_date`+eligible+` ORDER BY aep.local_date LIMIT 1`, personID).Scan(&firstDate)
	if err != nil {
		return event, refKind, err
	}
	date, err := time.Parse(time.DateOnly, firstDate)
	if err != nil {
		return event, refKind, fmt.Errorf("parse first appearance date: %w", err)
	}
	endDate := date.AddDate(0, 0, 2).Format(time.DateOnly)
	err = tx.QueryRowContext(ctx, `SELECT ae.message_id, ae.occurred_at, ae.conversation_id, ae.ref_kind`+eligible+` AND aep.local_date BETWEEN ? AND ? ORDER BY ae.occurred_at, ae.message_id LIMIT 1`, personID, firstDate, endDate).Scan(&event.MessageID, &event.OccurredAt, &event.ConversationID, &refKind)
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
	state.FirstSeenRef = activityStoredRef(sql.NullString{String: refKind, Valid: true}, sql.NullInt64{Int64: messageID, Valid: true})
	candidates, err := s.loadActivityCandidatesByIDQueryerContext(ctx, tx, []int64{messageID})
	if err != nil {
		return fmt.Errorf("load first-contact candidate: %w", err)
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
