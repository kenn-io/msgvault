package loops

import "slices"

type bodyResult struct {
	text string
	ok   bool
}

type traversal struct {
	limit, reads         int
	maxGroupCounterparts int
	suppressions         Suppressions
	bodies               map[int64]bodyResult
	uncertainAcks        map[int64]bool
	missingAnchors       map[int64]struct{}
	liveReplyAnchors     map[int64]struct{}
	anchorResults        map[int64]bool
	checkedAnchors       map[int64]bool
	truncated            bool
}

func (t *traversal) checkAnchor(v ConversationView, id int64) (bool, error) {
	if v.AnchorExists == nil {
		return true, nil
	}
	if t.checkedAnchors[id] {
		return t.anchorResults[id], nil
	}
	exists, err := v.AnchorExists(id)
	if err != nil {
		return false, err
	}
	if t.checkedAnchors == nil {
		t.checkedAnchors = make(map[int64]bool)
		t.anchorResults = make(map[int64]bool)
	}
	t.checkedAnchors[id] = true
	t.anchorResults[id] = exists
	if !exists {
		if t.missingAnchors == nil {
			t.missingAnchors = make(map[int64]struct{})
		}
		t.missingAnchors[id] = struct{}{}
	}
	return exists, nil
}

func (t *traversal) hasUndatedSubstantive(v ConversationView) bool {
	for _, m := range v.Messages {
		if _, missing := t.missingAnchors[m.ID]; missing {
			continue
		}
		if m.OccurredAt.IsZero() && metadataEligible(m) {
			return true
		}
	}
	return false
}

func (t *traversal) body(v ConversationView, id int64) bodyResult {
	if result, ok := t.bodies[id]; ok {
		return result
	}
	if t.reads >= t.limit {
		t.truncated = true
		return bodyResult{}
	}
	t.reads++
	var result bodyResult
	if v.BodyText != nil {
		var err error
		result.text, err = v.BodyText(id)
		result.ok = err == nil
	}
	t.bodies[id] = result
	return result
}

func (t *traversal) substantive(v ConversationView, i int) bool {
	m := v.Messages[i]
	if _, missing := t.missingAnchors[m.ID]; missing {
		return false
	}
	if !metadataSubstantive(m) {
		return false
	}
	if m.Direction == Inbound {
		return true
	}
	if m.HasUserAttachments {
		return true
	}
	body := t.body(v, m.ID)
	if !body.ok || !isAcknowledgementOnly(body.text, v.Participants) {
		return true
	}
	// A chain of acknowledgments can follow the same unanswered question.
	// Stop at a real owner answer; do not revive an older counterpart turn.
	for j := range slices.Backward(v.Messages[:i]) {
		previous := v.Messages[j]
		if _, missing := t.missingAnchors[previous.ID]; missing {
			continue
		}
		// List-ID mail cannot create a reply obligation. Bulk labels and headers
		// only mark an eligible direct question after owner participation or
		// when a verified live loop already uses that message as its anchor.
		if previous.Direction == Inbound && previous.ListID != "" {
			continue
		}
		if previous.Direction == Inbound && isBulkMail(previous) &&
			!t.bulkQuestionEligible(v, j, previous) {
			continue
		}
		if !metadataSubstantive(previous) {
			continue
		}
		if previous.Direction == Inbound {
			large := v.ConversationType == "group_chat" && nonOwnerCount(v.Participants) > t.maxGroupCounterparts
			if !addressedToOwner(previous, v.ConversationType, large) {
				continue
			}
			counterparty, ok := obligationCounterparty(v, ReplyOwed, previous)
			if !ok || suppressed(counterparty, t.suppressions) {
				continue
			}
			question := t.body(v, previous.ID)
			if !question.ok {
				if t.uncertainAcks == nil {
					t.uncertainAcks = make(map[int64]bool)
				}
				t.uncertainAcks[m.ID] = true
				return false
			}
			if asksQuestion(question.text) {
				return false
			}
			continue
		}
		if previous.Direction == Outbound && previous.HasUserAttachments {
			return true
		}
		priorBody := t.body(v, previous.ID)
		if !priorBody.ok || !isAcknowledgementOnly(priorBody.text, v.Participants) {
			return true
		}
	}
	return true
}

