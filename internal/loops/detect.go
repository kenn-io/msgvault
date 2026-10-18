package loops

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Detect computes turn-state changes without writing archive or user state.
// Its one body budget is shared by newest-first detection and forward
// settlement. Unread owner text counts as an answer, never as a new question.
func Detect(view ConversationView, live []LiveLoop, now time.Time, cfg Thresholds, supp Suppressions) Verdict {
	v := Verdict{}
	closeLive := func(loop LiveLoop, reason string) {
		v.CoverageGaps = slices.DeleteFunc(v.CoverageGaps, func(id int64) bool { return id == loop.ID })
		if !slices.ContainsFunc(v.Close, func(c CloseDecision) bool { return c.ID == loop.ID }) {
			v.Close = append(v.Close, CloseDecision{ID: loop.ID, Reason: reason, At: now})
		}
	}
	closeKept := func(kind, reason string) {
		for _, l := range live {
			if (kind == "" || l.Kind == kind) && (l.Kind == ReplyOwed || l.Kind == AwaitingReply) && !slices.Contains(v.CoverageGaps, l.ID) {
				closeLive(l, reason)
			}
		}
	}
	keepUncertain := func(kind string) {
		for _, l := range live {
			if (kind == "" || l.Kind == kind) && (l.Kind == ReplyOwed || l.Kind == AwaitingReply) &&
				!slices.ContainsFunc(v.Close, func(c CloseDecision) bool { return c.ID == l.ID }) &&
				!slices.Contains(v.CoverageGaps, l.ID) {
				v.CoverageGaps = append(v.CoverageGaps, l.ID)
			}
		}
	}
	if slices.Contains(supp.Conversations, view.ConversationID) {
		closeKept("", "muted")
		return v
	}
	if !eligibleConversation(view, now, cfg) {
		closeKept("", "withdrawn")
		return v
	}
	t := traversal{limit: cfg.RunMessageLimit, bodies: make(map[int64]bodyResult)}
	turns := t.trailing(view)
	recentTurns := turns
	// Extend at most once even when several live anchors fall outside the
	// normal window. Continue using the same body cache and global budget.
	settlementView := view
	extended := false
	settledTurnFlip := false
	type settledLoop struct {
		loop   LiveLoop
		anchor LoopMessage
	}
	retained := make(map[string]settledLoop)
	for _, l := range live {
		if l.Kind != ReplyOwed && l.Kind != AwaitingReply {
			continue
		}
		if !slices.ContainsFunc(settlementView.Messages, func(m LoopMessage) bool { return m.ID == l.AnchorMessageID }) && !extended && view.Extend != nil {
			var err error
			settlementView, err = view.Extend()
			extended = true
			if err != nil {
				v.Err = fmt.Errorf("extend loop conversation: %w", err)
				return v
			}
			settlementView.ConversationID = view.ConversationID
			settlementView.SourceID = view.SourceID
			settlementView.SourceType = view.SourceType
			settlementView.ConversationType = view.ConversationType
			settlementView.Participants = view.Participants
			settlementView.LastMessageAt = view.LastMessageAt
			settlementView.AnchorExists = view.AnchorExists
			settlementView.BodyText = view.BodyText
		}
		closed, gap, anchor, err := t.settle(settlementView, l, now, cfg)
		if err != nil {
			v.Err = err
			return v
		}
		if closed != nil {
			settledTurnFlip = settledTurnFlip || closed.Reason == "turn_flip"
			v.Close = append(v.Close, *closed)
		}
		if gap {
			v.CoverageGaps = append(v.CoverageGaps, l.ID)
		} else if anchor != nil {
			retained[l.Kind] = settledLoop{loop: l, anchor: *anchor}
		}
	}
	if extended && (len(turns) > 0 || settledTurnFlip) {
		// Extended evidence preserves a live anchor and reveals a new trailing
		// turn after settlement closes an older obligation.
		turns = t.trailing(settlementView)
	}
	for _, l := range live {
		if (l.Kind != ReplyOwed && l.Kind != AwaitingReply) || slices.ContainsFunc(v.Close, func(c CloseDecision) bool { return c.ID == l.ID }) {
			continue
		}
		anchorIndex := slices.IndexFunc(settlementView.Messages, func(m LoopMessage) bool { return m.ID == l.AnchorMessageID })
		if anchorIndex < 0 {
			continue
		}
		counterparty, ok := obligationCounterparty(settlementView, l.Kind, settlementView.Messages[anchorIndex])
		if ok && suppressed(counterparty, supp) {
			closeLive(l, "muted")
		}
	}
	if len(turns) == 0 {
		// Neutral messages in the recent window cannot overturn settlement's
		// proof that an older obligation is still unanswered.
		keepUncertain("")
		return v
	}
	if extended && !settledTurnFlip && !hasEligibleTrailingAnchor(view, recentTurns, &t, cfg) {
		// A statement nudge or CC-only message in the recent window cannot
		// create a second Open for a retained live anchor found by extension.
		return v
	}
	trailing := turns[0]
	last := trailing.messages[len(trailing.messages)-1]
	large := settlementView.ConversationType == "group_chat" && nonOwnerCount(settlementView.Participants) > cfg.MaxGroupCounterparts
	var anchor *LoopMessage
	var counterparty LoopParticipant
	kind, threshold := ReplyOwed, cfg.ReplyOwedAfter
	var uncertain bool
	if trailing.direction == Inbound {
		if settled, ok := retained[ReplyOwed]; ok {
			anchorCopy := settled.anchor
			anchor = &anchorCopy
		}
		ownerWrote := len(turns) > 1 && turns[1].direction == Outbound
		bulk := false
		list := false
		for _, m := range trailing.messages {
			list = list || m.ListID != ""
			bulk = bulk || slices.ContainsFunc(m.Labels, func(label string) bool {
				return label == "CATEGORY_PROMOTIONS" || label == "CATEGORY_SOCIAL" || label == "CATEGORY_FORUMS"
			})
			if m.Signals != nil {
				bulk = bulk || m.Signals.PrecedenceBulk || m.Signals.ListUnsubscribe
			}
			if addressedToOwner(m, settlementView.ConversationType, large) && anchor == nil {
				candidateCounterparty, ok := obligationCounterparty(settlementView, kind, m)
				if !ok || suppressed(candidateCounterparty, supp) {
					continue
				}
				candidate := m
				anchor = &candidate
				counterparty = candidateCounterparty
			}
		}
		if anchor != nil && counterparty.ID == 0 {
			if settled, ok := retained[ReplyOwed]; ok && settled.anchor.ID == anchor.ID && settled.loop.CounterpartyParticipantID != 0 {
				counterparty = LoopParticipant{ID: settled.loop.CounterpartyParticipantID, PersonID: settled.loop.CounterpartyPersonID}
			} else {
				counterparty = anchor.Sender
			}
		}
		if anchor != nil && suppressed(counterparty, supp) {
			closeKept(kind, "muted")
			return v
		}
		liveAnchorInTurn := slices.ContainsFunc(live, func(l LiveLoop) bool {
			return l.Kind == ReplyOwed &&
				!slices.ContainsFunc(v.Close, func(c CloseDecision) bool { return c.ID == l.ID }) &&
				!slices.Contains(v.CoverageGaps, l.ID) &&
				slices.ContainsFunc(trailing.messages, func(m LoopMessage) bool { return m.ID == l.AnchorMessageID })
		})
		if anchor != nil && (list || (bulk && !ownerWrote && !liveAnchorInTurn)) {
			closeKept(kind, "withdrawn")
			return v
		}
	} else {
		kind, threshold = AwaitingReply, cfg.AwaitingReplyAfter
		if settled, ok := retained[kind]; ok {
			anchorCopy := settled.anchor
			anchor = &anchorCopy
		}
		if large {
			closeKept(kind, "withdrawn")
			return v
		}
		for _, m := range trailing.messages {
			body := t.body(settlementView, m.ID)
			uncertain = uncertain || !body.ok
			if body.ok && asksQuestion(body.text) && anchor == nil {
				candidateCounterparty, ok := obligationCounterparty(settlementView, kind, m)
				if !ok || suppressed(candidateCounterparty, supp) {
					continue
				}
				candidate := m
				anchor = &candidate
				counterparty = candidateCounterparty
			}
		}
		if anchor != nil && counterparty.ID == 0 {
			if settled, ok := retained[kind]; ok && settled.anchor.ID == anchor.ID && settled.loop.CounterpartyParticipantID != 0 {
				for _, participant := range settlementView.Participants {
					if participant.ID == settled.loop.CounterpartyParticipantID {
						counterparty = participant
						break
					}
				}
				if counterparty.ID == 0 {
					counterparty = LoopParticipant{ID: settled.loop.CounterpartyParticipantID, PersonID: settled.loop.CounterpartyPersonID}
				}
			} else {
				candidate, ok := obligationCounterparty(settlementView, kind, *anchor)
				if !ok {
					closeKept(kind, "withdrawn")
					return v
				}
				counterparty = candidate
			}
		}
		if anchor != nil && suppressed(counterparty, supp) {
			closeKept(kind, "muted")
			return v
		}
		if uncertain {
			// A budget limit or failed read cannot disprove a live question.
			// Preserve its original evidence instead of selecting a newer nudge.
			keepUncertain(kind)
		}
	}
	// Missing or unread evidence cannot prove a new turn. Reconciliation can
	// retry later, but this pass must preserve the older obligation.
	for _, l := range live {
		if l.Kind == kind && slices.Contains(v.CoverageGaps, l.ID) {
			return v
		}
	}
	if anchor == nil {
		// Settlement already checked retained anchors, including the extended
		// window. A recent nudge or an unaddressed message cannot withdraw them.
		return v
	}
	summary := strings.TrimSpace(last.Subject)
	if summary == "" {
		summary = strings.TrimSpace(last.Snippet)
	}
	runes := []rune(summary)
	if len(runes) > 200 {
		summary = string(runes[:200])
	}
	opensAt := anchor.OccurredAt.Add(threshold)
	status := "held"
	if !now.Before(opensAt) {
		status = "open"
	}
	previousTurnAt := time.Time{}
	if len(turns) > 1 && len(turns[1].messages) > 0 {
		previousTurnAt = turns[1].messages[len(turns[1].messages)-1].OccurredAt
	}
	v.Open = &Spec{Kind: kind, Status: status, Summary: summary, Anchor: *anchor, LastActivity: last, Counterparty: counterparty,
		OwedSinceAt: anchor.OccurredAt, OpensAt: opensAt, TruncatedRun: t.truncated || uncertain, PreviousTurnKnown: len(turns) > 1, PreviousTurnAt: previousTurnAt}
	for _, m := range trailing.messages {
		v.Open.TurnMessageIDs = append(v.Open.TurnMessageIDs, m.ID)
	}
	return v
}

