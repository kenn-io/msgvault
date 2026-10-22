package loops

import (
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

// Detection contains each current counterparty's unanswered turn.
type Detection struct {
	Loops    []Loop
	Unparsed int
}

// Detect walks newest first, sharing text and MIME evidence across requesters.
// Each person's newer exchange settles only that person's older questions.
func Detect(conv store.LoopConversation, r Reader) (Detection, error) {
	if conv.GroupChat || (conv.ConversationType != "email_thread" && conv.ConversationType != "direct_chat") {
		return Detection{}, nil
	}
	w := walker{email: conv.ConversationType == "email_thread", r: r, evidence: map[int64]store.LoopMessageEvidence{}}
	turns := map[loopParticipantIdentity]Loop{}
	latest := map[loopParticipantIdentity]time.Time{}
	for m, err := range conv.Walk() {
		if err != nil {
			return Detection{}, err
		}
		if w.isSettledParticipantMessage(m) {
			continue
		}
		neutral, err := w.neutral(m)
		if err != nil {
			return Detection{}, err
		}
		if neutral {
			// A neutral acknowledgement counts as activity but opens or
			// settles no question.
			if key := w.activityKey(m); w.acknowledges(m) && key != (loopParticipantIdentity{}) {
				if err := w.touch(latest, key, m); err != nil {
					return Detection{}, err
				}
			}
			continue
		}
		if err := w.settle(m); err != nil {
			return Detection{}, err
		}
		key := w.activityKey(m)
		if err := w.touch(latest, key, m); err != nil {
			return Detection{}, err
		}
		kind, cp, ok, err := w.opens(m)
		if err != nil {
			return Detection{}, err
		}
		if ok {
			key = participantIdentity(cp)
			loop := turns[key]
			loop.Kind, loop.Counterparty, loop.Anchor = kind, cp, m
			loop.ConversationID, loop.SourceID = conv.ConversationID, conv.SourceID
			loop.Channel = store.ChannelEmail
			if !w.email {
				loop.Channel = store.ChannelChat
			}
			loop.Summary, loop.OwedSince, loop.LastActivity = summary(m), m.OccurredAt, latest[key]
			loop.Questions = append(loop.Questions, m)
			turns[key] = loop
		}
		if !w.email && w.answered[m.ID] {
			break
		}
	}
	result := Detection{Unparsed: w.unparsed}
	for _, loop := range turns {
		slices.Reverse(loop.Questions)
		result.Loops = append(result.Loops, loop)
	}
	slices.SortFunc(result.Loops, func(a, b Loop) int {
		if c := a.OwedSince.Compare(b.OwedSince); c != 0 {
			return c
		}
		return int(a.Anchor.ID - b.Anchor.ID)
	})
	return result, nil
}

// activityKey identifies whose turn a message keeps active: the sender of an
// inbound message, or the awaited recipient of an outbound one.
func (w *walker) activityKey(m store.LoopMessage) loopParticipantIdentity {
	cp := m.Sender
	if m.Direction == store.DirectionOutbound {
		cp, _ = w.awaitedParticipant(m)
	}
	return participantIdentity(cp)
}

// touch records m as the latest activity for key unless newer activity is
// already known or the stored original does not vouch for m.
func (w *walker) touch(latest map[loopParticipantIdentity]time.Time, key loopParticipantIdentity, m store.LoopMessage) error {
	if !latest[key].IsZero() {
		return nil
	}
	vouched, err := w.vouched(m)
	if err != nil {
		return err
	}
	if vouched {
		latest[key] = m.OccurredAt
	}
	return nil
}

type walker struct {
	email    bool
	r        Reader
	evidence map[int64]store.LoopMessageEvidence
	unparsed int
	replies  []store.LoopMessage
	sent     []store.LoopMessage
	answered map[int64]bool
	// settledParticipants remembers people whose newer, verified exchange
	// settles their older requests. Their older messages cannot reopen the
	// same turn, so paged history can skip parsing them while looking for
	// unanswered requests from other people.
	settledParticipants map[loopParticipantIdentity]bool
}

type loopParticipantIdentity struct {
	personID      int64
	participantID int64
}

func participantIdentity(p store.LoopParticipant) loopParticipantIdentity {
	if p.PersonID != 0 {
		return loopParticipantIdentity{personID: p.PersonID}
	}
	if p.ParticipantID != 0 {
		return loopParticipantIdentity{participantID: p.ParticipantID}
	}
	return loopParticipantIdentity{}
}

func (w *walker) markSettledParticipant(p store.LoopParticipant) {
	key := participantIdentity(p)
	if key == (loopParticipantIdentity{}) {
		return
	}
	if w.settledParticipants == nil {
		w.settledParticipants = map[loopParticipantIdentity]bool{}
	}
	w.settledParticipants[key] = true
}

func (w *walker) isSettledParticipant(p store.LoopParticipant) bool {
	key := participantIdentity(p)
	return key != (loopParticipantIdentity{}) && w.settledParticipants[key]
}

func (w *walker) isSettledParticipantMessage(m store.LoopMessage) bool {
	if !w.email {
		return false
	}
	switch m.Direction {
	case store.DirectionInbound:
		return w.isSettledParticipant(m.Sender)
	case store.DirectionOutbound:
		relevant := false
		for _, recipient := range m.Recipients {
			if recipient.IsOwner || (recipient.Type != "to" && recipient.Type != "cc" && recipient.Type != "bcc") {
				continue
			}
			relevant = true
			if !w.isSettledParticipant(recipient.LoopParticipant) {
				return false
			}
		}
		return relevant
	default:
		return false
	}
}

func (w *walker) facts(id int64) (store.LoopMessageEvidence, error) {
	if e, ok := w.evidence[id]; ok {
		return e, nil
	}
	e, err := w.r.Evidence(id)
	if err != nil {
		return e, err
	}
	if e.Unreadable {
		w.unparsed++
	}
	w.evidence[id] = e
	return e, nil
}

// neutral applies the message columns and cached body and attachment facts.
func (w *walker) neutral(m store.LoopMessage) (bool, error) {
	if m.Direction != store.DirectionInbound && m.Direction != store.DirectionOutbound {
		return true, nil
	}
	inbound := m.Direction == store.DirectionInbound
	switch {
	case m.ListID != "":
		return true, nil
	case inbound && w.email && mime.IsNoReplyAddress(m.Sender.Address):
		return true, nil
	case !inbound && len(m.Recipients) > 0 &&
		!slices.ContainsFunc(m.Recipients, func(r store.LoopRecipient) bool { return !r.IsOwner }):
		// Mail to yourself answers no one and asks no one.
		return true, nil
	}
	if !m.HasFileAttachment {
		evidence, err := w.facts(m.ID)
		if err != nil {
			return false, err
		}
		if evidence.Empty {
			if !m.MayHaveFileAttachment {
				return true, nil
			}
		} else if !evidence.Acknowledgement {
			return false, nil
		}
		if m.MayHaveFileAttachment && evidence.Attachment {
			return false, nil
		}
		return true, nil
	}
	return false, nil
}

// acknowledges reports whether a neutral inbound or outbound message contains
// only an acknowledgement. Its timestamp can keep an unanswered question
// active without opening or settling a loop.
func (w *walker) acknowledges(m store.LoopMessage) bool {
	e, ok := w.evidence[m.ID]
	return ok && !e.Empty && e.Acknowledgement
}

// vouched reports whether the stored original leaves a message in play: it is
// not an automatic reply, calendar item, or bulk mail, and it parses. Chats
// have no original. A message with no stored original is judged by its
// columns and text alone, which the caller has done.
func (w *walker) vouched(m store.LoopMessage) (bool, error) {
	if !w.email {
		return true, nil
	}
	evidence, err := w.facts(m.ID)
	if err != nil {
		return false, err
	}
	return !evidence.Automated && !evidence.Unreadable, nil
}

// opens reports whether m can anchor a loop, and the loop it anchors: it asks
// something, its original leaves it in play, and you can name who it waits on.
// Anchor choice and lastAsk share it, so both count the same questions.
func (w *walker) opens(m store.LoopMessage) (kind string, counterparty store.LoopParticipant, ok bool, err error) {
	if w.answered[m.ID] || (m.Direction == store.DirectionInbound && !w.addressedToOwner(m)) {
		return "", store.LoopParticipant{}, false, nil
	}
	evidence, err := w.facts(m.ID)
	if err != nil || !evidence.Asks {
		return "", store.LoopParticipant{}, false, err
	}
	if vouched, err := w.vouched(m); err != nil || !vouched {
		return "", store.LoopParticipant{}, false, err
	}
	if m.Direction == store.DirectionInbound {
		return ReplyOwed, m.Sender, true, nil
	}
	counterparty, ok = w.awaitedParticipant(m)
	return AwaitingReply, counterparty, ok, nil
}

// answers reports whether a vouched inbound message came from the awaited
// sender. An automated message or unreadable original answers nothing.
func (w *walker) answers(run []store.LoopMessage, sender store.LoopParticipant) (bool, error) {
	if !w.email {
		return true, nil
	}
	for _, m := range run {
		if vouched, err := w.vouched(m); err != nil {
			return false, err
		} else if !vouched {
			continue
		}
		if m.Sender.ParticipantID == sender.ParticipantID || (m.Sender.PersonID != 0 && m.Sender.PersonID == sender.PersonID) {
			return true, nil
		}
	}
	return false, nil
}

// settle remembers messages from each side and records the requests they
// settle. Replies can settle older outbound questions, and recipient-matched
// outbound messages can settle older inbound questions.
func (w *walker) settle(m store.LoopMessage) error {
	if m.Direction == store.DirectionInbound {
		w.replies = append(w.replies, m)
		answered, err := w.answeredByNewerOutbound(m.Sender)
		if err != nil {
			return err
		}
		if answered {
			vouched, err := w.vouched(m)
			if err != nil {
				return err
			}
			if vouched {
				if w.answered == nil {
					w.answered = map[int64]bool{}
				}
				w.answered[m.ID] = true
				w.markSettledParticipant(m.Sender)
			}
		}
		return nil
	}
	if m.Direction != store.DirectionOutbound {
		return nil
	}
	counterparty, ok := w.awaitedParticipant(m)
	if ok && len(w.replies) > 0 {
		answered, err := w.answers(w.replies, counterparty)
		if err != nil {
			return err
		}
		if answered {
			answered, err = w.vouched(m)
			if err != nil {
				return err
			}
		}
		if w.answered == nil {
			w.answered = map[int64]bool{}
		}
		w.answered[m.ID] = answered
		if answered {
			w.markSettledParticipant(counterparty)
		}
	}
	w.sent = append(w.sent, m)
	return nil
}

// answeredByNewerOutbound reports whether a newer outbound message reached
// this sender. The walk is newest first, so sent contains only newer messages.
func (w *walker) answeredByNewerOutbound(sender store.LoopParticipant) (bool, error) {
	if !w.email {
		return len(w.sent) > 0, nil
	}
	for _, m := range w.sent {
		if !slices.ContainsFunc(m.Recipients, func(r store.LoopRecipient) bool {
			return (r.Type == "to" || r.Type == "cc" || r.Type == "bcc") && !r.IsOwner &&
				(r.ParticipantID == sender.ParticipantID || (r.PersonID != 0 && r.PersonID == sender.PersonID))
		}) {
			continue
		}
		if vouched, err := w.vouched(m); err != nil {
			return false, err
		} else if vouched {
			return true, nil
		}
	}
	return false, nil
}

// addressedToOwner is true for an email with you in To, and for any message
// in a direct chat.
func (w *walker) addressedToOwner(m store.LoopMessage) bool {
	if !w.email {
		return true
	}
	return slices.ContainsFunc(m.Recipients, func(r store.LoopRecipient) bool {
		return r.IsOwner && r.Type == "to"
	})
}

// awaitedParticipant is who your message waits on: the one person you wrote
// to in an email, or the other side of a direct chat. Addresses linked to one
// person are one recipient. An email to several people is group handling,
// which is deferred.
func (w *walker) awaitedParticipant(m store.LoopMessage) (store.LoopParticipant, bool) {
	var found []store.LoopParticipant
	seen := map[loopParticipantIdentity]bool{}
	for _, r := range m.Recipients {
		if r.IsOwner || r.ParticipantID == 0 ||
			(w.email && r.Type != "to") || (!w.email && r.Type != "to" && r.Type != "member") {
			continue
		}
		key := participantIdentity(r.LoopParticipant)
		if !seen[key] {
			seen[key] = true
			found = append(found, r.LoopParticipant)
		}
	}
	if len(found) == 0 || (w.email && len(found) > 1) {
		return store.LoopParticipant{}, false
	}
	return found[0], true
}

func summary(m store.LoopMessage) string {
	text := strings.TrimSpace(m.Subject)
	if text == "" {
		text = strings.TrimSpace(m.Snippet)
	}
	return textutil.PrefixRunes(text, 200)
}
