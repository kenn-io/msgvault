package loops

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

var (
	now = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

	owner   = store.LoopParticipant{ParticipantID: 1, DisplayName: "Me", Address: "me@example.test", IsOwner: true}
	alex    = store.LoopParticipant{ParticipantID: 2, PersonID: 20, DisplayName: "Alex Example", Address: "alex@example.test"}
	support = store.LoopParticipant{ParticipantID: 4, PersonID: 40, DisplayName: "Support", Address: "support@example.test"}
	noreply = store.LoopParticipant{ParticipantID: 5, DisplayName: "Service", Address: "noreply@example.test"}
	alexAlt = store.LoopParticipant{ParticipantID: 3, PersonID: 20, DisplayName: "Alex Example", Address: "alex@home.example.test"}
	sam     = store.LoopParticipant{ParticipantID: 6, PersonID: 60, DisplayName: "Sam Rivera", Address: "sam@example.test"}
	// ownerAlias is an address linked to you, which the store marks as owner.
	ownerAlias = store.LoopParticipant{ParticipantID: 8, DisplayName: "Me", Address: "me@home.example.test", IsOwner: true}
)

// msg is one message in a scripted conversation. Zero fields mean an inbound
// email from alex to you.
type msg struct {
	out     bool
	observe bool
	from    store.LoopParticipant
	to      []store.LoopParticipant
	ccOnly  bool
	ago     time.Duration
	body    string
	subject string
	listID  string
	file    bool
	// rawFails is a message whose stored original fails MIME parsing.
	rawFails bool
	signals  mime.AutomationSignals
}

type script struct {
	chat  bool
	group bool
	msgs  []msg
}

// conversation returns the conversation, with its messages newest first, and
// a reader for them.
func (s script) conversation() (store.LoopConversation, *fakeReader) {
	conversationType := "email_thread"
	if s.chat {
		conversationType = "direct_chat"
	}
	if s.group {
		conversationType = "group_chat"
	}
	var messages []store.LoopMessage
	r := &fakeReader{bodies: map[int64]string{}, signals: map[int64]mime.AutomationSignals{}, files: map[int64]bool{}, rawFails: map[int64]bool{}}
	for i, in := range s.msgs {
		m := store.LoopMessage{ID: int64(i + 1), OccurredAt: now.Add(-in.ago), Direction: store.DirectionInbound,
			Subject: in.subject, ListID: in.listID, HasFileAttachment: in.file}
		recipientType := "to"
		if in.ccOnly {
			recipientType = "cc"
		}
		if s.chat {
			recipientType = "member"
		}
		switch {
		case in.observe:
			m.Direction = store.DirectionObserved
			m.Sender, m.Recipients = alex, []store.LoopRecipient{{LoopParticipant: sam, Type: recipientType}}
		case in.out:
			m.Direction = store.DirectionOutbound
			m.Sender = owner
			to := in.to
			if len(to) == 0 {
				to = []store.LoopParticipant{alex}
			}
			for _, p := range to {
				m.Recipients = append(m.Recipients, store.LoopRecipient{LoopParticipant: p, Type: recipientType})
			}
		default:
			m.Sender = alex
			if in.from.ParticipantID != 0 {
				m.Sender = in.from
			}
			m.Recipients = []store.LoopRecipient{{LoopParticipant: owner, Type: recipientType}}
		}
		messages = append(messages, m)
		r.bodies[m.ID], r.signals[m.ID], r.files[m.ID], r.rawFails[m.ID] = in.body, in.signals, in.file, in.rawFails
	}
	slices.Reverse(messages)
	return store.LoopConversation{ConversationID: 7, SourceID: 3, ConversationType: conversationType, Messages: messages}, r
}

func detect(s script) (Loop, bool, error) {
	conv, reader := s.conversation()
	detection, err := Detect(conv, reader)
	return detection.Loop, detection.Open, err
}

type fakeReader struct {
	bodies  map[int64]string
	signals map[int64]mime.AutomationSignals
	files   map[int64]bool
	// rawFails marks originals that fail MIME parsing.
	rawFails map[int64]bool
	err      error
	reads    map[int64]int
	parses   int
}

func (f *fakeReader) Body(id int64) (string, error) {
	if f.reads != nil {
		f.reads[id]++
	}
	return f.bodies[id], f.err
}

