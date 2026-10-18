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
	// Extend at most once even when several live anchors fall outside the
	// normal window. Continue using the same body cache and global budget.
	settlementView := view
	extended := false
	settledTurnFlip := false
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
		closed, gap, err := t.settle(settlementView, l, now, cfg)
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
	trailing := turns[0]
	last := trailing.messages[len(trailing.messages)-1]
	large := settlementView.ConversationType == "group_chat" && nonOwnerCount(settlementView.Participants) > cfg.MaxGroupCounterparts
	var anchor *LoopMessage
	var counterparty LoopParticipant
	kind, threshold := ReplyOwed, cfg.ReplyOwedAfter
	var uncertain bool
	if trailing.direction == Inbound {
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
				candidate := m
				anchor = &candidate
			}
		}
		if anchor != nil {
			var ok bool
			counterparty, ok = obligationCounterparty(settlementView, kind, *anchor)
			if !ok {
				closeKept(kind, "withdrawn")
				return v
			}
			if suppressed(counterparty, supp) {
				closeKept(kind, "muted")
				return v
			}
		}
		if anchor != nil && (list || (bulk && !ownerWrote)) {
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
				candidate := m
				anchor = &candidate
			}
		}
		if anchor != nil {
			var ok bool
			counterparty, ok = obligationCounterparty(settlementView, kind, *anchor)
			if !ok {
				closeKept(kind, "withdrawn")
				return v
			}
			if suppressed(counterparty, supp) {
				closeKept(kind, "muted")
				return v
			}
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

func obligationCounterparty(view ConversationView, kind string, anchor LoopMessage) (LoopParticipant, bool) {
	if kind == ReplyOwed {
		if anchor.Sender.ID != 0 && !anchor.Sender.IsOwner {
			return anchor.Sender, true
		}
		return LoopParticipant{}, false
	}
	var candidate LoopParticipant
	for _, recipient := range anchor.Recipients {
		validRecipientType := recipient.Type == "to" || recipient.Type == "mention" ||
			view.ConversationType == "group_chat" && recipient.Type == "member"
		if recipient.ID == 0 || recipient.IsOwner || !validRecipientType {
			continue
		}
		if candidate.ID == 0 || recipient.ID < candidate.ID {
			candidate = recipient.LoopParticipant
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

func suppressed(p LoopParticipant, s Suppressions) bool {
	return slices.Contains(s.Participants, p.ID) || (p.PersonID != 0 && slices.Contains(s.Persons, p.PersonID))
}

// These types are message sources only. Freshness's source inventory owns
// scheduling and sync outcomes; it must not infer eligibility from a schedule.
var messageSourceTypes = map[string]bool{
	"gmail": true, "imap": true, "o365": true, "msmail": true, "hey": true, "apple-mail": true, "mbox": true,
	"pst": true, "eml": true, "maildir": true, "google-groups": true, "facebook_messenger": true,
	"beeper": true, "slack": true, "teams": true, "discord": true, "matrix": true, "imessage": true,
	"whatsapp": true, "slackdump": true, "imazing_csv": true, "synctech-sms": true,
	"apple_messages": true, "google_messages": true, "synctech_sms": true,
}
