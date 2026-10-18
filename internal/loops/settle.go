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
	if !metadataSubstantive(v.Messages[anchor]) || v.Messages[anchor].Direction != side {
		return &CloseDecision{ID: live.ID, Reason: "withdrawn", At: now}, false, nil, nil
	}
	if _, ok := obligationCounterparty(v, live.Kind, v.Messages[anchor]); !ok {
		return &CloseDecision{ID: live.ID, Reason: "withdrawn", At: now}, false, nil, nil
	}
	// List mail cannot create a reply obligation, even when corrected metadata
	// arrives after the anchor has fallen outside the recent detection window.
	if live.Kind == ReplyOwed && v.Messages[anchor].ListID != "" {
		return &CloseDecision{ID: live.ID, Reason: "withdrawn", At: now}, false, nil, nil
	}
	anchorUnreadable := false
	switch live.Kind {
	case ReplyOwed:
		large := v.ConversationType == "group_chat" && nonOwnerCount(v.Participants) > cfg.MaxGroupCounterparts
		if !addressedToOwner(v.Messages[anchor], v.ConversationType, large) {
			return &CloseDecision{ID: live.ID, Reason: "withdrawn", At: now}, false, nil, nil
		}
	case AwaitingReply:
		body := t.body(v, v.Messages[anchor].ID)
		if !body.ok {
			anchorUnreadable = true
		} else if !asksQuestion(body.text) || !hasAwaitingReplyCounterparty(v, v.Messages[anchor]) {
			return &CloseDecision{ID: live.ID, Reason: "withdrawn", At: now}, false, nil, nil
		}
	}
	for i := anchor + 1; i < len(v.Messages); i++ {
		m := v.Messages[i]
		if m.Direction == side || !t.substantive(v, i) {
			continue
		}
		return &CloseDecision{ID: live.ID, Reason: "turn_flip", At: m.OccurredAt, MessageID: m.ID}, false, nil, nil
	}
	if anchorUnreadable {
		return nil, true, nil, nil
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
