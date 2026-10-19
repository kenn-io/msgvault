package loops

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

type ownerParticipationEvidence struct {
	MessageID int64
	Known     bool
}

func (t *traversal) settle(v ConversationView, live *LiveLoop, now time.Time, cfg Thresholds) (decision *CloseDecision, gap bool, retained *LoopMessage, ownerParticipation ownerParticipationEvidence, err error) {
	ownerParticipation = ownerParticipationEvidence{
		MessageID: live.OwnerParticipationMessageID,
		Known:     live.OwnerParticipationKnown || live.OwnerParticipationMessageID != 0,
	}
	ownerParticipationUnknown := live.OwnerParticipationUnknown
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
	if bulkReplyAnchor && ownerParticipation.Known {
		participation := slices.IndexFunc(v.Messages, func(message LoopMessage) bool {
			return message.ID == ownerParticipation.MessageID
		})
		if participation >= 0 {
			message := v.Messages[participation]
			if participation < anchor && message.Direction == Outbound && t.substantive(v, participation) {
				priorOwnerParticipation = true
				if v.CheckOwnerParticipation != nil {
					valid, known, checkErr := checkOwnerParticipation(v, live.AnchorMessageID, ownerParticipation.MessageID)
					if checkErr != nil {
						return nil, false, nil, ownerParticipation, fmt.Errorf("revalidate loop owner participation: %w", checkErr)
					}
					if !known {
						ownerParticipationUnknown = true
						priorOwnerParticipation = false
					} else if !valid {
						ownerParticipation = ownerParticipationEvidence{}
						ownerParticipationUnknown = false
						priorOwnerParticipation = false
					} else {
						ownerParticipationUnknown = false
					}
				} else {
					ownerParticipationUnknown = false
				}
			} else {
				ownerParticipation = ownerParticipationEvidence{}
				ownerParticipationUnknown = false
			}
		} else if v.CheckOwnerParticipation != nil {
			valid, known, checkErr := checkOwnerParticipation(v, live.AnchorMessageID, ownerParticipation.MessageID)
			if checkErr != nil {
				return nil, false, nil, ownerParticipation, fmt.Errorf("revalidate loop owner participation: %w", checkErr)
			}
			switch {
			case !known:
				ownerParticipationUnknown = true
			case !valid:
				ownerParticipation = ownerParticipationEvidence{}
				ownerParticipationUnknown = false
			default:
				priorOwnerParticipation = true
				ownerParticipationUnknown = false
			}
		} else if v.AnchorExists == nil {
			// A pure detector may not have an archive lookup. Keep the persisted
			// proof, but report that its current eligibility is unknown.
			priorOwnerParticipation = true
			ownerParticipationUnknown = true
		} else {
			exists, lookupErr := v.AnchorExists(ownerParticipation.MessageID)
			if lookupErr != nil {
				return nil, false, nil, ownerParticipation, fmt.Errorf("check owner participation message: %w", lookupErr)
			}
			if exists {
				// The current window cannot validate ownership or MIME eligibility
				// for this older message. Preserve its proof until it is available.
				priorOwnerParticipation = true
				ownerParticipationUnknown = true
			} else {
				ownerParticipation = ownerParticipationEvidence{}
				ownerParticipationUnknown = false
			}
		}
	}
	if bulkReplyAnchor && !priorOwnerParticipation {
		for i := anchor - 1; i >= 0; i-- {
			if !t.substantive(v, i) {
				continue
			}
			if v.Messages[i].Direction != Outbound {
				break
			}
			if v.CheckOwnerParticipation != nil {
				valid, known, checkErr := checkOwnerParticipation(v, live.AnchorMessageID, v.Messages[i].ID)
				if checkErr != nil {
					return nil, false, nil, ownerParticipation, fmt.Errorf("validate loop owner participation: %w", checkErr)
				}
				if !known {
					ownerParticipationUnknown = true
					if !ownerParticipation.Known {
						ownerParticipation = ownerParticipationEvidence{MessageID: v.Messages[i].ID, Known: true}
					}
					continue
				}
				if !valid {
					continue
				}
			}
			priorOwnerParticipation = true
			ownerParticipationUnknown = false
			ownerParticipation = ownerParticipationEvidence{MessageID: v.Messages[i].ID, Known: true}
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
		if !body.ok || strings.TrimSpace(body.text) == "" {
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
		if ownerParticipationUnknown || (anchor == 0 && !v.HistoryComplete) {
			// The bounded window cannot prove whether earlier owner participation
			// exists. Retain the row for retry without treating it as evidence.
			live.OwnerParticipationMessageID = ownerParticipation.MessageID
			live.OwnerParticipationKnown = ownerParticipation.Known
			live.OwnerParticipationUnknown = true
			return nil, true, &v.Messages[anchor], ownerParticipation, nil
		}
		// Another inbound bulk message cannot establish owner participation.
		// With complete coverage, or an earlier turn boundary in this window,
		// the anchor is known to lack the evidence required for eligibility.
		live.OwnerParticipationMessageID = 0
		live.OwnerParticipationKnown = false
		live.OwnerParticipationUnknown = false
		return &CloseDecision{ID: live.ID, Reason: "withdrawn", At: now}, false, &v.Messages[anchor], ownerParticipationEvidence{}, nil
	}
	if anchorUnreadable {
		live.OwnerParticipationMessageID = ownerParticipation.MessageID
		live.OwnerParticipationKnown = ownerParticipation.Known
		live.OwnerParticipationUnknown = false
		return nil, true, &v.Messages[anchor], ownerParticipation, nil
	}
	live.OwnerParticipationMessageID = ownerParticipation.MessageID
	live.OwnerParticipationKnown = ownerParticipation.Known
	live.OwnerParticipationUnknown = false
	return nil, false, &v.Messages[anchor], ownerParticipation, nil
}

func checkOwnerParticipation(v ConversationView, anchorMessageID, participationMessageID int64) (valid, known bool, err error) {
	if v.CheckOwnerParticipation == nil {
		// Hand-built detector views have no Store lookup. Their persisted IDs stay
		// authoritative, matching the older direct-call contract.
		return true, true, nil
	}
	return v.CheckOwnerParticipation(anchorMessageID, participationMessageID)
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
