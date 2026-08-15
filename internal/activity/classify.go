package activity

import (
	"slices"

	"go.kenn.io/msgvault/internal/store"
)

const (
	recipientTypeFrom   = "from"
	recipientTypeMember = "member"
)

// DefaultMaxDirectCounterparts is the largest outbound audience that counts
// as direct contact. Larger sends remain visible activity but are classified
// as co-presence rather than a personal interaction with every recipient.
const DefaultMaxDirectCounterparts = 25

// Classification is the owner-relative interpretation of one archived native
// event. Every person link is already collapsed to its strongest evidence and
// deterministic representative role.
type Classification struct {
	RefKind       store.ActivityRefKind
	Channel       store.ActivityChannel
	Direction     store.ActivityDirection
	OwnerSourceID *int64
	OwnerAddress  string
	Persons       []store.ActivityEventPerson
}

// Classify applies the same pure rules to incremental and backstop projection.
func Classify(candidate store.ActivityCandidate, maxDirectCounterparts int) Classification {
	if maxDirectCounterparts <= 0 {
		maxDirectCounterparts = DefaultMaxDirectCounterparts
	}
	counterparts := collapseCounterparts(candidate.Counterparts)

	meeting := store.IsMeetingMessageType(candidate.MessageType)
	result := Classification{
		RefKind: store.RefKindMessage,
		Channel: channelFor(candidate.ConversationType, meeting),
	}
	if meeting {
		result.RefKind = store.RefKindMeeting
	}

	// Every distinct 'from' counterpart is a sender: recipient storage keeps
	// one row per authoring participant, so multiple authors can co-sign one
	// message. Ownership of ANY author makes the message outbound; inbound
	// requires the owner to appear exclusively among non-authors. Counterpart
	// order is a SQL sort key, so direction must not depend on which author
	// row happens to come first. Source-native ownership is authoritative on
	// its own: a source that knows the owner sent this message outranks
	// counterpart resolution, which has no author row at all when the sender
	// participant is unresolved.
	owningSenderIndex := -1
	for index, counterpart := range counterparts {
		if counterpart.RecipientType == recipientTypeFrom && counterpart.IsOwner {
			owningSenderIndex = index
			break
		}
	}

	switch {
	case candidate.SourceIsFromMe || owningSenderIndex >= 0:
		result.Direction = store.DirectionOutbound
		if owningSenderIndex >= 0 {
			result.OwnerAddress = counterparts[owningSenderIndex].OwnerAddress
		}
	case ownerAmongNonSenders(counterparts):
		result.Direction = store.DirectionInbound
		result.OwnerAddress = firstNonSenderOwnerAddress(counterparts)
	default:
		result.Direction = store.DirectionObserved
	}
	if result.Direction != store.DirectionObserved && candidate.SourceID > 0 {
		sourceID := candidate.SourceID
		result.OwnerSourceID = &sourceID
	}

	// Ownership is row-scoped above because direction is envelope-authoritative
	// per role, but audience counting and person links are owner-relative: one
	// participant can be an owner 'from' row AND a non-owner 'member' row (a
	// source-native chat sender in conversation_participants with no matching
	// account identity). Counting that row toward the broadcast threshold or
	// emitting it as contact evidence would create direct activity between the
	// owner and their own person, so ownership collapses across every role of
	// the same participant — and across every participant of the same person.
	ownerParticipants := make(map[int64]struct{}, len(counterparts))
	ownerPersons := make(map[int64]struct{}, len(counterparts))
	for _, counterpart := range counterparts {
		if !counterpart.IsOwner {
			continue
		}
		ownerParticipants[counterpart.ParticipantID] = struct{}{}
		if counterpart.PersonID != nil {
			ownerPersons[*counterpart.PersonID] = struct{}{}
		}
	}
	ownerLinked := func(counterpart store.ActivityCounterpart) bool {
		if counterpart.IsOwner {
			return true
		}
		if _, owned := ownerParticipants[counterpart.ParticipantID]; owned {
			return true
		}
		if counterpart.PersonID == nil {
			return false
		}
		_, owned := ownerPersons[*counterpart.PersonID]
		return owned
	}

	// The audience is person-relative: several alias participants of one
	// curated person are ONE counterpart, matching the person-link collapse
	// below — otherwise an alias-heavy direct message could cross the
	// broadcast threshold and demote real contact to co-presence. Unlinked
	// participants count individually.
	type audienceKey struct {
		person bool
		id     int64
	}
	nonOwnerIDs := make(map[audienceKey]struct{}, len(counterparts))
	for _, counterpart := range counterparts {
		if ownerLinked(counterpart) {
			continue
		}
		key := audienceKey{id: counterpart.ParticipantID}
		if counterpart.PersonID != nil {
			key = audienceKey{person: true, id: *counterpart.PersonID}
		}
		nonOwnerIDs[key] = struct{}{}
	}
	broadcast := len(nonOwnerIDs) > maxDirectCounterparts

	for _, counterpart := range counterparts {
		if ownerLinked(counterpart) || counterpart.PersonID == nil {
			continue
		}
		isSender := counterpart.RecipientType == recipientTypeFrom
		result.Persons = append(result.Persons, store.ActivityEventPerson{
			PersonID: *counterpart.PersonID,
			Role:     activityRole(counterpart.RecipientType, isSender, meeting),
			Evidence: activityEvidence(result.Direction, isSender, broadcast),
		})
	}
	result.Persons = strongestPersonLinks(result.Persons)
	return result
}

