package loops

import (
	"fmt"
	"time"
)

func (t *traversal) settle(v ConversationView, live LiveLoop, now time.Time, cfg Thresholds) (decision *CloseDecision, gap bool, err error) {
	if v.AnchorExists != nil {
		exists, lookupErr := v.AnchorExists(live.AnchorMessageID)
		if lookupErr != nil {
			return nil, false, fmt.Errorf("check loop anchor: %w", lookupErr)
		}
		if !exists {
			return &CloseDecision{ID: live.ID, Reason: "withdrawn", At: now}, false, nil
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
		return nil, true, nil
	}
	side := Inbound
	if live.Kind == AwaitingReply {
		side = Outbound
	}
	// Corrected automation or ownership metadata can disqualify the anchor
	// itself. Neutral messages after an eligible anchor do not.
	if !metadataSubstantive(v.Messages[anchor]) || v.Messages[anchor].Direction != side {
		return &CloseDecision{ID: live.ID, Reason: "withdrawn", At: now}, false, nil
	}
	anchorUnreadable := false
	if live.Kind == ReplyOwed {
		large := v.ConversationType == "group_chat" && nonOwnerCount(v.Participants) > cfg.MaxGroupCounterparts
		if !addressedToOwner(v.Messages[anchor], v.ConversationType, large) {
			return &CloseDecision{ID: live.ID, Reason: "withdrawn", At: now}, false, nil
		}
	} else if live.Kind == AwaitingReply {
		body := t.body(v, v.Messages[anchor].ID)
		if !body.ok {
			anchorUnreadable = true
		} else if !asksQuestion(body.text) {
			return &CloseDecision{ID: live.ID, Reason: "withdrawn", At: now}, false, nil
		}
	}
	for i := anchor + 1; i < len(v.Messages); i++ {
		m := v.Messages[i]
		if m.Direction == side || !t.substantive(v, i) {
			continue
		}
		return &CloseDecision{ID: live.ID, Reason: "turn_flip", At: m.OccurredAt, MessageID: m.ID}, false, nil
	}
	if anchorUnreadable {
		return nil, true, nil
	}
	return nil, false, nil
}
