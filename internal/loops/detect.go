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
	recordClose := func(decision CloseDecision) {
		if !slices.ContainsFunc(v.Close, func(existing CloseDecision) bool { return existing.ID == decision.ID }) {
			v.Close = append(v.Close, decision)
		}
		v.CoverageGaps = slices.DeleteFunc(v.CoverageGaps, func(id int64) bool { return id == decision.ID })
	}
	closeLive := func(loop LiveLoop, reason string) {
		recordClose(CloseDecision{ID: loop.ID, Reason: reason, At: now})
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
	hasLiveCoverageGap := func(kind string) bool {
		return slices.ContainsFunc(live, func(l LiveLoop) bool {
			return l.Kind == kind && slices.Contains(v.CoverageGaps, l.ID)
		})
	}
	if slices.Contains(supp.Conversations, view.ConversationID) {
		closeKept("", "muted")
		return v
	}
	// A person link or suppression may change while the message window remains
	// neutral. Close a live row from its saved counterpart identity even when
	// there is no new Open decision for the store to recheck.
	for _, loop := range live {
		counterparty := LoopParticipant{ID: loop.CounterpartyParticipantID, PersonID: loop.CounterpartyPersonID}
		for _, participant := range view.Participants {
			if participant.ID == counterparty.ID {
				counterparty = participant
				if counterparty.PersonID == 0 {
					counterparty.PersonID = loop.CounterpartyPersonID
				}
				break
			}
		}
		if (counterparty.ID != 0 || counterparty.PersonID != 0) && suppressed(counterparty, supp) {
			recordClose(CloseDecision{ID: loop.ID, Reason: "muted", At: now})
		}
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
	settledExtendedWithdrawal := false
	type settledLoop struct {
		loop   LiveLoop
		anchor *LoopMessage
	}
	retained := make(map[string]settledLoop)
	unprovenBulkAnchors := make(map[int64]int64)
	for _, l := range live {
		if l.Kind != ReplyOwed && l.Kind != AwaitingReply {
			continue
		}
		anchorInWindow := slices.ContainsFunc(settlementView.Messages, func(m LoopMessage) bool { return m.ID == l.AnchorMessageID })
		hasOwnerParticipationEvidence := l.OwnerParticipationKnown || l.OwnerParticipationMessageID != 0
		needsOwnerParticipationEvidence := l.Kind == ReplyOwed && !hasOwnerParticipationEvidence
		if l.Kind == ReplyOwed && hasOwnerParticipationEvidence {
			anchorIndex := slices.IndexFunc(settlementView.Messages, func(m LoopMessage) bool { return m.ID == l.AnchorMessageID })
			needsOwnerParticipationEvidence = anchorIndex >= 0 && isBulkMessage(settlementView.Messages[anchorIndex]) &&
				!slices.ContainsFunc(settlementView.Messages, func(m LoopMessage) bool { return m.ID == l.OwnerParticipationMessageID })
		}
		if (!anchorInWindow || needsOwnerParticipationEvidence) && !extended && view.Extend != nil {
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
		closed, gap, anchor, ownerParticipation, err := t.settle(settlementView, l, now, cfg)
		if err != nil {
			v.Err = err
			return v
		}
		if closed != nil {
			settledTurnFlip = settledTurnFlip || closed.Reason == "turn_flip"
			settledExtendedWithdrawal = settledExtendedWithdrawal ||
				(closed.Reason == "withdrawn" && !slices.ContainsFunc(view.Messages, func(m LoopMessage) bool { return m.ID == l.AnchorMessageID }))
			recordClose(*closed)
		}
		if gap && !slices.ContainsFunc(v.Close, func(decision CloseDecision) bool { return decision.ID == l.ID }) {
			v.CoverageGaps = append(v.CoverageGaps, l.ID)
		}
		if closed == nil && !slices.ContainsFunc(v.Close, func(decision CloseDecision) bool { return decision.ID == l.ID }) {
			l.OwnerParticipationMessageID = ownerParticipation.MessageID
			l.OwnerParticipationKnown = ownerParticipation.Known
			retained[l.Kind] = settledLoop{loop: l, anchor: anchor}
			if gap && l.Kind == ReplyOwed && !ownerParticipation.Known && anchor != nil && isBulkMessage(*anchor) {
				unprovenBulkAnchors[l.ID] = anchor.ID
			}
		}
	}
	if extended && (len(turns) > 0 || settledTurnFlip || settledExtendedWithdrawal) {
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
		var counterparty LoopParticipant
		var ok bool
		if l.Kind == AwaitingReply {
			counterparty, ok = selectAwaitingReplyCounterparty(settlementView, turns,
				&settlementView.Messages[anchorIndex], l.CounterpartyParticipantID)
		} else {
			counterparty, ok = obligationCounterparty(settlementView, l.Kind, settlementView.Messages[anchorIndex])
		}
		if ok && suppressed(counterparty, supp) {
			closeLive(l, "muted")
			if settled, ok := retained[l.Kind]; ok && settled.loop.ID == l.ID {
				delete(retained, l.Kind)
			}
			delete(unprovenBulkAnchors, l.ID)
		}
	}
	if len(turns) == 0 {
		// Neutral messages in the recent window cannot overturn settlement's
		// proof that an older obligation is still unanswered.
		keepUncertain("")
		return v
	}
	if extended && !settledTurnFlip && !settledExtendedWithdrawal && !hasEligibleTrailingAnchor(view, recentTurns, &t, cfg) {
		// A statement nudge or CC-only message in the recent window cannot
		// create a second Open for a retained live anchor found by extension.
		for _, loop := range live {
			if _, unproven := unprovenBulkAnchors[loop.ID]; unproven {
				closeLive(loop, "withdrawn")
			}
		}
		return v
	}
	trailing := turns[0]
	last := trailing.messages[len(trailing.messages)-1]
	large := settlementView.ConversationType == "group_chat" && nonOwnerCount(settlementView.Participants) > cfg.MaxGroupCounterparts
	var anchor *LoopMessage
	var counterparty LoopParticipant
	ownerParticipation := ownerParticipationEvidence{}
	kind, threshold := ReplyOwed, cfg.ReplyOwedAfter
	var uncertain bool
	if trailing.direction == Inbound {
		ownerWrote := len(turns) > 1 && turns[1].direction == Outbound
		if ownerWrote && len(turns[1].messages) > 0 {
			ownerParticipation = ownerParticipationEvidence{MessageID: turns[1].messages[len(turns[1].messages)-1].ID, Known: true}
		}
		retainedOwnerParticipation := ownerParticipationEvidence{}
		retainedAnchorMessageID := int64(0)
		if settled, ok := retained[ReplyOwed]; ok {
			if anchor == nil && settled.anchor != nil {
				anchorCopy := *settled.anchor
				anchor = &anchorCopy
			}
			retainedOwnerParticipation = ownerParticipationEvidence{
				MessageID: settled.loop.OwnerParticipationMessageID,
				Known:     settled.loop.OwnerParticipationKnown || settled.loop.OwnerParticipationMessageID != 0,
			}
			retainedAnchorMessageID = settled.loop.AnchorMessageID
		}
		bulk := false
		list := false
		for _, m := range trailing.messages {
			list = list || m.ListID != ""
			bulk = bulk || isBulkMessage(m)
			if addressedToOwner(m, settlementView.ConversationType, large) &&
				(anchor == nil || messageBefore(m, *anchor)) {
				candidateCounterparty, ok := obligationCounterparty(settlementView, kind, m)
				if !ok || suppressed(candidateCounterparty, supp) {
					continue
				}
				candidate := m
				anchor = &candidate
				counterparty = candidateCounterparty
			}
		}
		if anchor != nil {
			if counterparty.ID == 0 {
				// Sender attribution can be corrected after the live row was saved.
				// Prefer the current anchor identity; retain the saved one only when
				// the current anchor has no attributable sender.
				if anchor.Sender.ID != 0 && !anchor.Sender.IsOwner {
					counterparty = anchor.Sender
				} else if settled, ok := retained[ReplyOwed]; ok && settled.anchor != nil && settled.anchor.ID == anchor.ID && settled.loop.CounterpartyParticipantID != 0 {
					counterparty = LoopParticipant{ID: settled.loop.CounterpartyParticipantID, PersonID: settled.loop.CounterpartyPersonID}
				} else {
					counterparty = anchor.Sender
				}
			}
			// A retained anchor can come from the extended settlement window,
			// outside the recent messages used to classify this trailing turn.
			list = list || anchor.ListID != ""
			bulk = bulk || isBulkMessage(*anchor)
		}
		if !ownerParticipation.Known && anchor != nil && anchor.ID == retainedAnchorMessageID {
			ownerParticipation = retainedOwnerParticipation
		}
		if ownerParticipation.Known {
			ownerWrote = true
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
			if bulk && !ownerWrote {
				for _, loop := range live {
					if anchorID, unproven := unprovenBulkAnchors[loop.ID]; unproven && slices.ContainsFunc(trailing.messages, func(message LoopMessage) bool { return message.ID == anchorID }) {
						closeLive(loop, "withdrawn")
					}
				}
			}
			return v
		}
	} else {
		kind, threshold = AwaitingReply, cfg.AwaitingReplyAfter
		if settled, ok := retained[AwaitingReply]; ok && settled.anchor != nil {
			anchorCopy := *settled.anchor
			anchor = &anchorCopy
		}
		if large {
			closeKept(kind, "withdrawn")
			return v
		}
		for _, m := range trailing.messages {
			body := t.body(settlementView, m.ID)
			uncertain = uncertain || !body.ok
			if body.ok && asksQuestion(body.text) &&
				(anchor == nil || messageBefore(m, *anchor)) {
				candidate := m
				preferredCounterpartyID := int64(0)
				if settled, ok := retained[kind]; ok && settled.anchor != nil && settled.anchor.ID == candidate.ID {
					preferredCounterpartyID = settled.loop.CounterpartyParticipantID
				}
				candidateCounterparty, ok := selectAwaitingReplyCounterparty(
					settlementView, turns, &candidate, preferredCounterpartyID,
				)
				if !ok || suppressed(candidateCounterparty, supp) {
					continue
				}
				anchor = &candidate
				counterparty = candidateCounterparty
			}
		}
		preferredCounterpartyID := int64(0)
		if settled, ok := retained[kind]; ok && settled.anchor != nil && anchor != nil && settled.anchor.ID == anchor.ID {
			preferredCounterpartyID = settled.loop.CounterpartyParticipantID
		}
		selectedCounterparty, hasCounterparty := selectAwaitingReplyCounterparty(settlementView, turns, anchor, preferredCounterpartyID)
		counterparty = selectedCounterparty
		if !hasCounterparty {
			counterparty = LoopParticipant{}
		}
		if suppressed(counterparty, supp) {
			closeKept(kind, "muted")
			return v
		}
		if uncertain {
			// A body budget or read failure cannot prove that the live question
			// has no recipient or has been withdrawn.
			keepUncertain(kind)
		}
		if !hasCounterparty {
			if hasLiveCoverageGap(kind) {
				return v
			}
			closeKept(kind, "withdrawn")
			return v
		}
	}
	// Missing or unread evidence cannot prove a new turn. Reconciliation can
	// retry later, but this pass must preserve the older obligation.
	if hasLiveCoverageGap(kind) {
		return v
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
	previousTurnMessageID := int64(0)
	if len(turns) > 1 && len(turns[1].messages) > 0 {
		previousTurn := turns[1].messages[len(turns[1].messages)-1]
		previousTurnAt = previousTurn.OccurredAt
		previousTurnMessageID = previousTurn.ID
	}
	v.Open = &Spec{Kind: kind, Status: status, Summary: summary, Anchor: *anchor, LastActivity: last, Counterparty: counterparty,
		OwedSinceAt: anchor.OccurredAt, OpensAt: opensAt, TruncatedRun: t.truncated || uncertain,
		PreviousTurnKnown: len(turns) > 1, PreviousTurnAt: previousTurnAt, PreviousTurnMessageID: previousTurnMessageID,
		OwnerParticipationMessageID: ownerParticipation.MessageID, OwnerParticipationKnown: ownerParticipation.Known}
	for _, m := range trailing.messages {
		v.Open.TurnMessageIDs = append(v.Open.TurnMessageIDs, m.ID)
	}
	return v
}

func messageBefore(a, b LoopMessage) bool {
	return a.OccurredAt.Before(b.OccurredAt) || (a.OccurredAt.Equal(b.OccurredAt) && a.ID < b.ID)
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
	return cfg.Lookback == 0 || v.LastMessageAt.IsZero() || !v.LastMessageAt.Before(now.Add(-cfg.Lookback))
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

func selectAwaitingReplyCounterparty(view ConversationView, turns []turn, anchor *LoopMessage, preferredID int64) (LoopParticipant, bool) {
	var candidates []LoopParticipant
	if view.ConversationType == "direct_chat" || view.ConversationType == "group_chat" {
		for _, participant := range view.Participants {
			if !participant.IsOwner {
				candidates = append(candidates, participant)
			}
		}
	} else {
		if anchor != nil {
			for _, recipient := range anchor.Recipients {
				if recipient.Type == "to" && !recipient.IsOwner {
					candidates = append(candidates, recipient.LoopParticipant)
				}
			}
		}
	}
	counts := make(map[int64]int)
	for _, turn := range turns {
		for _, message := range turn.messages {
			counts[message.Sender.ID]++
		}
	}
	for _, participant := range candidates {
		if participant.ID == preferredID && preferredID != 0 {
			return participant, true
		}
	}
	candidatePriorities := make(map[int64]int, len(candidates))
	for _, participant := range candidates {
		candidatePriorities[participant.ID] = 2
	}
	if anchor != nil {
		for _, recipient := range anchor.Recipients {
			priority, valid := counterpartyRecipientPriority(recipient.Type, view.ConversationType)
			if !valid || recipient.ID == 0 || recipient.IsOwner {
				continue
			}
			if current, ok := candidatePriorities[recipient.ID]; ok && priority < current {
				candidatePriorities[recipient.ID] = priority
			}
		}
	}
	bestPriority := 3
	for _, participant := range candidates {
		if priority := candidatePriorities[participant.ID]; priority < bestPriority {
			bestPriority = priority
		}
	}
	var counterparty LoopParticipant
	for _, participant := range candidates {
		if candidatePriorities[participant.ID] != bestPriority {
			continue
		}
		if counterparty.ID == 0 || counts[participant.ID] > counts[counterparty.ID] ||
			(counts[participant.ID] == counts[counterparty.ID] && participant.ID < counterparty.ID) {
			counterparty = participant
		}
	}
	return counterparty, counterparty.ID != 0
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
