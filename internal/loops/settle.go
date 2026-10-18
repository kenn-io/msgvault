package loops

import (
	"fmt"
	"time"
)

func (t *traversal) settle(v ConversationView, live LiveLoop, now time.Time, cfg Thresholds) (decision *CloseDecision, gap bool, retained *LoopMessage, err error) {
	if v.AnchorExists != nil {
		exists, lookupErr := v.AnchorExists(live.AnchorMessageID)
		if lookupErr != nil {
			return nil, false, nil, fmt.Errorf("check loop anchor: %w", lookupErr)
		}
		if !exists {
			return &CloseDecision{ID: live.ID, Reason: "withdrawn", At: now}, false, nil, nil
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
		return nil, true, nil, nil
	}
	var side Direction
	switch live.Kind {
	case ReplyOwed:
		side = Inbound
	case AwaitingReply:
		side = Outbound
	default:
		return nil, false, nil, fmt.Errorf("unsupported live loop kind %q", live.Kind)
	}
	if live.Kind == AwaitingReply && v.ConversationType == "group_chat" && nonOwnerCount(v.Participants) > cfg.MaxGroupCounterparts {
		return &CloseDecision{ID: live.ID, Reason: "withdrawn", At: now}, false, nil, nil
	}
	// Corrected automation or ownership metadata can disqualify the anchor
	// itself. Neutral messages after an eligible anchor do not.
	anchorMessage := v.Messages[anchor]
	if !metadataSubstantive(anchorMessage) || anchorMessage.Direction != side {
		return &CloseDecision{ID: live.ID, Reason: "withdrawn", At: now}, false, nil, nil
	}
	if live.Kind == ReplyOwed && anchorMessage.ListID != "" {
		return &CloseDecision{ID: live.ID, Reason: "withdrawn", At: now}, false, nil, nil
	}
	bulkReplyAnchor := live.Kind == ReplyOwed && isBulkMessage(anchorMessage)
	priorOwnerParticipation := false
	if bulkReplyAnchor {
		for i := anchor - 1; i >= 0; i-- {
			if !t.substantive(v, i) {
				continue
			}
			priorOwnerParticipation = v.Messages[i].Direction == Outbound
			break
		}
	}
	if _, ok := obligationCounterparty(v, live.Kind, anchorMessage); !ok {
		return &CloseDecision{ID: live.ID, Reason: "withdrawn", At: now}, false, nil, nil
	}
	anchorUnreadable := false
	switch live.Kind {
	case ReplyOwed:
		large := v.ConversationType == "group_chat" && nonOwnerCount(v.Participants) > cfg.MaxGroupCounterparts
		if !addressedToOwner(anchorMessage, v.ConversationType, large) {
			return &CloseDecision{ID: live.ID, Reason: "withdrawn", At: now}, false, nil, nil
		}
	case AwaitingReply:
		body := t.body(v, anchorMessage.ID)
		if !body.ok {
			// The shared budget may have been consumed by newer messages. Keep
			// checking for a conclusive opposing turn before treating this pass as
			// uncertain; a later inbound answer still settles the live obligation.
			anchorUnreadable = true
		} else if !asksQuestion(body.text) || !hasAwaitingReplyCounterparty(v, anchorMessage) {
			return &CloseDecision{ID: live.ID, Reason: "withdrawn", At: now}, false, nil, nil
		}
	}
	laterSameSideActivity := false
	for i := anchor + 1; i < len(v.Messages); i++ {
		m := v.Messages[i]
		if m.Direction == side {
			if bulkReplyAnchor && !laterSameSideActivity {
				laterSameSideActivity = t.substantive(v, i)
			}
			continue
		}
		if !t.substantive(v, i) {
			continue
		}
		return &CloseDecision{ID: live.ID, Reason: "turn_flip", At: m.OccurredAt, MessageID: m.ID}, false, nil, nil
	}
	if bulkReplyAnchor && !priorOwnerParticipation && !laterSameSideActivity {
		return &CloseDecision{ID: live.ID, Reason: "withdrawn", At: now}, false, nil, nil
	}
	if anchorUnreadable {
		return nil, true, &v.Messages[anchor], nil
	}
	return nil, false, &v.Messages[anchor], nil
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