func (f *fakeReader) Original(id int64) (mime.AutomationSignals, bool, bool, error) {
	f.parses++
	if f.rawFails[id] {
		return mime.AutomationSignals{}, false, true, f.err
	}
	return f.signals[id], f.files[id], false, f.err
}

const (
	hour = time.Hour
	day  = 24 * time.Hour
)

func TestDetect(t *testing.T) {
	for _, tt := range []struct {
		name        string
		script      script
		wantKind    string
		wantAnchor  int64 // message ID, 1-based position
		wantLastAsk int64
	}{
		{"question you owe", script{msgs: []msg{{ago: 2 * day, body: "Can you send the contract?"}}}, ReplyOwed, 1, 0},
		{"a request only to your own linked address opens nothing", script{msgs: []msg{{out: true, to: []store.LoopParticipant{ownerAlias}, ago: 4 * day, body: "Can you review it?"}}}, "", 0, 0},
		{"carbon copy owes nothing", script{msgs: []msg{{ago: 2 * day, ccOnly: true, body: "Can you send it?"}}}, "", 0, 0},
		{"thanks does not close your question", script{msgs: []msg{
			{out: true, ago: 5 * day, body: "Can you review it?"},
			{ago: 4 * day, body: "Thanks!"},
		}}, AwaitingReply, 1, 0},
		{"human support reply settles", script{msgs: []msg{
			{out: true, ago: 5 * day, body: "Can you reset my account?", to: []store.LoopParticipant{support}},
			{from: support, ago: 4 * day, body: "Done, you can log in again."},
		}}, "", 0, 0},
		{"no-reply sender settles nothing", script{msgs: []msg{
			{out: true, to: []store.LoopParticipant{noreply}, ago: 5 * day, body: "Can you review it?"},
			{from: noreply, ago: 4 * day, body: "Your ticket was received."},
		}}, AwaitingReply, 1, 0},
		{"auto-submitted reply settles nothing", script{msgs: []msg{
			{out: true, ago: 5 * day, body: "Can you review it?"},
			{ago: 4 * day, body: "I am away until Monday.", signals: mime.AutomationSignals{AutoSubmitted: true}},
		}}, AwaitingReply, 1, 0},
		{"automated reply followed by a statement leaves the request open", script{msgs: []msg{
			{out: true, to: []store.LoopParticipant{alex}, ago: 6 * day, body: "Can you send the contract?"},
			{ago: 5 * day, body: "I am away until Monday.", signals: mime.AutomationSignals{AutoSubmitted: true}},
			{out: true, to: []store.LoopParticipant{alex}, ago: 4 * day, body: "I am still waiting for the contract."},
		}}, AwaitingReply, 1, 0},
		{"unreadable reply followed by a statement leaves the request open", script{msgs: []msg{
			{out: true, to: []store.LoopParticipant{alex}, ago: 6 * day, body: "Can you send the contract?"},
			{ago: 5 * day, body: "I am away until Monday.", rawFails: true},
			{out: true, to: []store.LoopParticipant{alex}, ago: 4 * day, body: "I am still waiting for the contract."},
		}}, AwaitingReply, 1, 0},
		{"list reply settles nothing", script{msgs: []msg{
			{out: true, ago: 5 * day, body: "Can you review it?"},
			{ago: 4 * day, body: "Weekly digest", listID: "news.example.test"},
		}}, AwaitingReply, 1, 0},
		{"bulk reply settles nothing", script{msgs: []msg{
			{out: true, ago: 5 * day, body: "Can you review it?"},
			{ago: 4 * day, body: "Unsubscribe below", signals: mime.AutomationSignals{ListUnsubscribe: true}},
		}}, AwaitingReply, 1, 0},
		{"calendar reply settles nothing", script{msgs: []msg{
			{out: true, ago: 5 * day, body: "Can you review it?"},
			{ago: 4 * day, body: "Alex accepted", signals: mime.AutomationSignals{CalendarPart: true}},
		}}, AwaitingReply, 1, 0},
		{"reply with no text settles nothing", script{msgs: []msg{
			{out: true, ago: 5 * day, body: "Can you review it?"},
			{ago: 4 * day, body: "  "},
		}}, AwaitingReply, 1, 0},
		{"quoted-only reply settles nothing", script{msgs: []msg{
			{out: true, ago: 5 * day, body: "Can you review it?"},
			{ago: 4 * day, body: "On Monday, Me wrote:\n> Can you review it?"},
		}}, AwaitingReply, 1, 0},
		{"attachment-only reply settles", script{msgs: []msg{
			{out: true, ago: 5 * day, body: "Can you send the deck?"},
			{ago: 4 * day, file: true},
		}}, "", 0, 0},
		{"chat message with no text opens nothing", script{chat: true, msgs: []msg{{ago: 2 * day}}}, "", 0, 0},
		{"question and answer, then a new question", script{msgs: []msg{
			{out: true, ago: 6 * day, body: "Can you review it?"},
			{ago: 5 * day, body: "Looks good. Can you send the invoice?"},
		}}, ReplyOwed, 2, 0},
		{"mail to your own linked address settles nothing", script{msgs: []msg{
			{ago: 5 * day, body: "Can you send the contract?"},
			{out: true, to: []store.LoopParticipant{ownerAlias}, ago: 4 * day, body: "Note to self, sent it."},
		}}, ReplyOwed, 1, 0},
		{"your thanks does not clear their question", script{msgs: []msg{
			{ago: 5 * day, body: "Can you send the contract?"},
			{out: true, ago: 4 * day, body: "Thanks!"},
		}}, ReplyOwed, 1, 0},
		{"your attached reply settles even when it only says thanks", script{msgs: []msg{
			{ago: 5 * day, body: "Can you send the contract?"},
			{out: true, ago: 4 * day, body: "Thanks!", file: true},
		}}, "", 0, 0},
		{"your answer settles", script{msgs: []msg{
			{ago: 5 * day, body: "Can you send the contract?"},
			{out: true, ago: 4 * day, body: "Sent, see the attachment."},
		}}, "", 0, 0},
		{"your forward to someone else settles nothing", script{msgs: []msg{
			{ago: 5 * day, body: "Can you send the contract?"},
			{out: true, ago: 4 * day, body: "Passing this along, see below.", to: []store.LoopParticipant{sam}},
		}}, ReplyOwed, 1, 0},
		{"a further exchange with the forward recipient leaves the requester waiting", script{msgs: []msg{
			{ago: 6 * day, body: "Can you send the contract?"},
			{out: true, ago: 5 * day, body: "Forwarding this to you.", to: []store.LoopParticipant{sam}},
			{from: sam, ago: 4 * day, body: "Here is the contract."},
			{out: true, ago: 3 * day, body: "I have the contract now.", to: []store.LoopParticipant{sam}},
		}}, ReplyOwed, 1, 0},
		{"your answer to them and someone else settles", script{msgs: []msg{
			{ago: 5 * day, body: "Can you send the contract?"},
			{out: true, ago: 4 * day, body: "Sent, see the attachment.", to: []store.LoopParticipant{sam, alexAlt}},
		}}, "", 0, 0},
		{"your answer that copies them settles", script{msgs: []msg{
			{ago: 5 * day, body: "Can you send the contract?"},
			{out: true, ccOnly: true, ago: 4 * day, body: "Sent, see the attachment."},
		}}, "", 0, 0},
		{"your answer in a direct chat settles", script{chat: true, msgs: []msg{
			{ago: 5 * day, body: "can you send the contract?"},
			{out: true, ago: 4 * day, body: "sent"},
		}}, "", 0, 0},
		{"your auto-reply settles nothing", script{msgs: []msg{
			{ago: 5 * day, body: "Can you send the contract?"},
			{out: true, ago: 4 * day, body: "I am out until Monday.", signals: mime.AutomationSignals{AutoSubmitted: true}},
		}}, ReplyOwed, 1, 0},
		{"your auto-reply followed by a forward settles nothing", script{msgs: []msg{
			{ago: 5 * day, body: "Can you send the contract?"},
			{out: true, ago: 4 * day, body: "I am out until Monday.", signals: mime.AutomationSignals{AutoSubmitted: true}},
			{out: true, ago: 3 * day, body: "Passing this along, see below.", to: []store.LoopParticipant{sam}},
		}}, ReplyOwed, 1, 0},
		{"nudges keep the oldest anchor", script{msgs: []msg{
			{ago: 6 * day, body: "Can you send the contract?"},
			{ago: 3 * day, body: "Any update?"},
		}}, ReplyOwed, 1, 0},
		{"a question after statements anchors at the question", script{msgs: []msg{
			{ago: 6 * day, body: "Morning."},
			{ago: 5 * day, body: "Can you send the contract?"},
			{ago: 4 * day, body: "Thanks in advance."},
		}}, ReplyOwed, 2, 0},
		{"observed mail does not open a loop", script{msgs: []msg{
			{observe: true, ago: day, body: "Can you send the contract?"},
		}}, "", 0, 0},
		{"group chats are skipped", script{group: true, chat: true, msgs: []msg{{ago: 2 * day, body: "Can you send it?"}}}, "", 0, 0},
		{"direct chat question you owe", script{chat: true, msgs: []msg{{ago: 2 * day, body: "are you coming?"}}}, ReplyOwed, 1, 0},
		{"direct chat question you asked", script{chat: true, msgs: []msg{{out: true, ago: 4 * day, body: "are you coming?"}}}, AwaitingReply, 1, 0},
		{"no loop opened before an unanswered forward", script{msgs: []msg{
			{ago: 6 * day, body: "Here is the contract."},
			{out: true, ago: 4 * day, body: "Can you take a look?", to: []store.LoopParticipant{sam}},
			{ago: 3 * day, body: "Any update?"},
		}}, ReplyOwed, 3, 3},
		{"stale anchor across an unanswered forward", script{msgs: []msg{
			{ago: 6 * day, body: "Can you send the contract?"},
			{out: true, ago: 4 * day, body: "Can you take a look?", to: []store.LoopParticipant{sam}},
			{ago: 3 * day, body: "Any update?"},
		}}, ReplyOwed, 1, 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			loop, ok, err := detect(tt.script)
			require.NoError(err)
			if tt.wantKind == "" {
				assert.False(ok)
				return
			}
			require.True(ok)
			assert.Equal(tt.wantKind, loop.Kind)
			assert.Equal(tt.wantAnchor, loop.Anchor.ID)
			assert.True(loop.OwedSince.Equal(loop.Anchor.OccurredAt))
			if tt.wantLastAsk != 0 {
				assert.Equal(tt.wantLastAsk, loop.LastAsk.ID)
				assert.True(now.Add(-3 * day).Equal(loop.LastAsk.OccurredAt))
				assert.Equal(alex.Address, loop.Counterparty.Address)
			}
		})
	}
}