func (t *traversal) bulkQuestionEligible(v ConversationView, index int, message LoopMessage) bool {
	counterparty, ok := obligationCounterparty(v, ReplyOwed, message)
	if !ok || suppressed(counterparty, t.suppressions) {
		return false
	}
	if _, live := t.liveReplyAnchors[message.ID]; live {
		return true
	}
	for i := index - 1; i >= 0; i-- {
		previous := v.Messages[i]
		if previous.Direction == Inbound && isListOrBulkMail(previous) {
			continue
		}
		if !t.substantive(v, i) {
			continue
		}
		return previous.Direction == Outbound
	}
	return false
}

type turn struct {
	direction Direction
	messages  []LoopMessage
}

func (t *traversal) trailing(v ConversationView) []turn {
	var turns []turn
	var trailingListMail []LoopMessage
	for i := range slices.Backward(v.Messages) {
		m := v.Messages[i]
		if m.Direction == Inbound && len(turns) == 0 && m.ListID != "" {
			if !t.substantive(v, i) {
				continue
			}
			// Delay List-ID mail until an older substantive message shows whether
			// it follows an unanswered outbound question or an inbound turn.
			trailingListMail = append(trailingListMail, m)
			continue
		}
		if m.Direction == Inbound && len(turns) > 0 && turns[0].direction == Outbound && isListOrBulkMail(m) {
			// Settlement ignores inbound list and bulk mail as reply evidence.
			continue
		}
		if !t.substantive(v, i) {
			continue
		}
		if len(turns) == 0 {
			if len(trailingListMail) > 0 && m.Direction == Inbound {
				turns = append(turns, turn{direction: Inbound, messages: append(trailingListMail, m)})
				trailingListMail = nil
				continue
			}
			trailingListMail = nil
			turns = append(turns, turn{direction: m.Direction})
		}
		if m.Direction != turns[0].direction {
			turns = append(turns, turn{direction: m.Direction, messages: []LoopMessage{m}})
			break
		}
		turns[0].messages = append(turns[0].messages, m)
	}
	if len(turns) == 0 && len(trailingListMail) > 0 {
		turns = append(turns, turn{direction: Inbound, messages: trailingListMail})
	}
	if len(turns) > 0 {
		slices.Reverse(turns[0].messages)
	}
	return turns
}

func (t *traversal) pendingOutboundQuestionAfterBulk(v ConversationView, turns []turn) ([]turn, bool) {
	if len(turns) == 0 || turns[0].direction != Inbound || len(turns[0].messages) == 0 {
		return nil, false
	}
	for _, m := range turns[0].messages {
		if !isListOrBulkMail(m) {
			return nil, false
		}
	}
	firstIndex := slices.IndexFunc(v.Messages, func(m LoopMessage) bool { return m.ID == turns[0].messages[0].ID })
	if firstIndex <= 0 {
		return nil, false
	}

	var outbound []LoopMessage
	var previous *LoopMessage
	question, unreadable := false, false
	for i := firstIndex - 1; i >= 0; i-- {
		m := v.Messages[i]
		if _, missing := t.missingAnchors[m.ID]; missing {
			continue
		}
		if m.Direction == Inbound && isListOrBulkMail(m) {
			continue
		}
		if !t.substantive(v, i) {
			continue
		}
		if m.Direction != Outbound {
			previous = &m
			break
		}
		outbound = append(outbound, m)
		counterparty, ok := obligationCounterparty(v, AwaitingReply, m)
		if !ok || suppressed(counterparty, t.suppressions) {
			continue
		}
		body := t.body(v, m.ID)
		if !body.ok {
			unreadable = true
			continue
		}
		question = question || asksQuestion(body.text)
	}
	if len(outbound) == 0 || (!question && !unreadable) {
		return nil, false
	}
	slices.Reverse(outbound)
	promoted := []turn{{direction: Outbound, messages: outbound}}
	if previous != nil {
		promoted = append(promoted, turn{direction: previous.Direction, messages: []LoopMessage{*previous}})
	}
	return promoted, true
}
