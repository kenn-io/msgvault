package personscope

import (
	"errors"
	"fmt"
	"strings"
)

// Validate checks an internal person constraint. The public resolver rejects
// zero participants; MessagePredicate still renders a directly-built empty
// scope as FALSE for defense in depth.
func Validate(scope Scope) error {
	if len(scope.Directions) == 0 {
		return errors.New("resolved person scope requires directions")
	}
	for _, id := range scope.ParticipantIDs {
		if id <= 0 {
			return errors.New("resolved person scope contains an invalid participant ID")
		}
	}
	for _, direction := range scope.Directions {
		switch direction {
		case FromPerson, ToPerson, Group:
		default:
			return fmt.Errorf("resolved person scope contains unknown direction %q", direction)
		}
	}
	return nil
}

// MessagePredicate returns a dialect-neutral SQL predicate (using ? bind
// placeholders) for the shared message-person relation. Callers must provide
// aliases for the messages and conversations tables.
func MessagePredicate(scope Scope, message, conversation string) (string, []any) {
	if len(scope.ParticipantIDs) == 0 || len(scope.Directions) == 0 {
		return "FALSE", nil
	}
	var conditions []string
	var args []any
	appendIDs := func() string {
		for _, id := range scope.ParticipantIDs {
			args = append(args, id)
		}
		return placeholders(len(scope.ParticipantIDs))
	}
	directionEvidence := `(` + message + `.is_from_me = TRUE OR ` + message + `.sender_id IS NOT NULL OR EXISTS (
		SELECT 1 FROM message_recipients known_from
		WHERE known_from.message_id = ` + message + `.id
		  AND LOWER(known_from.recipient_type) = 'from'))`
	for _, direction := range scope.Directions {
		switch direction {
		case FromPerson:
			senderIDs, envelopeIDs := appendIDs(), appendIDs()
			conditions = append(conditions, `(`+message+`.sender_id IN (`+senderIDs+`) OR EXISTS (
				SELECT 1 FROM message_recipients person_from
				WHERE person_from.message_id = `+message+`.id
				  AND LOWER(person_from.recipient_type) = 'from'
				  AND person_from.participant_id IN (`+envelopeIDs+`)))`)
		case ToPerson:
			explicitIDs, rosterIDs := appendIDs(), appendIDs()
			senderIDs, envelopeSenderIDs := appendIDs(), appendIDs()
			conditions = append(conditions, `(EXISTS (
				SELECT 1 FROM message_recipients person_to
				WHERE person_to.message_id = `+message+`.id
				  AND LOWER(person_to.recipient_type) IN ('to', 'cc', 'bcc')
				  AND person_to.participant_id IN (`+explicitIDs+`)) OR (
				LOWER(COALESCE(`+conversation+`.conversation_type, '')) = 'direct_chat'
				AND `+directionEvidence+`
				AND EXISTS (SELECT 1 FROM conversation_participants person_roster
					WHERE person_roster.conversation_id = `+message+`.conversation_id
					  AND person_roster.participant_id IN (`+rosterIDs+`))
				AND NOT ((`+message+`.sender_id IS NOT NULL AND `+message+`.sender_id IN (`+senderIDs+`)) OR (
					`+message+`.sender_id IS NULL AND EXISTS (
						SELECT 1 FROM message_recipients scoped_sender
						WHERE scoped_sender.message_id = `+message+`.id
						  AND LOWER(scoped_sender.recipient_type) = 'from'
						  AND scoped_sender.participant_id IN (`+envelopeSenderIDs+`))))))`)
		case Group:
			rosterIDs := appendIDs()
			rosterType := `LOWER(COALESCE(` + conversation + `.conversation_type, '')) IN ('group_chat', 'channel')`
			if scope.IncludeUnclassifiedRosterRows {
				rosterType = `(LOWER(COALESCE(` + conversation + `.conversation_type, '')) <> 'direct_chat' OR NOT ` + directionEvidence + `)`
			}
			conditions = append(conditions, `(`+rosterType+` AND EXISTS (
				SELECT 1 FROM conversation_participants person_group
				WHERE person_group.conversation_id = `+message+`.conversation_id
				  AND person_group.participant_id IN (`+rosterIDs+`)))`)
		}
	}
	return strings.Join(conditions, " OR "), args
}

func placeholders(count int) string {
	if count <= 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}