func TestDetectKeepsAnUnansweredRequestAcrossAnOlderUnrelatedStatement(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	loop, open, err := detect(script{msgs: []msg{
		{from: alex, ago: 6 * day, body: "The meeting moved to Thursday."},
		{out: true, to: []store.LoopParticipant{sam}, ago: 4 * day, body: "Can you send the deck?"},
	}})
	require.NoError(err)
	require.True(open)
	assert.Equal(AwaitingReply, loop.Kind)
	assert.Equal(int64(2), loop.Anchor.ID)
	assert.Equal(sam, loop.Counterparty)
}

func TestDetectStopsAtAnsweredInboundBoundaryBeforeScanningOlderExchanges(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var messages []msg
	for i := range 10 {
		age := time.Duration(22-2*i) * day
		messages = append(messages,
			msg{out: true, ago: age, body: "Can you review the previous draft?"},
			msg{ago: age - day, body: "I reviewed it."},
		)
	}
	messages = append(messages, msg{out: true, ago: 2 * day, body: "Can you send the new draft?"})
	conv, reader := (script{msgs: messages}).conversation()
	reader.reads = map[int64]int{}

	detection, err := Detect(conv, reader)
	require.NoError(err)
	require.True(detection.Open)
	assert.Equal(int64(len(messages)), detection.Loop.Anchor.ID)
	assert.LessOrEqual(reader.parses, 8, "an older inbound answer ends the scan before prior exchanges")
}

