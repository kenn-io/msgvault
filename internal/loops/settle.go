package loops

import (
	"fmt"
	"slices"
	"time"
)

func (t *traversal) settle(v ConversationView, live LiveLoop, now time.Time, cfg Thresholds) (decision *CloseDecision, gap bool, err error) {
	if v.AnchorExists != nil {
		exists, lookupErr := t.checkAnchor(v, live.AnchorMessageID)
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
	var side Direction
	switch live.Kind {
	case ReplyOwed:
		side = Inbound
	case AwaitingReply:
		side = Outbound
	default:
		return nil, false, fmt.Errorf("unsupported live loop kind %q", live.Kind)
	}
	if live.Kind == AwaitingReply && v.ConversationType == "group_chat" && nonOwnerCount(v.Participants) > cfg.MaxGroupCounterparts {
		return &CloseDecision{ID: live.ID, Reason: "withdrawn", At: now}, false, nil
	}
	// Corrected automation or ownership metadata can disqualify the anchor
	// itself. Neutral messages after an eligible anchor do not.
	if !metadataEligible(v.Messages[anchor]) || v.Messages[anchor].Direction != side {
		return &CloseDecision{ID: live.ID, Reason: "withdrawn", At: now}, false, nil
	}
	if _, ok := obligationCounterparty(v, live.Kind, v.Messages[anchor]); !ok {
		return &CloseDecision{ID: live.ID, Reason: "withdrawn", At: now}, false, nil
	}
	// List mail cannot create a reply obligation, even when corrected metadata
	// arrives after the anchor has fallen outside the recent detection window.
	if live.Kind == ReplyOwed && v.Messages[anchor].ListID != "" {
		return &CloseDecision{ID: live.ID, Reason: "withdrawn", At: now}, false, nil
	}
	anchorUnreadable := false
	switch live.Kind {
	case ReplyOwed:
		large := v.ConversationType == "group_chat" && nonOwnerCount(v.Participants) > cfg.MaxGroupCounterparts
		if !addressedToOwner(v.Messages[anchor], v.ConversationType, large) {
			return &CloseDecision{ID: live.ID, Reason: "withdrawn", At: now}, false, nil
		}
	case AwaitingReply:
		body := t.body(v, v.Messages[anchor].ID)
		if !body.ok {
			anchorUnreadable = true
		} else if !asksQuestion(body.text) {
			return &CloseDecision{ID: live.ID, Reason: "withdrawn", At: now}, false, nil
		}
	}
	if t.hasUndatedSubstantive(v) {
		return nil, true, nil
	}
	uncertainAcknowledgement := false
	for i := anchor + 1; i < len(v.Messages); i++ {
		m := v.Messages[i]
		if m.Direction == side {
			continue
		}
		if live.Kind == AwaitingReply && m.Direction == Inbound && isListOrBulkMail(m) {
			continue
		}
		if !t.substantive(v, i) {
			uncertainAcknowledgement = uncertainAcknowledgement || t.uncertainAcks[m.ID]
			continue
		}
		return &CloseDecision{ID: live.ID, Reason: "turn_flip", At: m.OccurredAt, MessageID: m.ID}, false, nil
	}
	if anchorUnreadable || uncertainAcknowledgement {
		return nil, true, nil
	}
	return nil, false, nil
}

func isListOrBulkMail(m LoopMessage) bool {
	return m.ListID != "" || isBulkMail(m)
}

func isBulkMail(m LoopMessage) bool {
	if hasBulkCategory(m) {
		return true
	}
	return m.Signals != nil && (m.Signals.PrecedenceBulk || m.Signals.ListUnsubscribe)
}

func hasBulkCategory(m LoopMessage) bool {
	return slices.ContainsFunc(m.Labels, isBulkCategoryLabel)
}

func isBulkCategoryLabel(label string) bool {
	switch label {
	case "CATEGORY_PROMOTIONS", "CATEGORY_SOCIAL", "CATEGORY_FORUMS":
		return true
	default:
		return false
	}
}
