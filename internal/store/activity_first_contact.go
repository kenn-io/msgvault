package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const maxIntroCounterparts = 4

// FirstContactKind describes the evidence for a person's first appearance.
type FirstContactKind string

const (
	FirstContactEmailIntro FirstContactKind = "email_intro"
	FirstContactObserved   FirstContactKind = "observed"
)

// FirstContactIntroducer identifies the sender of an introduction email.
type FirstContactIntroducer struct {
	ParticipantID int64  `json:"participant_id"`
	DisplayName   string `json:"display_name"`
	PersonID      *int64 `json:"person_id,omitzero" nullable:"false"`
}

// ClassifyFirstContact describes a person's earliest linked event using current identities.
func ClassifyFirstContact(candidate ActivityCandidate, personID, envelopeSenderID int64) (FirstContactKind, *FirstContactIntroducer) {
	direction, _ := activityDirection(candidate.SourceIsFromMe, candidate.Counterparts)
	if IsMeetingMessageType(candidate.MessageType) || candidate.ConversationType != "email_thread" || direction != DirectionInbound || envelopeSenderID == 0 {
		return FirstContactObserved, nil
	}
	ownerLinked := activityOwnerLinked(candidate.Counterparts)
	var sender *ActivityCounterpart
	addressed := false
	others := map[audienceKey]struct{}{}
	for i := range candidate.Counterparts {
		c := &candidate.Counterparts[i]
		isTarget := c.PersonID != nil && *c.PersonID == personID
		if c.RecipientType == recipientTypeFrom {
			if envelopeSenderID == c.ParticipantID {
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
	if sender != nil && !ownerLinked(*sender) && (sender.PersonID == nil || *sender.PersonID != personID) && addressed && len(others) <= maxIntroCounterparts {
		return FirstContactEmailIntro, &FirstContactIntroducer{ParticipantID: sender.ParticipantID, PersonID: sender.PersonID}
	}
	return FirstContactObserved, nil
}

// earliestFirstContactTx reads the earliest linked event in the current snapshot.
func earliestFirstContactTx(ctx context.Context, tx *loggedTx, personID int64) (ActivityEvent, string, error) {
	const eligible = ` FROM activity_event_persons aep JOIN activity_events ae ON ae.message_id = aep.message_id
 WHERE aep.person_id = ? AND aep.role <> 'member'`
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
	// UTC−12 through UTC+14 place the earliest timestamp within three local dates.
	endDate := date.AddDate(0, 0, 2).Format(time.DateOnly)
	err = tx.QueryRowContext(ctx, `SELECT ae.message_id, ae.occurred_at, ae.conversation_id, ae.ref_kind, ae.channel`+eligible+` AND aep.local_date BETWEEN ? AND ? ORDER BY ae.occurred_at, ae.message_id LIMIT 1`, personID, firstDate, endDate).Scan(&event.MessageID, &event.OccurredAt, &event.ConversationID, &refKind, &event.Channel)
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
	if event.Channel != ChannelEmail {
		state.FirstContactKind = FirstContactObserved
		return nil
	}
	candidates, err := s.loadActivityCandidatesByIDQueryerContext(ctx, tx, []int64{messageID})
	if err != nil {
		return fmt.Errorf("load first-contact candidate: %w", err)
	}
	candidate := candidates[0]
	var count, participants, relayed int
	var senderID, recordedSenderID sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT NULLIF(LOWER(TRIM(mr.email_address)), '')),
 COUNT(DISTINCT CASE WHEN NULLIF(TRIM(mr.email_address), '') IS NOT NULL THEN mr.participant_id END),
 MIN(CASE WHEN NULLIF(TRIM(mr.email_address), '') IS NOT NULL THEN mr.participant_id END), m.sender_id,
 SUM(CASE WHEN mr.is_relayed THEN 1 ELSE 0 END)
 FROM messages m LEFT JOIN message_recipients mr ON mr.message_id = m.id AND mr.recipient_type = 'from'
 WHERE m.id = ? GROUP BY m.sender_id`, messageID).Scan(&count, &participants, &senderID, &recordedSenderID, &relayed)
	if err != nil {
		return fmt.Errorf("load first-contact sender evidence: %w", err)
	}
	if count != 1 || participants != 1 || relayed != 0 || (recordedSenderID.Valid && recordedSenderID.Int64 != senderID.Int64) {
		senderID = sql.NullInt64{}
	}
	state.FirstContactKind, state.Introducer = ClassifyFirstContact(candidate, state.PersonID, senderID.Int64)
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