func TestDetectKeepsOriginalRequestAgeAcrossAnUnrelatedExchangeBeforeNudge(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	loop, open, err := detect(script{msgs: []msg{
		{out: true, to: []store.LoopParticipant{sam}, ago: 8 * day, body: "Can you send the deck?"},
		{from: alex, ago: 7 * day, body: "Can you review the contract?"},
		{out: true, to: []store.LoopParticipant{alex}, ago: 6 * day, body: "I reviewed the contract."},
		{out: true, to: []store.LoopParticipant{sam}, ago: 3 * day, body: "Any update on the deck?"},
	}})
	require.NoError(err)
	require.True(open)
	assert.Equal(AwaitingReply, loop.Kind)
	assert.Equal(int64(1), loop.Anchor.ID)
	assert.Equal(int64(4), loop.LastAsk.ID)
	assert.Equal(sam, loop.Counterparty)
	assert.True(now.Add(-8 * day).Equal(loop.OwedSince))
}

func TestDetectKeepsUnansweredRequesterAcrossDifferentCounterpartyBoundary(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	loop, open, err := detect(script{msgs: []msg{
		{from: alex, ago: 8 * day, body: "Can you approve the contract?"},
		{out: true, to: []store.LoopParticipant{sam}, ago: 6 * day, body: "Forwarding Alex's request."},
		{from: sam, ago: 5 * day, body: "Could you send the latest version?"},
		{from: alex, ago: 3 * day, body: "Any update?"},
	}})
	require.NoError(err)
	require.True(open)
	assert.Equal(ReplyOwed, loop.Kind)
	assert.Equal(int64(1), loop.Anchor.ID)
	assert.Equal(int64(4), loop.LastAsk.ID)
	assert.Equal(alex, loop.Counterparty)
	assert.True(now.Add(-8 * day).Equal(loop.OwedSince))
}

