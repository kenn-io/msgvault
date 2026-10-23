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
	out      bool
	observe  bool
	from     store.LoopParticipant
	to       []store.LoopParticipant
	mentions []store.LoopParticipant
	ccOnly   bool
	cc       []store.LoopParticipant
	ago      time.Duration
	body     string
	subject  string
	listID   string
	file     bool
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
			for _, p := range in.mentions {
				m.Recipients = append(m.Recipients, store.LoopRecipient{LoopParticipant: p, Type: "mention"})
			}
			to := in.to
			if to == nil {
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
		for _, p := range in.cc {
			m.Recipients = append(m.Recipients, store.LoopRecipient{LoopParticipant: p, Type: "cc"})
		}
		messages = append(messages, m)
		r.bodies[m.ID], r.signals[m.ID], r.files[m.ID], r.rawFails[m.ID] = in.body, in.signals, in.file, in.rawFails
	}
	slices.Reverse(messages)
	return store.LoopConversation{ConversationID: 7, SourceID: 3, ConversationType: conversationType, GroupChat: s.group, Messages: messages}, r
}

func detect(s script) (Loop, bool, error) {
	conv, reader := s.conversation()
	detection, err := Detect(conv, reader)
	if len(detection.Loops) == 0 {
		return Loop{}, false, err
	}
	return detection.Loops[0], true, err
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

func (f *fakeReader) Evidence(id int64) (store.LoopMessageEvidence, error) {
	if f.reads != nil {
		f.reads[id]++
	}
	f.parses++
	return classify(f.bodies[id], f.signals[id], f.files[id], f.rawFails[id]), f.err
}

const (
	hour = time.Hour
	day  = 24 * time.Hour
)

func TestDetect(t *testing.T) {
	for _, tt := range []struct {
		name             string
		script           script
		wantKind         string
		wantAnchor       int64 // message ID, 1-based position
		wantLastAsk      int64
		wantCounterparty store.LoopParticipant
		wantActivity     time.Duration
		wantSummary      string
	}{
		{"settled To recipient must retain an answer to an unsettled Cc recipient", script{msgs: []msg{
			{from: sam, ago: 8 * day, body: "Can you send the budget?"},
			{out: true, to: []store.LoopParticipant{alex}, cc: []store.LoopParticipant{sam}, ago: 7 * day, body: "Here is the budget."},
			{from: alex, ago: 5 * day, body: "Can you send the contract?"},
			{out: true, ago: 4 * day, body: "Here is the contract."},
		}}, "", 0, 0, store.LoopParticipant{}, 0, ""},
		{"question you owe", script{msgs: []msg{{ago: 2 * day, body: "Can you send the contract?"}}}, ReplyOwed, 1, 0, store.LoopParticipant{}, 0, ""},
		{"a request only to your own linked address opens nothing", script{msgs: []msg{{out: true, to: []store.LoopParticipant{ownerAlias}, ago: 4 * day, body: "Can you review it?"}}}, "", 0, 0, store.LoopParticipant{}, 0, ""},
		{"carbon copy owes nothing", script{msgs: []msg{{ago: 2 * day, ccOnly: true, body: "Can you send it?"}}}, "", 0, 0, store.LoopParticipant{}, 0, ""},
		{"thanks does not close your question", script{msgs: []msg{
			{out: true, ago: 5 * day, body: "Can you review it?"},
			{ago: 4 * day, body: "Thanks!"},
		}}, AwaitingReply, 1, 0, store.LoopParticipant{}, 0, ""},
		{"emoji acknowledgement does not close your question", script{msgs: []msg{
			{out: true, ago: 5 * day, body: "Can you review it?"},
			{from: alex, ago: 4 * day, body: "👍"},
		}}, AwaitingReply, 1, 0, store.LoopParticipant{}, 0, ""},
		{"recipientless outbound keeps a question open", script{msgs: []msg{
			{ago: 5 * day, body: "Can you approve the contract?"},
			{out: true, to: []store.LoopParticipant{}, ago: 3 * day, body: "The report is ready for review."},
		}}, ReplyOwed, 1, 0, store.LoopParticipant{}, 0, ""},
		{"requester acknowledgement keeps the question open", script{msgs: []msg{
			{ago: 5 * day, body: "Can you approve the contract?"},
			{ago: day, body: "Thanks!"},
		}}, ReplyOwed, 1, 0, store.LoopParticipant{}, day, ""},
		{"outbound emoji acknowledgement keeps the question open", script{msgs: []msg{
			{from: alex, ago: 5 * day, body: "Can you send the contract?"},
			{out: true, to: []store.LoopParticipant{alex}, ago: day, body: "🙏"},
		}}, ReplyOwed, 1, 0, store.LoopParticipant{}, day, ""},
		{"outbound acknowledgement keeps the awaited person active", script{msgs: []msg{
			{out: true, ago: 5 * day, body: "Can you review it?"},
			{out: true, to: []store.LoopParticipant{alex}, ago: day, body: "Thanks!"},
		}}, AwaitingReply, 1, 0, alex, day, ""},
		{"automated acknowledgement doesn't refresh a request", script{msgs: []msg{
			{ago: 40 * day, body: "Can you approve the contract?"},
			{ago: day, body: "Thanks!", signals: mime.AutomationSignals{AutoSubmitted: true}},
		}}, ReplyOwed, 1, 0, store.LoopParticipant{}, 40 * day, ""},
		{"unreadable update doesn't refresh a request", script{msgs: []msg{
			{ago: 40 * day, body: "Can you approve the contract?"},
			{ago: day, body: "The ticket was updated.", rawFails: true},
		}}, ReplyOwed, 1, 0, store.LoopParticipant{}, 40 * day, ""},
		{"awaited reply on Cc", script{msgs: []msg{
			{out: true, ago: 5 * day, body: "Can you send the contract?"},
			{from: alex, ago: 3 * day, body: "Here is the contract.", ccOnly: true},
		}}, "", 0, 0, store.LoopParticipant{}, 0, ""},
		{"unrelated Cc reply", script{msgs: []msg{
			{out: true, ago: 5 * day, body: "Can you send the contract?"},
			{from: sam, ago: 3 * day, body: "Here is the contract.", ccOnly: true},
		}}, AwaitingReply, 1, 0, store.LoopParticipant{}, 0, ""},
		{"quoted Cc reply", script{msgs: []msg{
			{out: true, ago: 5 * day, body: "Can you send the contract?"},
			{from: alex, ago: 3 * day, body: "Thanks!\n\nOn Tuesday 20 October 2026\nOwner Example <owner@example.test> wrote:\n> Can you send the contract?", ccOnly: true},
		}}, AwaitingReply, 1, 0, store.LoopParticipant{}, 0, ""},
		{"another person asks on Cc", script{msgs: []msg{
			{out: true, ago: 5 * day, body: "Can you send the contract?"},
			{from: sam, ago: 3 * day, body: "Could you send the report?", ccOnly: true},
		}}, AwaitingReply, 1, 0, store.LoopParticipant{}, 0, ""},
		{"human support reply settles", script{msgs: []msg{
			{out: true, ago: 5 * day, body: "Can you reset my account?", to: []store.LoopParticipant{support}},
			{from: support, ago: 4 * day, body: "Done, you can log in again."},
		}}, "", 0, 0, store.LoopParticipant{}, 0, ""},
		{"no-reply sender settles nothing", script{msgs: []msg{
			{out: true, to: []store.LoopParticipant{noreply}, ago: 5 * day, body: "Can you review it?"},
			{from: noreply, ago: 4 * day, body: "Your ticket was received."},
		}}, AwaitingReply, 1, 0, store.LoopParticipant{}, 0, ""},
		{"auto-submitted reply settles nothing", script{msgs: []msg{
			{out: true, ago: 5 * day, body: "Can you review it?"},
			{ago: 4 * day, body: "I am away until Monday.", signals: mime.AutomationSignals{AutoSubmitted: true}},
		}}, AwaitingReply, 1, 0, store.LoopParticipant{}, 0, ""},
		{"automated reply followed by a statement leaves the request open", script{msgs: []msg{
			{out: true, to: []store.LoopParticipant{alex}, ago: 6 * day, body: "Can you send the contract?"},
			{ago: 5 * day, body: "I am away until Monday.", signals: mime.AutomationSignals{AutoSubmitted: true}},
			{out: true, to: []store.LoopParticipant{alex}, ago: 4 * day, body: "I am still waiting for the contract."},
		}}, AwaitingReply, 1, 0, store.LoopParticipant{}, 0, ""},
		{"unreadable reply followed by a statement leaves the request open", script{msgs: []msg{
			{out: true, to: []store.LoopParticipant{alex}, ago: 6 * day, body: "Can you send the contract?"},
			{ago: 5 * day, body: "I am away until Monday.", rawFails: true},
			{out: true, to: []store.LoopParticipant{alex}, ago: 4 * day, body: "I am still waiting for the contract."},
		}}, AwaitingReply, 1, 0, store.LoopParticipant{}, 0, ""},
		{"list reply settles nothing", script{msgs: []msg{
			{out: true, ago: 5 * day, body: "Can you review it?"},
			{ago: 4 * day, body: "Weekly digest", listID: "news.example.test"},
		}}, AwaitingReply, 1, 0, store.LoopParticipant{}, 0, ""},
		{"bulk reply settles nothing", script{msgs: []msg{
			{out: true, ago: 5 * day, body: "Can you review it?"},
			{ago: 4 * day, body: "Unsubscribe below", signals: mime.AutomationSignals{ListUnsubscribe: true}},
		}}, AwaitingReply, 1, 0, store.LoopParticipant{}, 0, ""},
		{"calendar reply settles nothing", script{msgs: []msg{
			{out: true, ago: 5 * day, body: "Can you review it?"},
			{ago: 4 * day, body: "Alex accepted", signals: mime.AutomationSignals{CalendarPart: true}},
		}}, AwaitingReply, 1, 0, store.LoopParticipant{}, 0, ""},
		{"reply with no text settles nothing", script{msgs: []msg{
			{out: true, ago: 5 * day, body: "Can you review it?"},
			{ago: 4 * day, body: "  "},
		}}, AwaitingReply, 1, 0, store.LoopParticipant{}, 0, ""},
		{"attachment-only reply settles", script{msgs: []msg{
			{out: true, ago: 5 * day, body: "Can you send the deck?"},
			{ago: 4 * day, file: true},
		}}, "", 0, 0, store.LoopParticipant{}, 0, ""},
		{"chat message with no text opens nothing", script{chat: true, msgs: []msg{{ago: 2 * day}}}, "", 0, 0, store.LoopParticipant{}, 0, ""},
		{"question and answer, then a new question", script{msgs: []msg{
			{out: true, ago: 6 * day, body: "Can you review it?"},
			{ago: 5 * day, body: "Looks good. Can you send the invoice?"},
		}}, ReplyOwed, 2, 0, store.LoopParticipant{}, 0, ""},
		{"mail to your own linked address settles nothing", script{msgs: []msg{
			{ago: 5 * day, body: "Can you send the contract?"},
			{out: true, to: []store.LoopParticipant{ownerAlias}, ago: 4 * day, body: "Note to self, sent it."},
		}}, ReplyOwed, 1, 0, store.LoopParticipant{}, 0, ""},
		{"your thanks does not clear their question", script{msgs: []msg{
			{ago: 5 * day, body: "Can you send the contract?"},
			{out: true, ago: 4 * day, body: "Thanks!"},
		}}, ReplyOwed, 1, 0, store.LoopParticipant{}, 0, ""},
		{"your attached reply settles even when it only says thanks", script{msgs: []msg{
			{ago: 5 * day, body: "Can you send the contract?"},
			{out: true, ago: 4 * day, body: "Thanks!", file: true},
		}}, "", 0, 0, store.LoopParticipant{}, 0, ""},
		{"your answer settles", script{msgs: []msg{
			{ago: 5 * day, body: "Can you send the contract?"},
			{out: true, ago: 4 * day, body: "Sent, see the attachment."},
		}}, "", 0, 0, store.LoopParticipant{}, 0, ""},
		{"your forward to someone else settles nothing", script{msgs: []msg{
			{ago: 5 * day, body: "Can you send the contract?"},
			{out: true, ago: 4 * day, body: "Passing this along, see below.", to: []store.LoopParticipant{sam}},
		}}, ReplyOwed, 1, 0, store.LoopParticipant{}, 0, ""},
		{"your answer to them and someone else settles", script{msgs: []msg{
			{ago: 5 * day, body: "Can you send the contract?"},
			{out: true, ago: 4 * day, body: "Sent, see the attachment.", to: []store.LoopParticipant{sam, alexAlt}},
		}}, "", 0, 0, store.LoopParticipant{}, 0, ""},
		{"your answer that copies them settles", script{msgs: []msg{
			{ago: 5 * day, body: "Can you send the contract?"},
			{out: true, ccOnly: true, ago: 4 * day, body: "Sent, see the attachment."},
		}}, "", 0, 0, store.LoopParticipant{}, 0, ""},
		{"your answer in a direct chat settles", script{chat: true, msgs: []msg{
			{ago: 5 * day, body: "can you send the contract?"},
			{out: true, ago: 4 * day, body: "sent"},
		}}, "", 0, 0, store.LoopParticipant{}, 0, ""},
		{"your auto-reply settles nothing", script{msgs: []msg{
			{ago: 5 * day, body: "Can you send the contract?"},
			{out: true, ago: 4 * day, body: "I am out until Monday.", signals: mime.AutomationSignals{AutoSubmitted: true}},
		}}, ReplyOwed, 1, 0, store.LoopParticipant{}, 0, ""},
		{"nudges keep the oldest anchor", script{msgs: []msg{
			{ago: 6 * day, body: "Can you send the contract?"},
			{ago: 3 * day, body: "Any update?"},
		}}, ReplyOwed, 1, 0, store.LoopParticipant{}, 0, ""},
		{"a question after statements anchors at the question", script{msgs: []msg{
			{ago: 6 * day, body: "Morning."},
			{ago: 5 * day, body: "Can you send the contract?"},
			{ago: 4 * day, body: "Thanks in advance."},
		}}, ReplyOwed, 2, 0, store.LoopParticipant{}, 0, ""},
		{"observed mail does not open a loop", script{msgs: []msg{
			{observe: true, ago: day, body: "Can you send the contract?"},
		}}, "", 0, 0, store.LoopParticipant{}, 0, ""},
		{"group chats are skipped", script{group: true, chat: true, msgs: []msg{{ago: 2 * day, body: "Can you send it?"}}}, "", 0, 0, store.LoopParticipant{}, 0, ""},
		{"direct chat question you owe", script{chat: true, msgs: []msg{{ago: 2 * day, body: "are you coming?"}}}, ReplyOwed, 1, 0, store.LoopParticipant{}, 0, ""},
		{"direct chat question you asked", script{chat: true, msgs: []msg{{out: true, ago: 4 * day, body: "are you coming?"}}}, AwaitingReply, 1, 0, store.LoopParticipant{}, 0, ""},
		{"direct chat question waits on the recipient, not a mentioned person", script{chat: true, msgs: []msg{{out: true, ago: 4 * day, body: "Can you review this?", to: []store.LoopParticipant{alex}, mentions: []store.LoopParticipant{sam}}}}, AwaitingReply, 1, 0, alex, 0, ""},

		{"stale anchor across an unanswered forward", script{msgs: []msg{
			{ago: 6 * day, subject: "Contract draft", body: "Can you send the contract?"},
			{out: true, ago: 4 * day, body: "Can you take a look?", to: []store.LoopParticipant{sam}},
			{ago: 3 * day, body: "Any update?"},
		}}, ReplyOwed, 1, 3, alex, 3 * day, "Contract draft"},
		{"an older unrelated statement leaves a request open", script{msgs: []msg{
			{from: alex, ago: 6 * day, body: "The meeting moved to Thursday."},
			{out: true, to: []store.LoopParticipant{sam}, ago: 4 * day, body: "Can you send the deck?"},
		}}, AwaitingReply, 2, 0, sam, 0, ""},
		{"an unrelated exchange before a nudge preserves the request age", script{msgs: []msg{
			{out: true, to: []store.LoopParticipant{sam}, ago: 8 * day, body: "Can you send the deck?"},
			{from: alex, ago: 7 * day, body: "Can you review the contract?"},
			{out: true, to: []store.LoopParticipant{alex}, ago: 6 * day, body: "I reviewed the contract."},
			{out: true, to: []store.LoopParticipant{sam}, ago: 3 * day, body: "Any update on the deck?"},
		}}, AwaitingReply, 1, 4, sam, 0, ""},
		{"another counterparty boundary leaves the requester waiting", script{msgs: []msg{
			{from: alex, ago: 8 * day, body: "Can you approve the contract?"},
			{out: true, to: []store.LoopParticipant{sam}, ago: 6 * day, body: "Forwarding Alex's request."},
			{from: sam, ago: 5 * day, body: "Could you send the latest version?"},
			{from: alex, ago: 3 * day, body: "Any update?"},
		}}, ReplyOwed, 1, 4, alex, 0, ""},
		{"group handling cannot become the newest question", script{msgs: []msg{
			{out: true, ago: 6 * day, body: "Can you review it?"},
			{out: true, ago: 3 * day, body: "Alex and Sam, can you both review it?", to: []store.LoopParticipant{alex, sam}},
		}}, AwaitingReply, 1, 1, alex, 0, ""},
		{"reply settles only its sender after another request", script{msgs: []msg{{out: true, ago: 6 * day, body: "Can you send the contract?"}, {out: true, to: []store.LoopParticipant{sam}, ago: 5 * day, body: "Can you send the deck?"}, {ago: 4 * day, body: "Here is the contract."}}}, AwaitingReply, 2, 2, sam, 0, ""},
		{"reply settles only its sender before another request", script{msgs: []msg{{out: true, to: []store.LoopParticipant{sam}, ago: 7 * day, body: "Can you send the deck?"}, {out: true, ago: 6 * day, body: "Can you send the contract?"}, {ago: 4 * day, body: "Here is the contract."}}}, AwaitingReply, 1, 1, sam, 0, ""},
		{"linked addresses are one recipient", script{msgs: []msg{{out: true, ago: 4 * day, body: "Can you review it?", to: []store.LoopParticipant{alex, alexAlt}}}}, AwaitingReply, 1, 0, alex, 0, ""},
		{"several people are group handling", script{msgs: []msg{{out: true, ago: 4 * day, body: "Can you both review it?", to: []store.LoopParticipant{alex, sam}}}}, "", 0, 0, store.LoopParticipant{}, 0, ""},
		{"owner aliases do not add a recipient", script{msgs: []msg{{out: true, ago: 4 * day, body: "Can you review it?", to: []store.LoopParticipant{sam, ownerAlias}}}}, AwaitingReply, 1, 0, sam, 0, ""},
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
			assert.True(now.Add(-tt.script.msgs[tt.wantAnchor-1].ago).Equal(loop.OwedSince))
			if tt.wantCounterparty.ParticipantID != 0 {
				assert.Equal(tt.wantCounterparty, loop.Counterparty)
			}
			if tt.wantActivity != 0 {
				assert.True(now.Add(-tt.wantActivity).Equal(loop.LastActivity))
			}
			if tt.wantSummary != "" {
				assert.Equal(tt.wantSummary, summary(loop.Anchor))
			}
			assert.True(loop.OwedSince.Equal(loop.Anchor.OccurredAt))
			if tt.wantLastAsk != 0 {
				assert.Equal(tt.wantLastAsk, loop.Questions[len(loop.Questions)-1].ID)
				assert.True(now.Add(-tt.script.msgs[tt.wantLastAsk-1].ago).Equal(loop.Questions[len(loop.Questions)-1].OccurredAt))
			}
		})
	}
}

