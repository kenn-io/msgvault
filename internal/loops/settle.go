package loops

import (
	"fmt"
	"time"
)

type ownerParticipationEvidence struct {
	MessageID int64
	Known     bool
}

func (t *traversal) settle(v ConversationView, live LiveLoop, now time.Time, cfg Thresholds) (decision *CloseDecision, gap bool, retained *LoopMessage, ownerParticipation ownerParticipationEvidence, err error) {
	ownerParticipation = ownerParticipationEvidence{
		MessageID: live.OwnerParticipationMessageID,
		Known:     live.OwnerParticipationKnown || live.OwnerParticipationMessageID != 0,
	}
	if v.AnchorExists != nil {
		exists, lookupErr := v.AnchorExists(live.AnchorMessageID)
		if lookupErr != nil {
			return nil, false, nil, ownerParticipation, fmt.Errorf("check loop anchor: %w", lookupErr)
		}
		if !exists {
			return &CloseDecision{ID: live.ID, Reason: "withdrawn", At: now}, false, nil, ownerParticipation, nil
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
		return nil, true, nil, ownerParticipation, nil
	}
	var side Direction
	switch live.Kind {
	case ReplyOwed:
		side = Inbound
	case AwaitingReply:
		side = Outbound
	default:
		return nil, false, nil, ownerParticipation, fmt.Errorf("unsupported live loop kind %q", live.Kind)
	}
	if live.Kind == AwaitingReply && v.ConversationType == "group_chat" && nonOwnerCount(v.Participants) > cfg.MaxGroupCounterparts {
		return &CloseDecision{ID: live.ID, Reason: "withdrawn", At: now}, false, nil, ownerParticipation, nil
	}
	// Corrected automation or ownership metadata can disqualify the anchor
	// itself. Neutral messages after an eligible anchor do not.
	anchorMessage := v.Messages[anchor]
	if !metadataSubstantive(anchorMessage) || anchorMessage.Direction != side {
		return &CloseDecision{ID: live.ID, Reason: "withdrawn", At: now}, false, nil, ownerParticipation, nil
	}
	if live.Kind == ReplyOwed && anchorMessage.ListID != "" {
		return &CloseDecision{ID: live.ID, Reason: "withdrawn", At: now}, false, nil, ownerParticipation, nil
	}
	bulkReplyAnchor := live.Kind == ReplyOwed && isBulkMessage(anchorMessage)
	priorOwnerParticipation := false
	ownerParticipationUncertain := false
	if bulkReplyAnchor && ownerParticipation.Known {
		participation := -1
		for i, message := range v.Messages {
			if message.ID == ownerParticipation.MessageID {
				participation = i
				break
			}
		}
		if participation >= 0 {
			message := v.Messages[participation]
			if participation < anchor && message.Direction == Outbound && t.substantive(v, participation) {
				priorOwnerParticipation = true
			} else {
				ownerParticipation = ownerParticipationEvidence{}
			}
		} else if v.AnchorExists == nil {
			// A pure detector may not have an archive lookup. Keep the persisted
			// proof, but report that its current eligibility is unknown.
			priorOwnerParticipation = true
			ownerParticipationUncertain = true
		} else {
			exists, lookupErr := v.AnchorExists(ownerParticipation.MessageID)
			if lookupErr != nil {
				return nil, false, nil, ownerParticipation, fmt.Errorf("check owner participation message: %w", lookupErr)
			}
			if exists {
				// The current window cannot validate ownership or MIME eligibility
				// for this older message. Preserve its proof until it is available.
				priorOwnerParticipation = true
				ownerParticipationUncertain = true
			} else {
				ownerParticipation = ownerParticipationEvidence{}
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
				ownerParticipation = ownerParticipationEvidence{MessageID: v.Messages[i].ID, Known: true}
			}
			break
		}
	}
	large := v.ConversationType == "group_chat" && nonOwnerCount(v.Participants) > cfg.MaxGroupCounterparts
	if _, ok := obligationCounterparty(v, live.Kind, anchorMessage); !ok {
		return &CloseDecision{ID: live.ID, Reason: "withdrawn", At: now}, false, nil, ownerParticipation, nil
	}
	if live.Kind == ReplyOwed && !addressedToOwner(anchorMessage, v.ConversationType, large) {
		return &CloseDecision{ID: live.ID, Reason: "withdrawn", At: now}, false, nil, ownerParticipation, nil
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
			return &CloseDecision{ID: live.ID, Reason: "withdrawn", At: now}, false, nil, ownerParticipation, nil
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
		return &CloseDecision{ID: live.ID, Reason: "turn_flip", At: m.OccurredAt, MessageID: m.ID}, false, nil, ownerParticipation, nil
	}
	if bulkReplyAnchor && !priorOwnerParticipation {
		// A bounded metadata window cannot prove owner participation. Keep the
		// anchor available to the detector, which requires durable participation
		// evidence before allowing the bulk message to remain a reply owed.
		return nil, true, &v.Messages[anchor], ownerParticipation, nil
	}
	if ownerParticipationUncertain {
		return nil, true, &v.Messages[anchor], ownerParticipation, nil
	}
	if anchorUnreadable {
		return nil, true, &v.Messages[anchor], ownerParticipation, nil
	}
	return nil, false, &v.Messages[anchor], ownerParticipation, nil
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