func TestDetectLoopFields(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	loop, ok, err := detect(script{msgs: []msg{
		{ago: 6 * day, subject: "Contract draft", body: "Can you send the contract?"},
		{out: true, ago: 5 * day, body: "Can you send the contract?", to: []store.LoopParticipant{sam}},
		{from: sam, ago: 4 * day, body: "Here is the contract."},
		{ago: 3 * day, subject: "Contract draft", body: "Any update?"},
	}})
	require.NoError(err)
	require.True(ok)
	assert.Equal(ReplyOwed, loop.Kind)
	assert.Equal(int64(1), loop.Anchor.ID)
	assert.Equal(int64(4), loop.LastAsk.ID)
	assert.Equal(alex, loop.Counterparty)
	assert.True(loop.OwedSince.Equal(now.Add(-6 * day)))
	assert.True(loop.LastActivity.Equal(now.Add(-3 * day)))
	assert.Equal("Contract draft", summary(loop.Anchor))
	assert.Equal("are you coming?", summary(store.LoopMessage{Snippet: "are you coming?"}), "a chat falls back to its snippet")
}

func TestDetectLastAskSkipsQuestionsThatCannotAnchor(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	loop, ok, err := detect(script{msgs: []msg{
		{out: true, ago: 6 * day, body: "Can you review it?"},
		{out: true, ago: 3 * day, body: "Alex and Sam, can you both review it?", to: []store.LoopParticipant{alex, sam}},
	}})
	require.NoError(err)
	require.True(ok)
	assert.Equal(int64(1), loop.Anchor.ID)
	assert.True(loop.LastAsk.OccurredAt.Equal(now.Add(-6*day)), "a question to two people is group handling, so it is not the newest question, got %s", loop.LastAsk.OccurredAt)
}

func TestDetectPropagatesReadErrors(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	conv, reader := script{msgs: []msg{{ago: 2 * day, body: "Can you send it?"}}}.conversation()
	reader.err = errors.New("read failed")
	detection, err := Detect(conv, reader)
	require.ErrorContains(err, "read failed")
	assert.False(detection.Open)
}

func TestDetectCollapsesLinkedAddressesToOnePerson(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	loop, ok, err := detect(script{msgs: []msg{{out: true, ago: 4 * day, body: "Can you review it?", to: []store.LoopParticipant{alex, alexAlt}}}})
	require.NoError(err)
	require.True(ok, "two addresses of one person are one recipient")
	assert.Equal(alex.PersonID, loop.Counterparty.PersonID)

	_, ok, err = detect(script{msgs: []msg{{out: true, ago: 4 * day, body: "Can you both review it?", to: []store.LoopParticipant{alex, sam}}}})
	require.NoError(err)
	assert.False(ok, "two people are group handling")

	loop, ok, err = detect(script{msgs: []msg{{out: true, ago: 4 * day, body: "Can you review it?", to: []store.LoopParticipant{sam, ownerAlias}}}})
	require.NoError(err)
	require.True(ok, "your own addresses on To don't count as recipients")
	assert.Equal(sam.ParticipantID, loop.Counterparty.ParticipantID)
}