func TestSummaryUsesSnippetWhenSubjectIsMissing(t *testing.T) {
	assert := assert.New(t)
	assert.Equal("are you coming?", summary(store.LoopMessage{Snippet: "are you coming?"}))
}

func TestDetectPropagatesReadErrors(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	conv, reader := script{msgs: []msg{{ago: 2 * day, body: "Can you send it?"}}}.conversation()
	reader.err = errors.New("read failed")
	detection, err := Detect(conv, reader)
	require.ErrorContains(err, "read failed")
	assert.Empty(detection.Loops)
}

func TestDetectParsesOriginalsOnlyWhenTheyCanChangeTheAnswer(t *testing.T) {
	nudges := make([]msg, 0, 26)
	nudges = append(nudges, msg{ago: 30 * day, body: "Can you send the contract?"})
	for i := range 25 {
		nudges = append(nudges, msg{ago: time.Duration(29-i) * hour, body: "Any update?"})
	}
	var answered []msg
	for i := range 10 {
		age := time.Duration(22-2*i) * day
		answered = append(answered, msg{out: true, ago: age, body: "Can you review the previous draft?"}, msg{ago: age - day, body: "I reviewed it."})
	}
	answered = append(answered, msg{out: true, ago: 2 * day, body: "Can you send the new draft?"})
	for _, tt := range []struct {
		name      string
		script    script
		parses    int
		open      bool
		questions int
		unparsed  int
		maxParses int
		anchor    int64
	}{
		{"a statement with nothing before it", script{msgs: []msg{{ago: 2 * day, body: "See you Saturday!"}}}, 1, false, 0, 0, 0, 0},
		{"an acknowledgement", script{msgs: []msg{{ago: day + hour, body: "Thanks!"}}}, 1, false, 0, 0, 0, 0},
		{"a question", script{msgs: []msg{{ago: 2 * day, body: "Can you send the contract?"}}}, 1, true, 0, 0, 0, 0},
		{"a statement your own question may hide behind", script{msgs: []msg{
			{out: true, ago: 5 * day, body: "Can you review it?"},
			{ago: 4 * day, body: "Out of office.", signals: mime.AutomationSignals{AutoSubmitted: true}},
		}}, 2, true, 0, 0, 0, 0},
		{"an answered two-message thread", script{msgs: []msg{
			{out: true, ago: 5 * day, body: "Can you review it?"},
			{ago: 4 * day, body: "Looks good to me."},
		}}, 2, false, 0, 0, 0, 0},
		{"an older asking message on the answering side", script{msgs: []msg{
			{ago: 6 * day, body: "Can you send the contract?"},
			{out: true, ago: 5 * day, body: "On it."},
			{ago: 4 * day, body: "Looks good to me."},
		}}, 2, false, 0, 0, 0, 0},
		{"25 nudges after the question", script{msgs: nudges}, 26, true, 26, 0, 0, 0},
		{"unreadable reply settles nothing", script{msgs: []msg{{out: true, ago: 5 * day, body: "Can you review it?"}, {ago: 4 * day, body: "Looks good to me.", rawFails: true}}}, 2, true, 0, 1, 0, 0},
		{"answered boundary skips older exchanges", script{msgs: answered}, 0, true, 0, 0, 8, int64(len(answered))},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			conv, reader := tt.script.conversation()
			reader.reads = map[int64]int{}
			detection, err := Detect(conv, reader)
			require.NoError(t, err)
			assert.Equal(tt.open, len(detection.Loops) > 0)
			if tt.maxParses > 0 {
				assert.LessOrEqual(reader.parses, tt.maxParses, "an older inbound answer ends the scan before prior exchanges")
			} else {
				assert.Equal(tt.parses, reader.parses, "originals parsed")
			}
			if tt.anchor != 0 {
				require.NotEmpty(t, detection.Loops)
				assert.Equal(tt.anchor, detection.Loops[0].Anchor.ID)
			}
			assert.Equal(tt.unparsed, detection.Unparsed)
			if tt.questions > 0 {
				assert.Len(detection.Loops[0].Questions, tt.questions, "each eligible question can keep the loop open after a partial mark")
			}
			for id, n := range reader.reads {
				assert.Equal(1, n, "message %d body read count", id)
			}
		})
	}
}