func nonOwnerCount(participants []LoopParticipant) int {
	ids := make(map[int64]struct{})
	for _, p := range participants {
		if !p.IsOwner {
			ids[p.ID] = struct{}{}
		}
	}
	return len(ids)
}

func eligibleConversation(v ConversationView, now time.Time, cfg Thresholds) bool {
	if v.ConversationType != "email_thread" && v.ConversationType != "direct_chat" && v.ConversationType != "group_chat" {
		return false
	}
	if !messageSourceTypes[v.SourceType] || nonOwnerCount(v.Participants) == 0 {
		return false
	}
	return cfg.Lookback == 0 || !v.LastMessageAt.Before(now.Add(-cfg.Lookback))
}

func addressedToOwner(m LoopMessage, conversationType string, large bool) bool {
	if conversationType == "direct_chat" && !large {
		return true
	}
	for _, r := range m.Recipients {
		if r.IsOwner && (r.Type == "mention" || (!large && (r.Type == "to" || conversationType == "group_chat" && r.Type == "member"))) {
			return true
		}
	}
	return false
}

func hasEligibleTrailingAnchor(view ConversationView, turns []turn, t *traversal, cfg Thresholds) bool {
	if len(turns) == 0 {
		return false
	}
	large := view.ConversationType == "group_chat" && nonOwnerCount(view.Participants) > cfg.MaxGroupCounterparts
	for _, message := range turns[0].messages {
		if turns[0].direction == Inbound {
			if !addressedToOwner(message, view.ConversationType, large) {
				continue
			}
			if _, ok := obligationCounterparty(view, ReplyOwed, message); ok {
				return true
			}
			continue
		}
		body := t.body(view, message.ID)
		if body.ok && asksQuestion(body.text) {
			if _, ok := obligationCounterparty(view, AwaitingReply, message); ok {
				return true
			}
		}
	}
	return false
}

