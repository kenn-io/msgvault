package store

import "slices"

const DefaultMaxDirectActivityCounterparts = 25

// ActivityClassification is the owner-relative interpretation of one native
// activity candidate. It lives in store so identity mutations can rebuild
// affected rows inside their authoritative transaction.
type ActivityClassification struct {
	RefKind       ActivityRefKind
	Channel       ActivityChannel
	Direction     ActivityDirection
	OwnerSourceID *int64
	OwnerAddress  string
	Persons       []ActivityEventPerson
}

// ClassifyActivityCandidate applies the same pure rules used by the external
// activity projector. Keeping one implementation prevents inline identity
// repairs from drifting from incremental and backstop projection.
func ClassifyActivityCandidate(
	candidate ActivityCandidate, maxDirectCounterparts int,
) ActivityClassification {
	if maxDirectCounterparts <= 0 {
		maxDirectCounterparts = DefaultMaxDirectActivityCounterparts
	}
	counterparts := collapseActivityCounterparts(candidate.Counterparts)
	meeting := IsMeetingMessageType(candidate.MessageType)
	result := ActivityClassification{
		RefKind: RefKindMessage,
		Channel: activityChannelFor(candidate.ConversationType, meeting),
	}
	if meeting {
		result.RefKind = RefKindMeeting
	}

	owningSenderIndex := -1
	for index, counterpart := range counterparts {
		if counterpart.RecipientType == "from" && counterpart.IsOwner {
			owningSenderIndex = index
			break
		}
	}
	switch {
	case candidate.SourceIsFromMe || owningSenderIndex >= 0:
		result.Direction = DirectionOutbound
		if owningSenderIndex >= 0 {
			result.OwnerAddress = counterparts[owningSenderIndex].OwnerAddress
		}
	case activityOwnerAmongNonSenders(counterparts):
		result.Direction = DirectionInbound
		result.OwnerAddress = firstActivityNonSenderOwnerAddress(counterparts)
	default:
		result.Direction = DirectionObserved
	}
	if result.Direction != DirectionObserved && candidate.SourceID > 0 {
		sourceID := candidate.SourceID
		result.OwnerSourceID = &sourceID
	}

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
	ownerLinked := func(counterpart ActivityCounterpart) bool {
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
		isSender := counterpart.RecipientType == "from"
		result.Persons = append(result.Persons, ActivityEventPerson{
			PersonID: *counterpart.PersonID,
			Role:     classifiedActivityRole(counterpart.RecipientType, isSender, meeting),
			Evidence: classifiedActivityEvidence(result.Direction, isSender, broadcast),
		})
	}
	result.Persons = strongestClassifiedActivityLinks(result.Persons)
	return result
}

func collapseActivityCounterparts(counterparts []ActivityCounterpart) []ActivityCounterpart {
	if len(counterparts) < 2 {
		return counterparts
	}
	type key struct {
		participantID int64
		recipientType string
	}
	seen := make(map[key]int, len(counterparts))
	result := make([]ActivityCounterpart, 0, len(counterparts))
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

func activityChannelFor(conversationType string, meeting bool) ActivityChannel {
	if meeting {
		return ChannelMeeting
	}
	switch conversationType {
	case "email_thread":
		return ChannelEmail
	case "chat", "group_chat", "direct_chat", "channel":
		return ChannelChat
	default:
		return ChannelOther
	}
}

func activityOwnerAmongNonSenders(counterparts []ActivityCounterpart) bool {
	for _, counterpart := range counterparts {
		if counterpart.RecipientType != "from" && counterpart.IsOwner {
			return true
		}
	}
	return false
}

func firstActivityNonSenderOwnerAddress(counterparts []ActivityCounterpart) string {
	for _, counterpart := range counterparts {
		if counterpart.RecipientType != "from" && counterpart.IsOwner {
			return counterpart.OwnerAddress
		}
	}
	return ""
}

func classifiedActivityRole(recipientType string, sender, meeting bool) ActivityRole {
	switch {
	case sender && meeting:
		return RoleOrganizer
	case sender:
		return RoleSender
	case recipientType == "member":
		return RoleMember
	case meeting:
		return RoleAttendee
	default:
		return RoleAddressed
	}
}

func classifiedActivityEvidence(
	direction ActivityDirection, sender, broadcast bool,
) ActivityEvidence {
	switch direction {
	case DirectionOutbound:
		if !broadcast {
			return EvidenceDirect
		}
	case DirectionInbound:
		if sender {
			return EvidenceDirect
		}
	case DirectionObserved:
	}
	return EvidenceCoPresence
}

func strongestClassifiedActivityLinks(links []ActivityEventPerson) []ActivityEventPerson {
	strongest := make(map[int64]ActivityEventPerson, len(links))
	for _, link := range links {
		current, found := strongest[link.PersonID]
		if !found || strongerClassifiedActivityLink(link, current) {
			strongest[link.PersonID] = link
		}
	}
	result := make([]ActivityEventPerson, 0, len(strongest))
	for _, link := range strongest {
		result = append(result, link)
	}
	slices.SortFunc(result, func(left, right ActivityEventPerson) int {
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

func strongerClassifiedActivityLink(candidate, current ActivityEventPerson) bool {
	if candidate.Evidence != current.Evidence {
		return candidate.Evidence == EvidenceDirect
	}
	return classifiedActivityRolePriority(candidate.Role) <
		classifiedActivityRolePriority(current.Role)
}

func classifiedActivityRolePriority(role ActivityRole) int {
	switch role {
	case RoleSender:
		return 0
	case RoleOrganizer:
		return 1
	case RoleAddressed:
		return 2
	case RoleAttendee:
		return 3
	case RoleMember:
		return 4
	default:
		return 5
	}
}
