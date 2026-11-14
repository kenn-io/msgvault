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
	ownerID := int64(11)
	targetRecipient := ActivityCounterpart{ParticipantID: 10, PersonID: &target, RecipientType: "cc"}
	ownerRecipient := ActivityCounterpart{ParticipantID: 11, PersonID: &ownerID, RecipientType: "to", IsOwner: true}
	unsavedSender := ActivityCounterpart{ParticipantID: 12, RecipientType: "from"}
	type testCase struct {
		name             string
		candidate        ActivityCandidate
		envelopeSenderID int64
		want             FirstContactKind
		introducer       int64
		person           *int64
	}
	tests := []testCase{
		{"introduction", email, 2, FirstContactEmailIntro, 2, &sender},
		{"group", ActivityCandidate{ConversationType: "group_chat", Counterparts: email.Counterparts}, 2, FirstContactObserved, 0, nil},
		{"inbound sender", ActivityCandidate{ConversationType: "email_thread", Counterparts: []ActivityCounterpart{owner, {ParticipantID: 3, PersonID: &target, RecipientType: "from"}, addressed}}, 3, FirstContactObserved, 0, nil},
		{"outbound email", ActivityCandidate{ConversationType: "email_thread", SourceIsFromMe: true, Counterparts: email.Counterparts}, 2, FirstContactObserved, 0, nil},
		{"bcc target", ActivityCandidate{ConversationType: "email_thread", Counterparts: []ActivityCounterpart{owner, from, {ParticipantID: 3, PersonID: &target, RecipientType: "bcc"}}}, 2, FirstContactObserved, 0, nil},
		{"unsaved sender", ActivityCandidate{ConversationType: "email_thread", Counterparts: []ActivityCounterpart{unsavedSender, ownerRecipient, targetRecipient}}, 12, FirstContactEmailIntro, 12, nil},
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
		tests = append(tests, testCase{fmt.Sprintf("%d other recipients", n), c, 2, want, intro, person})
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			kind, intro := ClassifyFirstContact(tc.candidate, target, tc.envelopeSenderID)
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
