package loops

import (
	"iter"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

// Detection is what Detect found in one conversation.
type Detection struct {
	Loop Loop
	// Open is true when the conversation has an open loop.
	Open bool
	// Unparsed counts the originals the walk passed over because they fail
	// MIME parsing, so the caller can say how much it could not read.
	Unparsed int
}

// Detect finds the loop open in a conversation. It walks the conversation
// newest first and stops at the last message from the other side that counts,
// reading past the newest page only as far as that takes. It reads through r
// only for messages it examines.
//
// A message is neutral, and neither opens nor settles a turn, when it is not
// between you and someone else (observed, or from an address linked to you),
// comes from an automated sender or list, is an email that doesn't have you
// in To, is an automatic reply or calendar item, has no text of its own and
// no file, or is a bare acknowledgement with no file attached. Detect walks back across neutral
// messages to the run of same-side messages that ends the conversation. The
// oldest message in that run that asks something starts the clock, however
// old it is. Your message settles only the people it went to, so a question
// to you stays open behind a forward to someone else and their answer.
//
// The stored original is the costly read, so Detect parses it only when it
// can change the answer. It checks possible file attachments when a missing
// attachment row would make an empty or acknowledgement-only message neutral.
// It also checks automation evidence for the exchange boundaries and questions
// it may use to settle or anchor a loop. When text alone shows there is no loop,
// and no message from the other side could expose an older one, it parses
// nothing.
func Detect(conv store.LoopConversation, r Reader) (Detection, error) {
	if conv.ConversationType != "email_thread" && conv.ConversationType != "direct_chat" {
		return Detection{}, nil
	}
	w := walker{email: conv.ConversationType == "email_thread", r: r, texts: map[int64]string{}, vouch: map[int64]bool{}}
	next, stop := iter.Pull2(conv.Walk())
	defer stop()
	var counted []store.LoopMessage
	exhausted := false
	// at returns the i-th message that is not neutral by its columns and text.
	at := func(i int) (store.LoopMessage, bool, error) {
		for len(counted) <= i && !exhausted {
			m, err, ok := next()
			if !ok {
				exhausted = true
				break
			}
			if err != nil {
				return store.LoopMessage{}, false, err
			}
			if w.isSettledParticipantMessage(m) {
				continue
			}
			if neutral, err := w.neutral(m); err != nil {
				return store.LoopMessage{}, false, err
			} else if !neutral {
				counted = append(counted, m)
			}
		}
		if i >= len(counted) {
			return store.LoopMessage{}, false, nil
		}
		return counted[i], true, nil
	}

	first, ok, err := at(0)
	if err != nil || !ok {
		return Detection{}, err
	}
	// With no original read, the run is the newest messages from one side.
	// If none of them asks and nothing from the other side follows, no
	// original can create a loop: setting messages aside only shortens the run.
	asked, other := false, false
	for i := 0; ; i++ {
		m, ok, err := at(i)
		if err != nil {
			return Detection{}, err
		}
		if !ok {
			break
		}
		if m.Direction != first.Direction {
			other = true
			break
		}
		text, err := w.text(m.ID)
		if err != nil {
			return Detection{}, err
		}
		asked = asked || asks(text)
	}
	if !asked && !other {
		return Detection{}, nil
	}

	run, settled := []store.LoopMessage(nil), false
	for i := 0; ; i++ {
		m, ok, err := at(i)
		if err != nil {
			return Detection{}, err
		}
		if !ok {
			break
		}
		if err := w.settle(m); err != nil {
			return Detection{}, err
		}
		if len(run) > 0 && m.Direction == run[0].Direction {
			run = append(run, m)
			if m.Direction == store.DirectionOutbound && !w.answered[m.ID] {
				settled = false
			}
			continue
		}
		// m starts the run or would end it. Your messages to no one m came from,
		// and a forward that never reached who asks, leave the turn open. A run
		// from the other side that asks nothing only settles your message m, which
		// restarts the run as settled. m's original is read only if one could join.
		var ask store.LoopMessage
		inbound := len(run) > 0 && run[0].Direction == store.DirectionInbound
		if len(run) > 0 {
			if ask, err = w.lastAsk(run); err != nil {
				return Detection{}, err
			}
		}
		quiet := inbound && ask.ID == 0 && w.answered[m.ID]
		foreign := inbound && (!w.answered[m.ID] || quiet)
		if len(run) > 0 && !inbound {
			answered, err := w.answers(run, m.Sender)
			if err != nil {
				return Detection{}, err
			}
			foreign = !answered
			if answered && (settled || ask.ID == 0) {
				// This exchange settles only its sender. Keep walking for
				// questions from people your messages haven't answered.
				if !settled {
					settled, err = w.vouched(m)
					if err != nil {
						return Detection{}, err
					}
				}
				if settled && !w.email {
					// The other side's substantive message settles this direct-chat
					// run. Older exchanges cannot change its result.
					break
				}
				continue
			}
		}
		olderAsks := false
		if len(run) > 0 && (!foreign || quiet) {
			sides := []store.ActivityDirection{run[0].Direction}
			if quiet {
				sides = append(sides, store.DirectionOutbound)
			}
			if olderAsks, err = w.olderRunAsks(conv, m, sides...); err != nil {
				return Detection{}, err
			} else if !olderAsks {
				break
			}
		}
		if vouched, err := w.vouched(m); err != nil {
			return Detection{}, err
		} else if !vouched {
			continue
		}
		if len(run) > 0 && (!foreign || (inbound && ask.ID != 0)) {
			boundary := ask.Sender
			if ask.Direction == store.DirectionOutbound {
				var ok bool
				boundary, ok = w.awaitedParticipant(ask)
				if !ok {
					continue
				}
			}
			if reaches, err := w.answers([]store.LoopMessage{m}, boundary); err != nil {
				return Detection{}, err
			} else if reaches && !olderAsks {
				break
			}
			// When an older request could still join this run, keep walking.
			// The request selected from run may belong to someone else than the
			// older one, so settle each older request against its own recipient.
			continue
		}
		if len(run) > 0 && !inbound && foreign && ask.ID != 0 {
			// An unrelated older statement cannot displace an unanswered ask.
			if _, _, eligible, err := w.opens(m); err != nil {
				return Detection{}, err
			} else if !eligible {
				continue
			}
		}
		run, settled = append(run[:0], m), quiet
	}
	if len(run) == 0 || settled {
		return Detection{Unparsed: w.unparsed}, nil
	}
	newest := run[0]
	slices.Reverse(run)

	var loop Loop
	anchorAt := 0
	for i, m := range run {
		kind, counterparty, ok, err := w.opens(m)
		if err != nil {
			return Detection{}, err
		}
		if ok {
			loop.Kind, loop.Counterparty, loop.Anchor, anchorAt = kind, counterparty, m, i
			break
		}
	}
	if loop.Kind == "" {
		return Detection{Unparsed: w.unparsed}, nil
	}
	if loop.LastAsk, err = w.lastAsk(run[anchorAt:]); err != nil {
		return Detection{}, err
	}
	loop.ConversationID, loop.SourceID, loop.Channel = conv.ConversationID, conv.SourceID, store.ChannelEmail
	if !w.email {
		loop.Channel = store.ChannelChat
	}
	loop.Summary, loop.OwedSince, loop.LastActivity = summary(loop.Anchor), loop.Anchor.OccurredAt, newest.OccurredAt
	return Detection{Loop: loop, Open: true, Unparsed: w.unparsed}, nil
}

type walker struct {
	email     bool
	r         Reader
	texts     map[int64]string
	originals map[int64]originalEvidence
	// vouch remembers each original's verdict, so one is read once.
	vouch    map[int64]bool
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

type originalEvidence struct {
	signals           mime.AutomationSignals
	hasFileAttachment bool
	unreadable        bool
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
		participant, ok := w.awaitedParticipant(m)
		return ok && w.isSettledParticipant(participant)
	default:
		return false
	}
}

// text is what the sender wrote in the message, without quotes or signature.
func (w *walker) text(id int64) (string, error) {
	if text, ok := w.texts[id]; ok {
		return text, nil
	}
	body, err := w.r.Body(id)
	if err != nil {
		return "", err
	}
	w.texts[id] = ownText(body)
	return w.texts[id], nil
}

func (w *walker) original(id int64) (originalEvidence, error) {
	if evidence, ok := w.originals[id]; ok {
		return evidence, nil
	}
	signals, hasFileAttachment, unreadable, err := w.r.Original(id)
	if err != nil {
		return originalEvidence{}, err
	}
	evidence := originalEvidence{
		signals: signals, hasFileAttachment: hasFileAttachment, unreadable: unreadable,
	}
	if unreadable {
		// The original failed to parse, so it says nothing about what the
		// sender wrote. Count it once even when both attachment and automation
		// checks need the same parsed result.
		w.unparsed++
	}
	if w.originals == nil {
		w.originals = map[int64]originalEvidence{}
	}
	w.originals[id] = evidence
	return evidence, nil
}

// neutral applies the checks that read only the message's columns and text,
// then checks stored MIME for a possible file attachment when needed.
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
	case inbound && !w.addressedToOwner(m):
		// Cc and Bcc mail is for your information.
		return true, nil
	case !inbound && len(m.Recipients) > 0 &&
		!slices.ContainsFunc(m.Recipients, func(r store.LoopRecipient) bool { return !r.IsOwner }):
		// Mail to yourself answers no one and asks no one.
		return true, nil
	}
	if !m.HasFileAttachment {
		text, err := w.text(m.ID)
		if err != nil {
			return false, err
		}
		if strings.TrimSpace(text) == "" {
			if !m.MayHaveFileAttachment {
				return true, nil
			}
		} else if !isAcknowledgementOnly(text) {
			return false, nil
		}
		if m.MayHaveFileAttachment {
			evidence, err := w.original(m.ID)
			if err != nil {
				return false, err
			}
			if evidence.hasFileAttachment {
				return false, nil
			}
		}
		return true, nil
	}
	return false, nil
}

