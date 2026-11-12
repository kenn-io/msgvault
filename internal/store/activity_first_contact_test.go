package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClassifyFirstContact(t *testing.T) {
	target, sender, ownerPerson := int64(10), int64(11), int64(13)
	owner := ActivityCounterpart{ParticipantID: 1, PersonID: &ownerPerson, RecipientType: "to", IsOwner: true}
	from := ActivityCounterpart{ParticipantID: 2, PersonID: &sender, RecipientType: "from"}
	addressed := ActivityCounterpart{ParticipantID: 3, PersonID: &target, RecipientType: "to"}
	email := ActivityCandidate{ConversationType: "email_thread", Counterparts: []ActivityCounterpart{owner, from, addressed}}
	tests := []struct {
		name       string
		candidate  ActivityCandidate
		want       FirstContactKind
		introducer int64
	}{
		{"introduction", email, FirstContactEmailIntro, 2},
		{"meeting", ActivityCandidate{MessageType: "calendar_event", Counterparts: email.Counterparts}, FirstContactMeeting, 0},
		{"owner absent", ActivityCandidate{ConversationType: "group_chat", Counterparts: []ActivityCounterpart{from, addressed}}, FirstContactObserved, 0},
		{"group", ActivityCandidate{ConversationType: "group_chat", Counterparts: email.Counterparts}, FirstContactGroupShared, 0},
		{"inbound sender", ActivityCandidate{ConversationType: "email_thread", Counterparts: []ActivityCounterpart{owner, {ParticipantID: 3, PersonID: &target, RecipientType: "from"}}}, FirstContactInboundOutreach, 0},
		{"owner sender", ActivityCandidate{ConversationType: "direct_chat", Counterparts: []ActivityCounterpart{{ParticipantID: 1, IsOwner: true, RecipientType: "from"}, addressed}}, FirstContactOwnerOutreach, 1},
		{"source flag without sender", ActivityCandidate{ConversationType: "email_thread", SourceIsFromMe: true, Counterparts: []ActivityCounterpart{addressed}}, FirstContactOwnerOutreach, 0},
		{"unknown chat", ActivityCandidate{ConversationType: "chat", SourceIsFromMe: true, Counterparts: []ActivityCounterpart{addressed}}, FirstContactObserved, 0},
		{"bcc target", ActivityCandidate{ConversationType: "email_thread", Counterparts: []ActivityCounterpart{owner, from, {ParticipantID: 3, PersonID: &target, RecipientType: "bcc"}}}, FirstContactObserved, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kind, intro := ClassifyFirstContact(tc.candidate, target, 25)
			assert.Equal(t, tc.want, kind)
			if tc.introducer == 0 {
				assert.Nil(t, intro)
			} else if assert.NotNil(t, intro) {
				assert.Equal(t, tc.introducer, intro.ParticipantID)
			}
		})
	}
	for _, n := range []int{4, 5} {
		t.Run(string(rune('0'+n))+" other recipients", func(t *testing.T) {
			c := email
			c.Counterparts = append([]ActivityCounterpart(nil), email.Counterparts...)
			for i := 0; i < n; i++ {
				id := int64(100 + i)
				c.Counterparts = append(c.Counterparts, ActivityCounterpart{ParticipantID: id, PersonID: &id, RecipientType: "cc"})
			}
			alias := int64(100)
			c.Counterparts = append(c.Counterparts, ActivityCounterpart{ParticipantID: 999, PersonID: &alias, RecipientType: "bcc"})
			kind, _ := ClassifyFirstContact(c, target, 25)
			want := FirstContactEmailIntro
			if n == 5 {
				want = FirstContactObserved
			}
			assert.Equal(t, want, kind)
		})
	}
}

func TestClassifyFirstContactBoundaries(t *testing.T) {
	target, owner, alias := int64(10), int64(11), int64(12)
	targetRecipient := ActivityCounterpart{ParticipantID: 10, PersonID: &target, RecipientType: "cc"}
	ownerRecipient := ActivityCounterpart{ParticipantID: 11, PersonID: &owner, RecipientType: "to", IsOwner: true}
	sender := ActivityCounterpart{ParticipantID: 12, RecipientType: "from"}
	cases := []struct {
		name      string
		candidate ActivityCandidate
		max       int
		kind      FirstContactKind
		intro     int64
	}{
		{"unsaved sender", ActivityCandidate{ConversationType: "email_thread", Counterparts: []ActivityCounterpart{sender, ownerRecipient, targetRecipient}}, 25, FirstContactEmailIntro, 12},
		{"owner alias recipient", ActivityCandidate{ConversationType: "email_thread", Counterparts: []ActivityCounterpart{sender, ownerRecipient, {ParticipantID: 13, PersonID: &owner, RecipientType: "bcc"}, targetRecipient}}, 25, FirstContactEmailIntro, 12},
		{"broadcast inbound", ActivityCandidate{ConversationType: "email_thread", Counterparts: []ActivityCounterpart{ownerRecipient, {ParticipantID: 10, PersonID: &target, RecipientType: "from"}, {ParticipantID: 14, PersonID: &alias, RecipientType: "to"}}}, 1, FirstContactObserved, 0},
		{"broadcast outbound", ActivityCandidate{ConversationType: "email_thread", SourceIsFromMe: true, Counterparts: []ActivityCounterpart{targetRecipient, {ParticipantID: 14, PersonID: &alias, RecipientType: "to"}}}, 1, FirstContactObserved, 0},
		{"direct chat member", ActivityCandidate{ConversationType: "direct_chat", SourceIsFromMe: true, Counterparts: []ActivityCounterpart{{ParticipantID: 10, PersonID: &target, RecipientType: "member"}}}, 25, FirstContactOwnerOutreach, 0},
		{"channel inbound", ActivityCandidate{ConversationType: "channel", Counterparts: []ActivityCounterpart{sender, ownerRecipient, targetRecipient}}, 25, FirstContactGroupShared, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, intro := ClassifyFirstContact(tc.candidate, target, tc.max)
			assert.Equal(t, tc.kind, kind)
			if tc.intro == 0 {
				assert.Nil(t, intro)
			} else if assert.NotNil(t, intro) {
				assert.Equal(t, tc.intro, intro.ParticipantID)
				assert.Nil(t, intro.PersonID)
			}
		})
	}
}
