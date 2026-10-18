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
	if !eligibleConversation(view) {
		closeKept("", "withdrawn")
		return v
	}
	t := traversal{limit: cfg.RunMessageLimit, bodies: make(map[int64]bodyResult)}
	if outsideLookback(view, now, cfg) && !t.hasUndatedSubstantive(view) {
		closeKept("", "withdrawn")
		return v
	}
	for _, l := range live {
		if l.Kind != ReplyOwed && l.Kind != AwaitingReply {
			continue
		}
		if _, err := t.checkAnchor(view, l.AnchorMessageID); err != nil {
			v.Err = fmt.Errorf("check loop anchor: %w", err)
			return v
		}
	}
	turns := t.trailing(view)
	recentTurns := turns
	// Extend at most once even when several live anchors fall outside the
	// normal window. Resolve the window before settlement so decisions do not
	// depend on live-loop order.
	settlementView := view
	extended := false
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
	}
	// Settlement shares a body-read budget, so process anchors chronologically.
	anchorOrder := make(map[int64]int, len(settlementView.Messages))
	for i, message := range settlementView.Messages {
		anchorOrder[message.ID] = i
	}
	settlementLive := slices.Clone(live)
	slices.SortFunc(settlementLive, func(a, b LiveLoop) int {
		aIndex, aFound := anchorOrder[a.AnchorMessageID]
		bIndex, bFound := anchorOrder[b.AnchorMessageID]
		if !aFound {
			aIndex = -1
		}
		if !bFound {
			bIndex = -1
		}
		if aIndex < bIndex {
			return -1
		}
		if aIndex > bIndex {
			return 1
		}
		if a.AnchorMessageID < b.AnchorMessageID {
			return -1
		}
		if a.AnchorMessageID > b.AnchorMessageID {
			return 1
		}
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return strings.Compare(a.Kind, b.Kind)
	})
	settledLoop := false
	for _, l := range settlementLive {
		if l.Kind != ReplyOwed && l.Kind != AwaitingReply {
			continue
		}
		closed, gap, err := t.settle(settlementView, l, now, cfg)
		if err != nil {
			v.Err = err
			return v
		}
		if closed != nil {
			settledLoop = true
			v.Close = append(v.Close, *closed)
		}
		if gap {
			v.CoverageGaps = append(v.CoverageGaps, l.ID)
		}
	}
	if len(t.missingAnchors) > 0 {
		// AnchorExists is authoritative even if a stale row remains in the
		// extended window. Rebuild settlement turns without absent anchors.
		turns = t.trailing(settlementView)
	} else if extended && (len(turns) > 0 || settledLoop) {
		// Extended evidence preserves a live anchor and reveals a new trailing
		// turn after settlement closes or withdraws an older obligation.
		turns = t.trailing(settlementView)
	}
	if promoted, ok := t.pendingOutboundQuestionAfterBulk(settlementView, turns); ok {
		turns = promoted
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
	if t.hasUndatedSubstantive(view) || t.hasUndatedSubstantive(settlementView) {
		keepUncertain("")
		return v
	}
	if outsideLookback(view, now, cfg) {
		closeKept("", "withdrawn")
		return v
	}
	if len(turns) == 0 {
		// Neutral messages in the recent window cannot overturn settlement's
		// proof that an older obligation is still unanswered.
		keepUncertain("")
		return v
	}
	if extended {
		trailingKind := ReplyOwed
		if turns[0].direction == Outbound {
			trailingKind = AwaitingReply
		}
		retained := slices.ContainsFunc(live, func(l LiveLoop) bool {
			if l.Kind != trailingKind || slices.Contains(v.CoverageGaps, l.ID) ||
				slices.ContainsFunc(v.Close, func(c CloseDecision) bool { return c.ID == l.ID }) {
				return false
			}
			return slices.ContainsFunc(settlementView.Messages, func(m LoopMessage) bool { return m.ID == l.AnchorMessageID })
		})
		turnWideListMail := turns[0].direction == Inbound && slices.ContainsFunc(turns[0].messages, func(m LoopMessage) bool {
			return m.ListID != ""
		})
		if retained && !turnWideListMail && !hasEligibleTrailingAnchor(view, recentTurns, &t, cfg) {
			// A statement nudge or CC-only message in the recent window cannot
			// create a second Open for a retained live anchor found by extension.
			return v
		}
	}
	trailing := turns[0]
	last := trailing.messages[len(trailing.messages)-1]
	large := settlementView.ConversationType == "group_chat" && nonOwnerCount(settlementView.Participants) > cfg.MaxGroupCounterparts
	var anchor *LoopMessage
	var counterparty LoopParticipant
	kind, threshold := ReplyOwed, cfg.ReplyOwedAfter
	var uncertain bool
	if trailing.direction == Inbound {
		// An unreadable body during acknowledgement lookback can hide a later
		// owner turn even when the selected inbound anchor is readable.
		uncertain = len(t.uncertainAcks) > 0
		ownerWrote := len(turns) > 1 && turns[1].direction == Outbound
		bulkLabels := false
		list := false
		for _, m := range trailing.messages {
			list = list || m.ListID != ""
			bulkLabels = bulkLabels || hasBulkCategory(m)
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
		latestBulkSignal := last.Signals != nil && (last.Signals.PrecedenceBulk || last.Signals.ListUnsubscribe)
		liveAnchorInTurn := slices.ContainsFunc(live, func(l LiveLoop) bool {
			return l.Kind == ReplyOwed &&
				!slices.ContainsFunc(v.Close, func(c CloseDecision) bool { return c.ID == l.ID }) &&
				!slices.Contains(v.CoverageGaps, l.ID) &&
				slices.ContainsFunc(trailing.messages, func(m LoopMessage) bool { return m.ID == l.AnchorMessageID })
		})
		if anchor != nil && (list || ((bulkLabels || latestBulkSignal) && !ownerWrote && !liveAnchorInTurn)) {
			closeKept(kind, "withdrawn")
			return v
		}
	} else {
		kind, threshold = AwaitingReply, cfg.AwaitingReplyAfter
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
	}
	if uncertain {
		// Incomplete body evidence cannot disprove a live obligation.
		keepUncertain(kind)
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
	v.Open = &Spec{Kind: kind, Status: status, Summary: summary, Anchor: *anchor, LastActivity: last, Counterparty: counterparty,
		OwedSinceAt: anchor.OccurredAt, OpensAt: opensAt, TruncatedRun: t.truncated || uncertain, PreviousTurnKnown: len(turns) > 1}
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

func eligibleConversation(v ConversationView) bool {
	if v.ConversationType != "email_thread" && v.ConversationType != "direct_chat" && v.ConversationType != "group_chat" {
		return false
	}
	if !messageSourceTypes[v.SourceType] || nonOwnerCount(v.Participants) == 0 {
		return false
	}
	return true
}

func outsideLookback(v ConversationView, now time.Time, cfg Thresholds) bool {
	return cfg.Lookback != 0 && !v.LastMessageAt.IsZero() && v.LastMessageAt.Before(now.Add(-cfg.Lookback))
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
	"apple_messages": true, "google_messages": true, "google_voice": true, "synctech_sms": true,
}