// vouched reports whether the stored original leaves a message in play: it is
// not an automatic reply, calendar item, or bulk mail, and it parses. Chats
// have no original. A message with no stored original is judged by its
// columns and text alone, which the caller has done.
func (w *walker) vouched(m store.LoopMessage) (bool, error) {
	if !w.email {
		return true, nil
	}
	if v, ok := w.vouch[m.ID]; ok {
		return v, nil
	}
	if _, err := w.text(m.ID); err != nil {
		return false, err
	}
	evidence, err := w.original(m.ID)
	if err != nil {
		return false, err
	}
	signals := evidence.signals
	automated := signals.AutoSubmitted || signals.CalendarPart || signals.ReplyToNoReply ||
		signals.PrecedenceBulk || signals.ListUnsubscribe
	vouched := !automated && !evidence.unreadable
	w.vouch[m.ID] = vouched
	return vouched, nil
}

// opens reports whether m can anchor a loop, and the loop it anchors: it asks
// something, its original leaves it in play, and you can name who it waits on.
// Anchor choice and lastAsk share it, so both count the same questions.
func (w *walker) opens(m store.LoopMessage) (kind string, counterparty store.LoopParticipant, ok bool, err error) {
	if w.answered[m.ID] {
		return "", store.LoopParticipant{}, false, nil
	}
	text, err := w.text(m.ID)
	if err != nil || !asks(text) {
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

// lastAsk is the newest message in run that can anchor a loop, or none. The
// run is oldest first when the answer is used for more than its presence.
func (w *walker) lastAsk(run []store.LoopMessage) (store.LoopMessage, error) {
	for _, m := range slices.Backward(run) {
		if _, _, ok, err := w.opens(m); err != nil {
			return store.LoopMessage{}, err
		} else if ok {
			return m, nil
		}
	}
	return store.LoopMessage{}, nil
}

// answers reports whether a message in run answers the sender: an inbound
// message came from them, or your message went to them in To or Cc.
// A message with no recipients on record is taken as theirs. A forward to
// someone else, an automated message, or an unreadable original answers nothing.
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
		if m.Direction == store.DirectionInbound {
			if m.Sender.ParticipantID == sender.ParticipantID || (m.Sender.PersonID != 0 && m.Sender.PersonID == sender.PersonID) {
				return true, nil
			}
			continue
		}
		if len(m.Recipients) == 0 || slices.ContainsFunc(m.Recipients, func(r store.LoopRecipient) bool {
			return (r.Type == "to" || r.Type == "cc") && !r.IsOwner &&
				(r.ParticipantID == sender.ParticipantID || (r.PersonID != 0 && r.PersonID == sender.PersonID))
		}) {
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
		if len(m.Recipients) > 0 && !slices.ContainsFunc(m.Recipients, func(r store.LoopRecipient) bool {
			return (r.Type == "to" || r.Type == "cc") && !r.IsOwner &&
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

// olderRunAsks reports whether an older request on one of the run's sides
// could remain unanswered after m. It checks each known request against m's
// recipient so a boundary to one person cannot hide another person's request.
// In email, messages beyond the loaded ones are unknown, so they count as a
// yes. A direct chat has one counterparty, so a substantive opposite-side
// message is a boundary for every older request in that conversation.
func (w *walker) olderRunAsks(conv store.LoopConversation, m store.LoopMessage, sides ...store.ActivityDirection) (bool, error) {
	if conv.ConversationType == "direct_chat" {
		return false, nil
	}
	at := slices.IndexFunc(conv.Messages, func(x store.LoopMessage) bool { return x.ID == m.ID })
	if at < 0 {
		return conv.HasEarlier(), nil
	}
	for _, older := range conv.Messages[at+1:] {
		if !slices.Contains(sides, older.Direction) {
			continue
		}
		if w.isSettledParticipantMessage(older) {
			continue
		}
		if neutral, err := w.neutral(older); err != nil {
			return false, err
		} else if neutral {
			continue
		}
		text, err := w.text(older.ID)
		if err != nil {
			return false, err
		}
		if asks(text) {
			if w.answered[older.ID] {
				continue
			}
			counterparty := older.Sender
			if older.Direction == store.DirectionOutbound {
				var ok bool
				counterparty, ok = w.awaitedParticipant(older)
				if !ok {
					continue
				}
			}
			answered, err := w.answers([]store.LoopMessage{m}, counterparty)
			if err != nil {
				return false, err
			}
			if !answered {
				return true, nil
			}
		}
	}
	return conv.HasEarlier(), nil
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
	type person struct {
		linked bool
		id     int64
	}
	var found []store.LoopParticipant
	seen := map[person]bool{}
	for _, r := range m.Recipients {
		if r.IsOwner || r.ParticipantID == 0 || (w.email && r.Type != "to") {
			continue
		}
		key := person{id: r.ParticipantID}
		if r.PersonID != 0 {
			key = person{linked: true, id: r.PersonID}
		}
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