// collapseCounterparts removes duplicate envelope rows for one native
// participant/role before direction and audience rules run. Recipient storage
// deliberately preserves separate envelope aliases for identity discovery;
// activity is person-relative and must count that participant once. Owner
// evidence is merged with OR semantics so SQL row order cannot reverse the
// message direction when one duplicate alias is confirmed for the source.
func collapseCounterparts(counterparts []store.ActivityCounterpart) []store.ActivityCounterpart {
	if len(counterparts) < 2 {
		return counterparts
	}
	type key struct {
		participantID int64
		recipientType string
	}
	seen := make(map[key]int, len(counterparts))
	result := make([]store.ActivityCounterpart, 0, len(counterparts))
	for _, counterpart := range counterparts {
		k := key{participantID: counterpart.ParticipantID, recipientType: counterpart.RecipientType}
		index, found := seen[k]
		if !found {
			seen[k] = len(result)
			result = append(result, counterpart)
			continue
		}
		current := &result[index]
		if current.PersonID == nil && counterpart.PersonID != nil {
			personID := *counterpart.PersonID
			current.PersonID = &personID
		}
		if counterpart.IsOwner {
			current.IsOwner = true
			if current.OwnerAddress == "" ||
				(counterpart.OwnerAddress != "" && counterpart.OwnerAddress < current.OwnerAddress) {
				current.OwnerAddress = counterpart.OwnerAddress
			}
		}
	}
	return result
}

func channelFor(conversationType string, meeting bool) store.ActivityChannel {
	if meeting {
		return store.ChannelMeeting
	}
	switch conversationType {
	case "email_thread":
		return store.ChannelEmail
	case "group_chat", "direct_chat", "channel":
		return store.ChannelChat
	default:
		return store.ChannelOther
	}
}

func ownerAmongNonSenders(counterparts []store.ActivityCounterpart) bool {
	for _, counterpart := range counterparts {
		if counterpart.RecipientType != recipientTypeFrom && counterpart.IsOwner {
			return true
		}
	}
	return false
}

func firstNonSenderOwnerAddress(counterparts []store.ActivityCounterpart) string {
	for _, counterpart := range counterparts {
		if counterpart.RecipientType != recipientTypeFrom && counterpart.IsOwner {
			return counterpart.OwnerAddress
		}
	}
	return ""
}

func activityRole(recipientType string, sender, meeting bool) store.ActivityRole {
	switch {
	case sender && meeting:
		return store.RoleOrganizer
	case sender:
		return store.RoleSender
	case recipientType == recipientTypeMember:
		return store.RoleMember
	case meeting:
		return store.RoleAttendee
	default:
		return store.RoleAddressed
	}
}

func activityEvidence(
	direction store.ActivityDirection,
	sender bool,
	broadcast bool,
) store.ActivityEvidence {
	switch direction {
	case store.DirectionOutbound:
		if !broadcast {
			return store.EvidenceDirect
		}
	case store.DirectionInbound:
		if sender {
			return store.EvidenceDirect
		}
	case store.DirectionObserved:
	}
	return store.EvidenceCoPresence
}

func strongestPersonLinks(links []store.ActivityEventPerson) []store.ActivityEventPerson {
	if len(links) == 0 {
		return nil
	}

	strongest := make(map[int64]store.ActivityEventPerson, len(links))
	for _, link := range links {
		current, found := strongest[link.PersonID]
		if !found || strongerActivityLink(link, current) {
			strongest[link.PersonID] = link
		}
	}

	result := make([]store.ActivityEventPerson, 0, len(strongest))
	for _, link := range strongest {
		result = append(result, link)
	}
	slices.SortFunc(result, func(left, right store.ActivityEventPerson) int {
		switch {
		case left.PersonID < right.PersonID:
			return -1
		case left.PersonID > right.PersonID:
			return 1
		default:
			return 0
		}
	})
	return result
}

func strongerActivityLink(candidate, current store.ActivityEventPerson) bool {
	candidateEvidence := activityEvidencePriority(candidate.Evidence)
	currentEvidence := activityEvidencePriority(current.Evidence)
	if candidateEvidence != currentEvidence {
		return candidateEvidence < currentEvidence
	}
	return activityRolePriority(candidate.Role) < activityRolePriority(current.Role)
}

func activityEvidencePriority(evidence store.ActivityEvidence) int {
	if evidence == store.EvidenceDirect {
		return 0
	}
	return 1
}

func activityRolePriority(role store.ActivityRole) int {
	switch role {
	case store.RoleSender:
		return 0
	case store.RoleOrganizer:
		return 1
	case store.RoleAddressed:
		return 2
	case store.RoleAttendee:
		return 3
	case store.RoleMember:
		return 4
	default:
		return 5
	}
}
