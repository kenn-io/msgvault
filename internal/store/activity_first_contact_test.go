package store

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassifyFirstContact(t *testing.T) {
	target, sender, ownerPerson := int64(10), int64(11), int64(13)
	owner := ActivityCounterpart{ParticipantID: 1, PersonID: &ownerPerson, RecipientType: "to", IsOwner: true}
	from := ActivityCounterpart{ParticipantID: 2, PersonID: &sender, RecipientType: "from"}
	addressed := ActivityCounterpart{ParticipantID: 3, PersonID: &target, RecipientType: "to"}
	email := ActivityCandidate{ConversationType: "email_thread", Counterparts: []ActivityCounterpart{owner, from, addressed}}
	ownerID, alias := int64(11), int64(12)
	targetRecipient := ActivityCounterpart{ParticipantID: 10, PersonID: &target, RecipientType: "cc"}
	ownerRecipient := ActivityCounterpart{ParticipantID: 11, PersonID: &ownerID, RecipientType: "to", IsOwner: true}
	unsavedSender := ActivityCounterpart{ParticipantID: 12, RecipientType: "from"}
	proof := []ActivityCounterpart{{ParticipantID: 1, PersonID: &sender, RecipientType: "from"}, {ParticipantID: 2, PersonID: &target, RecipientType: "to"}, {ParticipantID: 3, PersonID: &ownerPerson, RecipientType: "to", IsOwner: true}}
	type testCase struct {
		name       string
		candidate  ActivityCandidate
		want       FirstContactKind
		introducer int64
		person     *int64
		max        int
	}
	tests := []testCase{
		{"introduction", email, FirstContactEmailIntro, 2, &sender, 25},
		{"ambiguous from", ActivityCandidate{ConversationType: "email_thread", Counterparts: []ActivityCounterpart{owner, from, {ParticipantID: 4, RecipientType: "from"}, addressed}}, FirstContactObserved, 0, nil, 25},
		{"ambiguous target sender", ActivityCandidate{ConversationType: "email_thread", Counterparts: []ActivityCounterpart{owner, from, {ParticipantID: 4, PersonID: &target, RecipientType: "from"}}}, FirstContactObserved, 0, nil, 25},
		{"ambiguous owner sender", ActivityCandidate{ConversationType: "email_thread", SourceIsFromMe: true, Counterparts: []ActivityCounterpart{{ParticipantID: 1, IsOwner: true, RecipientType: "from"}, from, addressed}}, FirstContactOwnerOutreach, 0, nil, 25},
		{"recorded sender absent from envelope", ActivityCandidate{ConversationType: "email_thread", SenderID: new(int64(99)), Counterparts: email.Counterparts}, FirstContactObserved, 0, nil, 25},
		{"meeting", ActivityCandidate{MessageType: "calendar_event", Counterparts: email.Counterparts}, FirstContactMeeting, 0, nil, 25},
		{"owner absent", ActivityCandidate{ConversationType: "group_chat", Counterparts: []ActivityCounterpart{from, addressed}}, FirstContactObserved, 0, nil, 25},
		{"group", ActivityCandidate{ConversationType: "group_chat", Counterparts: email.Counterparts}, FirstContactGroupShared, 0, nil, 25},
		{"inbound sender", ActivityCandidate{ConversationType: "email_thread", Counterparts: []ActivityCounterpart{owner, {ParticipantID: 3, PersonID: &target, RecipientType: "from"}}}, FirstContactInboundOutreach, 0, nil, 25},
		{"owner sender", ActivityCandidate{ConversationType: "direct_chat", Counterparts: []ActivityCounterpart{{ParticipantID: 1, IsOwner: true, RecipientType: "from"}, addressed}}, FirstContactOwnerOutreach, 1, nil, 25},
		{"source flag without sender", ActivityCandidate{ConversationType: "email_thread", SourceIsFromMe: true, Counterparts: []ActivityCounterpart{addressed}}, FirstContactOwnerOutreach, 0, nil, 25},
		{"unknown chat", ActivityCandidate{ConversationType: "chat", SourceIsFromMe: true, Counterparts: []ActivityCounterpart{addressed}}, FirstContactObserved, 0, nil, 25},
		{"bcc target", ActivityCandidate{ConversationType: "email_thread", Counterparts: []ActivityCounterpart{owner, from, {ParticipantID: 3, PersonID: &target, RecipientType: "bcc"}}}, FirstContactObserved, 0, nil, 25},
		{"unsaved sender", ActivityCandidate{ConversationType: "email_thread", Counterparts: []ActivityCounterpart{unsavedSender, ownerRecipient, targetRecipient}}, FirstContactEmailIntro, 12, nil, 25},
		{"owner alias recipient", ActivityCandidate{ConversationType: "email_thread", Counterparts: []ActivityCounterpart{unsavedSender, ownerRecipient, {ParticipantID: 13, PersonID: &ownerID, RecipientType: "bcc"}, targetRecipient}}, FirstContactEmailIntro, 12, nil, 25},
		{"broadcast inbound", ActivityCandidate{ConversationType: "email_thread", Counterparts: []ActivityCounterpart{ownerRecipient, {ParticipantID: 10, PersonID: &target, RecipientType: "from"}, {ParticipantID: 14, PersonID: &alias, RecipientType: "to"}}}, FirstContactObserved, 0, nil, 1},
		{"broadcast outbound", ActivityCandidate{ConversationType: "email_thread", SourceIsFromMe: true, Counterparts: []ActivityCounterpart{targetRecipient, {ParticipantID: 14, PersonID: &alias, RecipientType: "to"}}}, FirstContactObserved, 0, nil, 1},
		{"direct chat member", ActivityCandidate{ConversationType: "direct_chat", SourceIsFromMe: true, Counterparts: []ActivityCounterpart{{ParticipantID: 10, PersonID: &target, RecipientType: "member"}}}, FirstContactOwnerOutreach, 0, nil, 25},
		{"channel inbound", ActivityCandidate{ConversationType: "channel", Counterparts: []ActivityCounterpart{unsavedSender, ownerRecipient, targetRecipient}}, FirstContactGroupShared, 0, nil, 25},
		{"meeting explicit", ActivityCandidate{MessageType: "meeting_transcript", MeetingParticipants: proof, Counterparts: proof}, FirstContactMeetingIntro, 1, &sender, 25},
		{"meeting missing owner", ActivityCandidate{MessageType: "meeting_transcript", MeetingParticipants: proof[:2], Counterparts: proof}, FirstContactMeeting, 0, nil, 25},
		{"meeting normalized only", ActivityCandidate{MessageType: "meeting_transcript", Counterparts: proof}, FirstContactMeeting, 0, nil, 25},
		{"meeting ambiguous organizer", ActivityCandidate{MessageType: "meeting_transcript", MeetingParticipants: append(append([]ActivityCounterpart{}, proof...), ActivityCounterpart{ParticipantID: 4, RecipientType: "from"}), Counterparts: proof}, FirstContactMeeting, 0, nil, 25},
		{"meeting target organizer", ActivityCandidate{MessageType: "meeting_transcript", MeetingParticipants: []ActivityCounterpart{{ParticipantID: 2, PersonID: &target, RecipientType: "from"}, proof[2]}, Counterparts: proof}, FirstContactMeeting, 0, nil, 25},
		{"meeting owner organizer", ActivityCandidate{MessageType: "meeting_transcript", MeetingParticipants: []ActivityCounterpart{{ParticipantID: 3, PersonID: &ownerPerson, RecipientType: "from", IsOwner: true}, proof[1]}, Counterparts: proof}, FirstContactMeeting, 0, nil, 25},
	}
	for _, n := range []int{maxIntroCounterparts, maxIntroCounterparts + 1} {
		c := email
		c.Counterparts = append([]ActivityCounterpart(nil), email.Counterparts...)
		for i := range n {
			id := int64(100 + i)
			c.Counterparts = append(c.Counterparts, ActivityCounterpart{ParticipantID: id, PersonID: &id, RecipientType: "cc"})
		}
		alias := int64(100)
		c.Counterparts = append(c.Counterparts, ActivityCounterpart{ParticipantID: 999, PersonID: &alias, RecipientType: "bcc"})
		want, intro, person := FirstContactEmailIntro, int64(2), &sender
		if n > maxIntroCounterparts {
			want, intro, person = FirstContactObserved, 0, nil
		}
		tests = append(tests, testCase{fmt.Sprintf("%d other recipients", n), c, want, intro, person, 25})
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			kind, intro := ClassifyFirstContact(tc.candidate, target, tc.max)
			assert.Equal(tc.want, kind)
			if tc.introducer == 0 {
				assert.Nil(intro)
			} else if assert.NotNil(intro) {
				assert.Equal(tc.introducer, intro.ParticipantID)
				if tc.person == nil {
					assert.Nil(intro.PersonID)
				} else {
					require.NotNil(t, intro.PersonID)
					assert.Equal(*tc.person, *intro.PersonID)
				}
			}
		})
	}
}