func TestDetectParsesOriginalsOnlyWhenTheyCanChangeTheAnswer(t *testing.T) {
	nudges := make([]msg, 0, 26)
	nudges = append(nudges, msg{ago: 30 * day, body: "Can you send the contract?"})
	for i := range 25 {
		nudges = append(nudges, msg{ago: time.Duration(29-i) * hour, body: "Any update?"})
	}
	for _, tt := range []struct {
		name   string
		script script
		parses int
		open   bool
	}{
		{"a statement with nothing before it", script{msgs: []msg{{ago: 2 * day, body: "See you Saturday!"}}}, 0, false},
		{"an acknowledgement", script{msgs: []msg{{ago: day + hour, body: "Thanks!"}}}, 0, false},
		{"a question", script{msgs: []msg{{ago: 2 * day, body: "Can you send the contract?"}}}, 1, true},
		{"a statement your own question may hide behind", script{msgs: []msg{
			{out: true, ago: 5 * day, body: "Can you review it?"},
			{ago: 4 * day, body: "Out of office.", signals: mime.AutomationSignals{AutoSubmitted: true}},
		}}, 2, true},
		{"an answered two-message thread", script{msgs: []msg{
			{out: true, ago: 5 * day, body: "Can you review it?"},
			{ago: 4 * day, body: "Looks good to me."},
		}}, 2, false},
		{"an older asking message on the answering side", script{msgs: []msg{
			{ago: 6 * day, body: "Can you send the contract?"},
			{out: true, ago: 5 * day, body: "On it."},
			{ago: 4 * day, body: "Looks good to me."},
		}}, 2, false},
		{"each body read once", script{msgs: []msg{
			{ago: 3 * day, body: "Can you send the contract?"},
			{ago: 2 * day, body: "And the deck?"},
		}}, 2, true},
		{"25 nudges after the question", script{msgs: nudges}, 26, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			conv, reader := tt.script.conversation()
			reader.reads = map[int64]int{}
			detection, err := Detect(conv, reader)
			require.NoError(t, err)
			assert.Equal(tt.open, detection.Open)
			assert.Equal(tt.parses, reader.parses, "originals parsed")
			if tt.name == "25 nudges after the question" {
				assert.Len(detection.Loop.Questions, 26, "each eligible question can keep the loop open after a partial mark")
			}
			for id, n := range reader.reads {
				assert.Equal(1, n, "message %d body read count", id)
			}
		})
	}
}

func TestDetectCountsAnOriginalThatFailsToParse(t *testing.T) {
	conv, reader := script{msgs: []msg{
		{out: true, ago: 5 * day, body: "Can you review it?"},
		{ago: 4 * day, body: "Looks good to me.", rawFails: true},
	}}.conversation()
	detection, err := Detect(conv, reader)
	require.NoError(t, err)
	assert.True(t, detection.Open, "the unreadable reply settles nothing")
	assert.Equal(t, 1, detection.Unparsed)
}

func TestDetectReplySettlesOnlyItsSender(t *testing.T) {
	for _, samAgo := range []time.Duration{5 * day, 7 * day} {
		t.Run(samAgo.String(), func(t *testing.T) {
			assert := assert.New(t)
			messages := []msg{
				{out: true, ago: 6 * day, body: "Can you send the contract?"},
				{out: true, to: []store.LoopParticipant{sam}, ago: samAgo, body: "Can you send the deck?"},
				{ago: 4 * day, body: "Here is the contract."},
			}
			waiting := int64(2)
			if samAgo > 6*day {
				messages[0], messages[1] = messages[1], messages[0]
				waiting = 1
			}
			loop, ok, err := detect(script{msgs: messages})
			require.NoError(t, err)
			require.True(t, ok)
			assert.Equal(AwaitingReply, loop.Kind)
			assert.Equal(waiting, loop.Anchor.ID)
			assert.Equal(waiting, loop.LastAsk.ID)
			assert.Equal("sam@example.test", loop.Counterparty.Address)
		})
	}
}
