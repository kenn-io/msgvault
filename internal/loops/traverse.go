package loops

import "slices"

type bodyResult struct {
	text string
	ok   bool
}

type traversal struct {
	limit, reads int
	bodies       map[int64]bodyResult
	truncated    bool
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
	if !metadataSubstantive(m) {
		return false
	}
	if m.Direction == Inbound {
		return true
	}
	body := t.body(v, m.ID)
	if !body.ok || !isAcknowledgementOnly(body.text) {
		return true
	}
	// A chain of acknowledgments can follow the same unanswered question.
	// Stop at a real owner answer; do not revive an older counterpart turn.
	for j := i - 1; j >= 0; j-- {
		previous := v.Messages[j]
		if !metadataSubstantive(previous) {
			continue
		}
		if previous.Direction == Inbound {
			question := t.body(v, previous.ID)
			return !question.ok || !asksQuestion(question.text)
		}
		priorBody := t.body(v, previous.ID)
		if !priorBody.ok || !isAcknowledgementOnly(priorBody.text) {
			return true
		}
	}
	return true
}

type turn struct {
	direction Direction
	messages  []LoopMessage
}

func (t *traversal) trailing(v ConversationView) []turn {
	var turns []turn
	for i := len(v.Messages) - 1; i >= 0; i-- {
		if !t.substantive(v, i) {
			continue
		}
		m := v.Messages[i]
		if len(turns) == 0 {
			turns = append(turns, turn{direction: m.Direction})
		}
		if m.Direction != turns[0].direction {
			turns = append(turns, turn{direction: m.Direction, messages: []LoopMessage{m}})
			break
		}
		turns[0].messages = append(turns[0].messages, m)
	}
	if len(turns) > 0 {
		slices.Reverse(turns[0].messages)
	}
	return turns
}
