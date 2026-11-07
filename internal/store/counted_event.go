package store

import (
	"fmt"

	"go.kenn.io/msgvault/internal/identityindex"
)

// CountedEventInputs contains trusted SQL expressions for the counted-contact
// rule. ListExcluded and LargeInbound are integer expressions (0 or 1).
type CountedEventInputs struct {
	Evidence, Role, Direction, RefKind, ListExcluded, LargeInbound string
}

// CountedEventWhere selects direct contact eligible for tier and timeline
// counts. Live-message eligibility and address exclusions are applied by callers.
func CountedEventWhere(in CountedEventInputs) string {
	return fmt.Sprintf(`((%s)='direct' AND (%s)=0 AND (%s) IN ('inbound','outbound') AND (
 ((%s)='meeting' AND (%s) IN ('attendee','organizer') AND ((%s)='outbound' OR (%s)=0)) OR
 ((%s)='message' AND (%s)='inbound' AND (%s)='sender' AND (%s)=0) OR
 ((%s)='message' AND (%s)='outbound' AND (%s) IN ('addressed','member'))
 ))`, in.Evidence, in.ListExcluded, in.Direction,
		in.RefKind, in.Role, in.Direction, in.LargeInbound,
		in.RefKind, in.Direction, in.Role, in.LargeInbound,
		in.RefKind, in.Direction, in.Role)
}

// CountedEvent is the Go equivalent of CountedEventWhere for classified
// counterparts, including participants without a saved person.
func CountedEvent(evidence ActivityEvidence, role ActivityRole, direction ActivityDirection, refKind string, listExcluded, largeInbound bool) bool {
	if evidence != EvidenceDirect || listExcluded || (direction != DirectionInbound && direction != DirectionOutbound) {
		return false
	}
	switch refKind {
	case "meeting":
		return (role == RoleAttendee || role == RoleOrganizer) && (direction == DirectionOutbound || !largeInbound)
	case "message":
		if direction == DirectionInbound {
			return role == RoleSender && !largeInbound
		}
		return role == RoleAddressed || role == RoleMember
	default:
		return false
	}
}

// countedMessageTypeSQL excludes call logs and other non-message records from
// message contact while retaining the shared email and chat classification.
func countedMessageTypeSQL(message, conversation string) string {
	return `(COALESCE(` + message + `.message_type,'') IN ('','` + MessageTypeEmail + `') OR ` + identityindex.IsChatSQL(message+".message_type", conversation+".conversation_type") + `)`
}