func obligationCounterparty(view ConversationView, kind string, anchor LoopMessage) (LoopParticipant, bool) {
	if kind == ReplyOwed {
		if anchor.Sender.ID != 0 && !anchor.Sender.IsOwner {
			return anchor.Sender, true
		}
		return LoopParticipant{}, false
	}
	var candidate LoopParticipant
	bestPriority := 3
	for _, recipient := range anchor.Recipients {
		priority, validRecipientType := counterpartyRecipientPriority(recipient.Type, view.ConversationType)
		if recipient.ID == 0 || recipient.IsOwner || !validRecipientType {
			continue
		}
		if candidate.ID == 0 || priority < bestPriority || priority == bestPriority && recipient.ID < candidate.ID {
			candidate = recipient.LoopParticipant
			bestPriority = priority
		}
	}
	if candidate.ID != 0 {
		return candidate, true
	}
	if view.ConversationType == "direct_chat" || view.ConversationType == "group_chat" {
		for _, participant := range view.Participants {
			if participant.ID != 0 && !participant.IsOwner && (candidate.ID == 0 || participant.ID < candidate.ID) {
				candidate = participant
			}
		}
	}
	return candidate, candidate.ID != 0
}

func counterpartyRecipientPriority(recipientType, conversationType string) (int, bool) {
	switch recipientType {
	case "mention":
		return 0, true
	case "to":
		return 1, true
	case "member":
		return 2, conversationType == "group_chat"
	default:
		return 0, false
	}
}

func suppressed(p LoopParticipant, s Suppressions) bool {
	return slices.Contains(s.Participants, p.ID) || (p.PersonID != 0 && slices.Contains(s.Persons, p.PersonID))
}

// These types are message sources only. Freshness's source inventory owns
// scheduling and sync outcomes; it must not infer eligibility from a schedule.
var messageSourceTypes = map[string]bool{
	"gmail": true, "imap": true, "o365": true, "msmail": true, "hey": true, "apple-mail": true, "mbox": true,
	"pst": true, "eml": true, "maildir": true, "google-groups": true, "facebook_messenger": true,
	"beeper": true, "chatwoot": true, "slack": true, "teams": true, "discord": true, "matrix": true, "imessage": true,
	"whatsapp": true, "slackdump": true, "imazing_csv": true, "synctech-sms": true,
	"apple_messages": true, "google_messages": true, "synctech_sms": true,
}
