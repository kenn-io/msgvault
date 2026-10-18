package loops

import (
	"fmt"
	"time"
)

func (t *traversal) settle(v ConversationView, live LiveLoop, now time.Time, cfg Thresholds) (decision *CloseDecision, gap bool, retained *LoopMessage, ownerParticipationMessageID int64, err error) {
	ownerParticipationMessageID = live.OwnerParticipationMessageID
	if v.AnchorExists != nil {
		exists, lookupErr := v.AnchorExists(live.AnchorMessageID)
		if lookupErr != nil {
			return nil, false, nil, ownerParticipationMessageID, fmt.Errorf("check loop anchor: %w", lookupErr)
		}
		if !exists {
			return &CloseDecision{ID: live.ID, Reason: "withdrawn", At: now}, false, nil, ownerParticipationMessageID, nil
		}
	}
	anchor := -1
	for i, m := range v.Messages {
		if m.ID == live.AnchorMessageID {
			anchor = i
			break
		}
	}
	if anchor < 0 {
		return nil, true, nil, ownerParticipationMessageID, nil
	}
	var side Direction
	switch live.Kind {
	case ReplyOwed:
		side = Inbound
	case AwaitingReply:
		side = Outbound
	default:
		return nil, false, nil, ownerParticipationMessageID, fmt.Errorf("unsupported live loop kind %q", live.Kind)
	}
	if live.Kind == AwaitingReply && v.ConversationType == "group_chat" && nonOwnerCount(v.Participants) > cfg.MaxGroupCounterparts {
		return &CloseDecision{ID: live.ID, Reason: "withdrawn", At: now}, false, nil, ownerParticipationMessageID, nil
	}
	// Corrected automation or ownership metadata can disqualify the anchor
	// itself. Neutral messages after an eligible anchor do not.
	anchorMessage := v.Messages[anchor]
	if !metadataSubstantive(anchorMessage) || anchorMessage.Direction != side {
		return &CloseDecision{ID: live.ID, Reason: "withdrawn", At: now}, false, nil, ownerParticipationMessageID, nil
	}
	if live.Kind == ReplyOwed && anchorMessage.ListID != "" {
		return &CloseDecision{ID: live.ID, Reason: "withdrawn", At: now}, false, nil, ownerParticipationMessageID, nil
	}
	bulkReplyAnchor := live.Kind == ReplyOwed && isBulkMessage(anchorMessage)
	priorOwnerParticipation := false
	ownerParticipationUncertain := false
	if bulkReplyAnchor && ownerParticipationMessageID != 0 {
		participation := -1
		for i, message := range v.Messages {
			if message.ID == ownerParticipationMessageID {
				participation = i
				break
			}
		}
		if participation >= 0 {
			message := v.Messages[participation]
			if participation < anchor && message.Direction == Outbound && t.substantive(v, participation) {
				priorOwnerParticipation = true
			} else {
				ownerParticipationMessageID = 0
			}
		} else if v.AnchorExists == nil {
			// A pure detector may not have an archive lookup. Keep the persisted
			// proof, but report that its current eligibility is unknown.
			priorOwnerParticipation = true
			ownerParticipationUncertain = true
		} else {
			exists, lookupErr := v.AnchorExists(ownerParticipationMessageID)
			if lookupErr != nil {
				return nil, false, nil, ownerParticipationMessageID, fmt.Errorf("check owner participation message: %w", lookupErr)
			}
			if exists {
				// The current window cannot validate ownership or MIME eligibility
				// for this older message. Preserve its proof until it is available.
				priorOwnerParticipation = true
				ownerParticipationUncertain = true
			} else {
				ownerParticipationMessageID = 0
			}
		}
	}
	if bulkReplyAnchor && !priorOwnerParticipation {
		for i := anchor - 1; i >= 0; i-- {
			if !t.substantive(v, i) {
				continue
			}
			priorOwnerParticipation = v.Messages[i].Direction == Outbound
			if priorOwnerParticipation {
				ownerParticipationMessageID = v.Messages[i].ID
			}
			break
		}
	}
	large := v.ConversationType == "group_chat" && nonOwnerCount(v.Participants) > cfg.MaxGroupCounterparts
	if _, ok := obligationCounterparty(v, live.Kind, anchorMessage); !ok {
		return &CloseDecision{ID: live.ID, Reason: "withdrawn", At: now}, false, nil, ownerParticipationMessageID, nil
	}
	if live.Kind == ReplyOwed && !addressedToOwner(anchorMessage, v.ConversationType, large) {
		return &CloseDecision{ID: live.ID, Reason: "withdrawn", At: now}, false, nil, ownerParticipationMessageID, nil
	}
	anchorUnreadable := false
	switch live.Kind {
	case AwaitingReply:
		body := t.body(v, anchorMessage.ID)
		if !body.ok {
			// The shared budget may have been consumed by newer messages. Keep
			// checking for a conclusive opposing turn before treating this pass as
			// uncertain; a later inbound answer still settles the live obligation.
			anchorUnreadable = true
		} else if !asksQuestion(body.text) || large || !hasAwaitingReplyCounterparty(v, anchorMessage) {
			return &CloseDecision{ID: live.ID, Reason: "withdrawn", At: now}, false, nil, ownerParticipationMessageID, nil
		}
	}
	for i := anchor + 1; i < len(v.Messages); i++ {
		m := v.Messages[i]
		if m.Direction == side {
			continue
		}
		if !t.substantive(v, i) {
			continue
		}
		return &CloseDecision{ID: live.ID, Reason: "turn_flip", At: m.OccurredAt, MessageID: m.ID}, false, nil, ownerParticipationMessageID, nil
	}
	if bulkReplyAnchor && !priorOwnerParticipation {
		// A bounded metadata window cannot prove owner participation. Keep the
		// anchor available to the detector, which requires durable participation
		// evidence before allowing the bulk message to remain a reply owed.
		return nil, true, &v.Messages[anchor], ownerParticipationMessageID, nil
	}
	if ownerParticipationUncertain {
		return nil, true, &v.Messages[anchor], ownerParticipationMessageID, nil
	}
	if anchorUnreadable {
		return nil, true, &v.Messages[anchor], ownerParticipationMessageID, nil
	}
	return nil, false, &v.Messages[anchor], ownerParticipationMessageID, nil
}

func hasAwaitingReplyCounterparty(view ConversationView, anchor LoopMessage) bool {
	if view.ConversationType == "email_thread" {
		for _, recipient := range anchor.Recipients {
			if recipient.Type == "to" && !recipient.IsOwner {
				return true
			}
		}
		return false
	}
	for _, participant := range view.Participants {
		if !participant.IsOwner {
			return true
		}
	}
	return false
}
